"""Kind regression for active-session scope and historical evidence (#288)."""
import json
import unittest

from e2e_kind_support import KindRegression


class StatusIdentity(KindRegression):
    def test_live_selection_history_scope_and_api_failure(self):
        first, _ = self.config("status-a")
        second, _ = self.config("status-b", relative=".okdev/okdev.yaml")
        self.up(first, "status-a")
        self.up(second, "status-b")
        self.cli("use", "status-a")
        self.run_cmd(self.base(second, "status-b") + ["down", "--yes"])
        self.sessions.remove((second, "status-b"))
        original_uid = self.pods("status-a")[0]["metadata"]["uid"]

        def check_live():
            for selection in ([], ["--session", "status-a"]):
                rows = json.loads(self.run_cmd([self.binary, *selection, "status", "--output", "json"]).stdout)
                self.assertIsInstance(rows, list, rows)
                self.assertEqual([(row["session"], row["phase"]) for row in rows], [("status-a", "Running")])
            self.assertEqual(self.pods("status-a")[0]["metadata"]["uid"], original_uid)

        check_live()
        # The discovered config can change independently of the active session's
        # saved config. Bare status must continue to observe the selected session.
        raw = json.loads(second.read_text())
        raw["spec"]["namespace"] = "kube-system"
        second.write_text(json.dumps(raw))
        check_live()
        cache = self.home / ".okdev/sessions/status-a/last-seen.json"
        snapshot = cache.read_bytes()
        elsewhere = self.run_cmd(self.base(first, "status-a") +
                                  ["--namespace", "kube-system", "status"]).stdout
        self.assertIn(b"No matching sessions found", elsewhere)
        self.assertNotIn(b"Last known state", elsewhere)
        self.assertEqual(cache.read_bytes(), snapshot)

        original_kube = self.root / "kubeconfig"
        original_text = original_kube.read_text()
        raw_kube = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        raw_kube["clusters"][0]["cluster"]["server"] = "https://127.0.0.1:1"
        try:
            original_kube.write_text(json.dumps(raw_kube))
            failed = self.run_cmd(self.base(first, "status-a") + ["status"], check=False, timeout=30)
            self.assertNotEqual(failed.returncode, 0)
            self.assertNotIn(b"Last known state", failed.stdout)
            self.assertNotIn(b"No matching sessions found", failed.stdout)
            self.assertEqual(cache.read_bytes(), snapshot)
        finally:
            original_kube.write_text(original_text)

        # Delete externally, so intentional down does not clear the snapshot.
        pod = self.pods("status-a")[0]["metadata"]["name"]
        self.run_cmd(["kubectl", "-n", self.namespace, "delete", "pod", pod, "--wait=true"])
        report = json.loads(self.run_cmd(self.base(first, "status-a") + ["status", "--output", "json"]).stdout)
        self.assertFalse(report["found"])
        self.assertTrue(report["historical"])
        self.assertEqual(report["context"], "kind-" + self.cluster)
        self.assertEqual(report["namespace"], self.namespace)
        self.assertEqual(report["lastSeenAt"], json.loads(snapshot)["at"].split(".")[0] + "Z")
        self.run_cmd(["kubectl", "config", "set-context", "other-context", "--cluster", "kind-" + self.cluster,
                      "--user", "kind-" + self.cluster])
        other = self.run_cmd(self.base(first, "status-a") + ["--context", "other-context", "status"]).stdout
        self.assertNotIn(b"Last known state", other)
        self.assertEqual(cache.read_bytes(), snapshot)


if __name__ == "__main__":
    unittest.main()
