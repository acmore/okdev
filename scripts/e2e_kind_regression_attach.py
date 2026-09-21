"""Access external StatefulSet pods without adopting or reconciling them."""
import hashlib
import json
import re
import unittest

from e2e_kind_support import KindRegression


class AttachOnly(KindRegression):
    def test_external_access_and_lifecycle_boundary(self):
        manifest = {"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": {"name": "external"},
                    "spec": {"replicas": 2, "serviceName": "external", "selector": {"matchLabels": {"app": "external"}},
                             "template": {"metadata": {"labels": {"app": "external"}}, "spec": {"containers": [
                                 {"name": name, "image": "ubuntu:22.04", "command": ["sleep", "infinity"]}
                                 for name in ("dev", "other")]}}}}
        self.run_cmd(["kubectl", "-n", self.namespace, "apply", "-f", "-"], data=json.dumps(manifest).encode())
        self.run_cmd(["kubectl", "-n", self.namespace, "rollout", "status", "statefulset/external", "--timeout=120s"])
        before = self.snapshot()
        config = self.root / "attach.yaml"
        raw = {"apiVersion": "okdev.io/v1alpha1", "kind": "DevEnvironment", "metadata": {"name": "access"},
               "spec": {"namespace": self.namespace, "attachOnly": {"selector": "app=external", "container": "dev",
                         "setup": "echo setup >> /tmp/setup-ran"},
                         "lifecycle": {"postCreate": "touch /tmp/automatic-hook"}}}
        config.write_text(json.dumps(raw))
        base = self.base(config)
        self.run_cmd(base + ["validate"])
        for args in (["up"], ["up", "--dry-run"], ["down", "--yes"], ["restart", "--yes"],
                     ["restart", "--pod", "external-0", "--yes"], ["sync"], ["ssh"]):
            rejected = self.run_cmd(base + args, check=False)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertIn(b"attach-only", rejected.stderr)
        ambiguous = self.run_cmd(base + ["exec", "--", "true"], check=False)
        self.assertNotEqual(ambiguous.returncode, 0)
        selected = self.run_cmd(base + ["exec", "--pod", "external-1", "--json", "--", "sh", "-c",
                                        "printf clean; printf diagnostic >&2; exit 7"])
        row = json.loads(selected.stdout)[0]
        self.assertEqual((row["pod"], row["exit"], row["stdout"], row["stderr"]),
                         ("external-1", 7, "clean", "diagnostic"))
        self.remote("external-0", "touch", "/tmp/dev-only")
        self.run_cmd(base + ["exec", "--pod", "external-0", "--container", "other", "--", "test", "!", "-e", "/tmp/dev-only"])
        payload = b"attach-only copy\x00" * 65536
        source = self.root / "payload"
        source.write_bytes(payload)
        self.run_cmd(base + ["cp", "--all", str(source), ":/tmp/payload"])
        for pod in ("external-0", "external-1"):
            self.assertEqual(self.remote(pod, "sha256sum", "/tmp/payload").stdout.split()[0].decode(), hashlib.sha256(payload).hexdigest())
        dest = self.root / "download"
        # Narrowing the config to an explicit pod enables the default target.
        single = self.root / "single.yaml"
        scoped = json.loads(config.read_text())
        scoped["spec"]["attachOnly"].pop("selector")
        scoped["spec"]["attachOnly"]["pods"] = ["external-0"]
        single.write_text(json.dumps(scoped))
        one = self.base(single)
        self.run_cmd(one + ["cp", ":/tmp/payload", str(dest)])
        self.assertEqual(hashlib.sha256(dest.read_bytes()).digest(), hashlib.sha256(payload).digest())
        excluded = self.run_cmd(one + ["exec", "--pod", "external-1", "--", "true"], check=False)
        self.assertNotEqual(excluded.returncode, 0)
        launched = self.run_cmd(base + ["exec", "--all", "--detach", "--", "sh", "-c", "echo detached"])
        job = re.search(rb"job_id=([^ ]+)", launched.stdout).group(1).decode()
        self.run_cmd(base + ["jobs", "wait", job])
        self.assertEqual(self.run_cmd(base + ["jobs", "logs", job, "--no-prefix"]).stdout, b"detached\ndetached\n")
        running = self.run_cmd(one + ["exec", "--detach", "--", "sh", "-c", 'printf "%s" "$OKDEV_JOB_ID" > /tmp/attach-job; sleep 120'])
        job = re.search(rb"job_id=([^ ]+)", running.stdout).group(1).decode()
        self.run_cmd(one + ["jobs", "ready", job, "--probe", "cat /tmp/attach-job", "--timeout", "10s"])
        self.run_cmd(one + ["jobs", "list", "--output", "json"])
        self.run_cmd(one + ["jobs", "stop", job])
        for pod in ("external-0", "external-1"):
            self.remote(pod, "test", "!", "-e", "/tmp/automatic-hook")
            self.remote(pod, "test", "!", "-e", "/tmp/setup-ran")
        self.run_cmd(base + ["attach-setup", "--pod", "external-1"])
        self.assertEqual(self.remote("external-1", "cat", "/tmp/setup-ran").stdout, b"setup\n")
        self.remote("external-0", "test", "!", "-e", "/tmp/setup-ran")
        self.assertEqual(self.snapshot(), before)

        # Existing owner labels remain authoritative, even in an external scope.
        self.run_cmd(["kubectl", "-n", self.namespace, "label", "pod", "external-1", "okdev.io/owner=someone-else"])
        denied = self.run_cmd(base + ["exec", "--all", "--", "true"], check=False)
        self.assertNotEqual(denied.returncode, 0)
        self.assertIn(b"owned by", denied.stderr)
        self.run_cmd(["kubectl", "-n", self.namespace, "label", "pod", "external-1", "okdev.io/owner-"])
        # A managed-session selector is also usable without a workload manifest.
        self.run_cmd(["kubectl", "-n", self.namespace, "label", "pod", "external-0", "okdev.io/managed=true", "okdev.io/session=existing"])
        scoped["spec"]["attachOnly"].pop("pods")
        scoped["spec"]["attachOnly"]["session"] = "existing"
        single.write_text(json.dumps(scoped))
        self.run_cmd(one + ["exec", "--", "true"])
        self.check_rbac(one)

    def snapshot(self):
        pods = json.loads(self.run_cmd(["kubectl", "-n", self.namespace, "get", "pods", "-l", "app=external", "-o", "json"]).stdout)["items"]
        controller = json.loads(self.run_cmd(["kubectl", "-n", self.namespace, "get", "statefulset", "external", "-o", "json"]).stdout)
        return {"controllerUID": controller["metadata"]["uid"], "controllerSpec": controller["spec"],
                "pods": {p["metadata"]["name"]: {"uid": p["metadata"]["uid"], "labels": p["metadata"]["labels"],
                         "owners": p["metadata"].get("ownerReferences"), "spec": p["spec"]} for p in pods}}

    def check_rbac(self, base):
        self.run_cmd(["kubectl", "-n", self.namespace, "create", "serviceaccount", "viewer"])
        self.run_cmd(["kubectl", "-n", self.namespace, "create", "role", "view-pods", "--verb=get,list", "--resource=pods"])
        self.run_cmd(["kubectl", "-n", self.namespace, "create", "rolebinding", "viewer", "--role=view-pods",
                      "--serviceaccount=" + self.namespace + ":viewer"])
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        raw["users"][0]["user"]["as"] = "system:serviceaccount:" + self.namespace + ":viewer"
        restricted = self.root / "restricted-kubeconfig"
        restricted.write_text(json.dumps(raw))
        original = self.env["KUBECONFIG"]
        self.env["KUBECONFIG"] = str(restricted)
        try:
            denied = self.run_cmd(base + ["exec", "--", "touch", "/tmp/forbidden"], check=False)
        finally:
            self.env["KUBECONFIG"] = original
        self.assertNotEqual(denied.returncode, 0)
        self.assertIn(b"forbidden", denied.stderr.lower())
        self.remote("external-0", "test", "!", "-e", "/tmp/forbidden")


if __name__ == "__main__":
    unittest.main()
