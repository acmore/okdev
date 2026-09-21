"""Replica diagnostics compare rendered intent without reconciling workloads."""
import json
import os
import time
import unittest

from e2e_kind_support import KindRegression


class Replicas(KindRegression):
    def test_deployment_scale_and_manifest_change(self):
        config, _ = self.config("replicas", replicas=2)
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"] = []
        config.write_text(json.dumps(raw))
        manifest = config.parent / "workload.json"
        original = manifest.read_bytes()
        self.up(config, "replicas")
        base = self.base(config, "replicas")
        initial = json.loads(self.run_cmd(base + ["status", "--output", "json"]).stdout)[0]
        group = initial["replicas"]["groups"][0]
        self.assertEqual((group["declared"], group["present"], group["countMismatch"]), (2, 2, False))
        self.run_cmd(["kubectl", "-n", self.namespace, "scale", "deployment/replicas", "--replicas=1"])
        deadline = time.monotonic() + 60
        while True:
            status = json.loads(self.run_cmd(base + ["status", "--output", "json"]).stdout)[0]
            if status["replicas"]["groups"][0]["present"] == 1:
                break
            self.assertLess(time.monotonic(), deadline, status)
            time.sleep(1)
        group = status["replicas"]["groups"][0]
        self.assertTrue(group["countMismatch"])
        for flags in ([], ["--details"]):
            text = self.run_cmd(base + ["status"] + flags).stdout.decode()
            self.assertIn("manifest declares 2; 1 present", text)
        detail = json.loads(self.run_cmd(base + ["status", "--details", "--output", "json"]).stdout)
        self.assertEqual(detail["replicas"]["groups"][0], group)
        all_rows = json.loads(self.run_cmd(base + ["status", "--all", "--output", "json"]).stdout)
        self.assertTrue(all("replicas" not in row for row in all_rows))
        self.assertEqual(manifest.read_bytes(), original)
        live = json.loads(self.run_cmd(["kubectl", "-n", self.namespace, "get", "deployment", "replicas", "-o", "json"]).stdout)
        self.assertEqual(live["spec"]["replicas"], 1)
        raw = json.loads(original)
        raw["spec"]["replicas"] = 1
        manifest.write_text(json.dumps(raw))
        status = json.loads(self.run_cmd(base + ["status", "--output", "json"]).stdout)[0]
        self.assertFalse(status["replicas"]["groups"][0]["countMismatch"])


@unittest.skipUnless(os.environ.get("RUN_PYTORCHJOB") == "1", "requires training operator")
class PyTorchReplicas(KindRegression):
    def test_master_and_worker_counts(self):
        config, _ = self.config("train")
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"] = []
        raw["spec"]["workload"] = {"type": "pytorchjob", "manifestPath": "workload.json",
            "attach": {"container": "pytorch"}, "inject": [
                {"path": "spec.pytorchReplicaSpecs.Master.template"},
                {"path": "spec.pytorchReplicaSpecs.Worker.template"}]}
        config.write_text(json.dumps(raw))
        replica = {"replicas": 1, "restartPolicy": "Never", "template": {"spec": {
            "terminationGracePeriodSeconds": 1,
            "containers": [{"name": "pytorch", "image": "ubuntu:22.04", "command": ["sleep", "infinity"]}]}}}
        manifest = config.parent / "workload.json"
        obj = {"apiVersion": "kubeflow.org/v1", "kind": "PyTorchJob", "metadata": {"name": "train"},
               "spec": {"pytorchReplicaSpecs": {"Master": replica, "Worker": replica}}}
        manifest.write_text(json.dumps(obj))
        self.up(config, "train")
        base = self.base(config, "train")
        deadline = time.monotonic() + 60
        while True:
            status = json.loads(self.run_cmd(base + ["status", "--output", "json"]).stdout)[0]
            groups = {row["role"]: row for row in status["replicas"]["groups"]}
            if all(row["present"] == 1 for row in groups.values()):
                break
            self.assertLess(time.monotonic(), deadline, groups)
            time.sleep(1)
        self.assertEqual(set(groups), {"Master", "Worker"})
        self.assertTrue(all(not row["countMismatch"] for row in groups.values()))
        # Edit only the declared intent: status must not apply it to the live job.
        obj = json.loads(manifest.read_text())
        obj["spec"]["pytorchReplicaSpecs"]["Worker"]["replicas"] = 4
        manifest.write_text(json.dumps(obj))
        original = manifest.read_bytes()
        result = json.loads(self.run_cmd(base + ["status", "--details", "--output", "json"]).stdout)
        groups = {row["role"]: row for row in result["replicas"]["groups"]}
        self.assertFalse(groups["Master"]["countMismatch"])
        self.assertEqual((groups["Worker"]["declared"], groups["Worker"]["present"]), (4, 1))
        self.assertTrue(groups["Worker"]["countMismatch"])
        text = self.run_cmd(base + ["status"]).stdout.decode()
        self.assertIn("Worker: manifest declares 4; 1 present", text)
        self.assertEqual(manifest.read_bytes(), original)
        live = json.loads(self.run_cmd(["kubectl", "-n", self.namespace, "get", "pytorchjob", "train", "-o", "json"]).stdout)
        self.assertEqual(live["spec"]["pytorchReplicaSpecs"]["Worker"]["replicas"], 1)


if __name__ == "__main__":
    unittest.main()
