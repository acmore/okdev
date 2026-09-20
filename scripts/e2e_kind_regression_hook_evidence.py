"""Hook exit success is separate from prerequisites and exposes per-pod output."""
import json
import unittest

from e2e_kind_support import KindRegression


class HookEvidence(KindRegression):
    def test_noop_output_failure_and_retry(self):
        config, _ = self.config("hooks", replicas=2, hooks={
            "postSync": "sh /workspace/missing-setup.sh || true",
            "postCreate": "for patch in /workspace/patches/*.patch; do [ -f \"$patch\" ] || continue; done",
        })
        result = self.up(config, "hooks")
        text = (result.stdout + result.stderr).decode()
        self.assertIn("hook exited 0 on 2 pod(s)", text)
        self.assertIn("setup effects are not independently verified", text)
        self.assertNotIn("workspace synced; ran on", text)
        pods = self.pods("hooks")
        self.assertEqual(len(pods), 2)
        for pod in pods:
            name = pod["metadata"]["name"]
            self.assertIn(f"[postSync pod={name}]", text)
            self.assertIn("missing-setup.sh", text)
            self.assertEqual(pod["metadata"]["annotations"]["okdev.io/post-sync-state"], "done")
            self.assertEqual(pod["metadata"]["annotations"]["okdev.io/post-create-state"], "done")
        # A successful empty hook remains successful and is not replayed.
        resumed = self.up(config, "hooks")
        self.assertIn(b"previous exit 0 recorded on 2 pod(s)", resumed.stdout)
        self.assertNotIn(b"[postSync pod=", resumed.stderr)

        raw = json.loads(config.read_text())
        raw["spec"]["lifecycle"]["postSync"] = (
            'set -eu; printf "checking prerequisites\\n"; '
            '[ -f /tmp/okdev-hook-prerequisite ] || '
            '{ printf "missing prerequisite\\n" >&2; exit 9; }; '
            'printf "verified\\n" > /tmp/okdev-hook-result; '
            'test -s /tmp/okdev-hook-result; printf "setup checked\\n"'
        )
        config.write_text(json.dumps(raw))
        # Explicitly reset the recorded completion for this test's new command.
        for pod in pods:
            self.run_cmd(["kubectl", "-n", self.namespace, "annotate", "pod", pod["metadata"]["name"],
                          "okdev.io/post-sync-done-", "okdev.io/post-sync-state-", "okdev.io/post-sync-at-"])
        failed = self.run_cmd(self.base(config, "hooks") + ["up", "--no-tmux"], check=False)
        self.assertNotEqual(failed.returncode, 0)
        text = (failed.stdout + failed.stderr).decode()
        self.assertNotIn("== Ready ==", text)
        for pod in self.pods("hooks"):
            name = pod["metadata"]["name"]
            self.assertIn(f"[postSync pod={name}] checking prerequisites", text)
            self.assertIn(f"[postSync pod={name}] missing prerequisite", text)
            self.assertEqual(pod["metadata"]["annotations"]["okdev.io/post-sync-state"], "failed")
            self.remote(name, "touch", "/tmp/okdev-hook-prerequisite")
        repaired = self.up(config, "hooks")
        self.assertIn(b"hook exited 0 on 2 pod(s)", repaired.stdout)
        for pod in self.pods("hooks"):
            name = pod["metadata"]["name"]
            self.assertIn(f"[postSync pod={name}] setup checked", repaired.stderr.decode())
            self.assertEqual(self.remote(name, "cat", "/tmp/okdev-hook-result").stdout, b"verified\n")


if __name__ == "__main__":
    unittest.main()
