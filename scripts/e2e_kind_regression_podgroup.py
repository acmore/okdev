"""Read controlled PodGroup evidence through the real Kind API, without a scheduler."""
import json
import unittest

from e2e_kind_support import KindRegression


class PodGroupStatus(KindRegression):
    def apply(self, value):
        return self.run_cmd(["kubectl", "apply", "-f", "-"], data=json.dumps(value).encode())

    def remove_crd(self):
        if self.created_crd:
            self.run_cmd(["kubectl", "delete", "crd", "podgroups.scheduling.volcano.sh",
                          "--ignore-not-found", "--wait=true", "--timeout=60s"])
            self.created_crd = False

    def test_evidence_optional_permissions_and_absent_crd(self):
        self.created_crd = False
        present = self.run_cmd(["kubectl", "get", "crd", "podgroups.scheduling.volcano.sh",
                                "--ignore-not-found", "-o", "name"]).stdout
        self.addCleanup(self.remove_crd)
        if not present:
            self.apply({"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
                        "metadata": {"name": "podgroups.scheduling.volcano.sh"},
                        "spec": {"group": "scheduling.volcano.sh", "scope": "Namespaced",
                                 "names": {"plural": "podgroups", "singular": "podgroup", "kind": "PodGroup"},
                                 "versions": [{"name": "v1beta1", "served": True, "storage": True,
                                               "subresources": {"status": {}},
                                               "schema": {"openAPIV3Schema": {"type": "object", "properties": {
                                                   "spec": {"type": "object", "x-kubernetes-preserve-unknown-fields": True},
                                                   "status": {"type": "object", "x-kubernetes-preserve-unknown-fields": True}}}}}]}})
            self.created_crd = True
            self.run_cmd(["kubectl", "wait", "--for=condition=Established", "crd/podgroups.scheduling.volcano.sh", "--timeout=60s"])
        config, _ = self.config("pg-status", replicas=2)
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"] = []
        config.write_text(json.dumps(raw))
        self.up(config, "pg-status")

        def report():
            return json.loads(self.run_cmd(self.base(config, "pg-status") + ["status", "--details", "--output", "json"]).stdout)

        self.assertNotIn("podGroups", report())
        pods = self.pods("pg-status")
        name = "diagnostic-group"
        self.apply({"apiVersion": "scheduling.volcano.sh/v1beta1", "kind": "PodGroup",
                    "metadata": {"name": name, "namespace": self.namespace}, "spec": {"minMember": 2, "queue": "research"}})
        status = {"status": {"phase": "Pending", "conditions": [{"type": "Unschedulable", "status": "True",
                  "reason": "QueueQuotaExceeded", "message": "queue research has insufficient GPU quota",
                  "lastTransitionTime": "2026-01-01T00:00:00Z"}]}}
        self.run_cmd(["kubectl", "-n", self.namespace, "patch", "podgroups.scheduling.volcano.sh", name,
                      "--subresource=status", "--type=merge", "-p", json.dumps(status)])
        pg = json.loads(self.run_cmd(["kubectl", "-n", self.namespace, "get", "podgroups.scheduling.volcano.sh", name, "-o", "json"]).stdout)
        uid = pg["metadata"]["uid"]
        for index, pod in enumerate(pods):
            key = "scheduling.volcano.sh/group-name" if index == 0 else "scheduling.k8s.io/group-name"
            self.run_cmd(["kubectl", "-n", self.namespace, "annotate", "pod", pod["metadata"]["name"], f"{key}={name}"])
        for suffix, kind, event_uid, reason, message in [
            ("quota", "PodGroup", uid, "QueueQuotaExceeded", "queue research GPU quota exhausted"),
            ("node", "PodGroup", uid, "NodeFitFailed", "node affinity did not match"),
            ("old", "PodGroup", "old-incarnation", "StaleQuota", "ignore old PodGroup"),
            ("pod", "Pod", uid, "UnrelatedPod", "ignore same-name Pod"),
        ]:
            self.apply({"apiVersion": "v1", "kind": "Event", "metadata": {"name": "pg-" + suffix, "namespace": self.namespace},
                        "involvedObject": {"apiVersion": "scheduling.volcano.sh/v1beta1" if kind == "PodGroup" else "v1",
                                           "kind": kind, "name": name, "namespace": self.namespace, "uid": event_uid},
                        "reason": reason, "message": message, "type": "Warning", "count": 1,
                        "source": {"component": "okdev-regression-fixture"}})
        groups = report()["podGroups"]
        self.assertEqual(len(groups), 1)
        self.assertEqual(groups[0]["pods"], sorted(pod["metadata"]["name"] for pod in pods))
        self.assertEqual(groups[0]["queue"], "research")
        self.assertEqual(groups[0]["conditions"][0]["message"], status["status"]["conditions"][0]["message"])
        self.assertEqual({event["reason"] for event in groups[0]["events"]}, {"QueueQuotaExceeded", "NodeFitFailed"})
        text = self.run_cmd(self.base(config, "pg-status") + ["status", "--details"]).stdout.decode()
        self.assertIn("queue=research phase=Pending minMember=2", text)
        self.assertIn("queue research GPU quota exhausted", text)
        self.assertIn("node affinity did not match", text)
        self.assertNotIn("ignore old PodGroup", text)

        role = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"name": "status-reader", "namespace": self.namespace},
                "rules": [{"apiGroups": ["*"], "resources": ["pods", "events", "deployments", "replicasets", "statefulsets", "jobs", "pytorchjobs", "services", "configmaps", "persistentvolumeclaims"], "verbs": ["get", "list", "watch"]}]}
        self.apply(role)
        self.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": {"name": "status-reader", "namespace": self.namespace},
                    "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "status-reader"},
                    "subjects": [{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": "okdev-regression-reader"}]})
        kube = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        kube["users"][0]["user"]["as"] = "okdev-regression-reader"
        restricted = self.root / "restricted-kubeconfig"
        restricted.write_text(json.dumps(kube))

        def restricted_report():
            original = self.env["KUBECONFIG"]
            try:
                self.env["KUBECONFIG"] = str(restricted)
                return report()
            finally:
                self.env["KUBECONFIG"] = original

        self.assertIn("forbidden", restricted_report()["podGroups"][0]["lookupError"].lower())
        role["rules"][0]["resources"].remove("events")
        role["rules"][0]["resources"].append("podgroups")
        self.apply(role)
        partial = restricted_report()["podGroups"][0]
        self.assertEqual(partial["queue"], "research")
        self.assertIn("forbidden", partial["eventsError"].lower())
        if self.created_crd:
            self.remove_crd()
            self.assertIn("not find", report()["podGroups"][0]["lookupError"].lower())


if __name__ == "__main__":
    unittest.main()
