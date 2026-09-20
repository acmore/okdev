"""Run the snapshot distribution recipe with sync paused (#276)."""
import re
import shlex
import unittest

from e2e_kind_support import KindRegression, REPO


class Snapshot(KindRegression):
    def test_distribution_is_explicit_verified_and_independent_of_sync(self):
        config, _ = self.config("snapshot", replicas=2)
        self.up(config, "snapshot")
        base = self.base(config, "snapshot")
        self.run_cmd(base + ["sync", "pause"])
        unavailable = self.run_cmd(base + ["sync", "wait", "--timeout", "2s"], check=False)
        self.assertNotEqual(unavailable.returncode, 0, "sync unexpectedly converged while paused")
        pods = [pod["metadata"]["name"] for pod in self.pods("snapshot")]
        self.assertEqual(len(pods), 2)
        source = self.root / "snapshot-source"
        source.mkdir()
        payload = bytes(range(256)) * 4096
        (source / "name with spaces").write_bytes(payload)
        (source / "empty-file").touch()
        (source / "empty-directory").mkdir()
        (source / "run.sh").write_text("#!/bin/sh\nprintf 'snapshot-ok\\n'\n")
        (source / "run.sh").chmod(0o755)
        document = (REPO / "docs/snapshot-distribution.md").read_text()
        match = re.search(r"<!-- kind-recipe: snapshot -->\n```bash\n(.*?)\n```", document, re.S)
        self.assertIsNotNone(match, "missing documented recipe")
        wrapper = 'okdev() { command ' + shlex.join(base) + ' "$@"; }\n'

        def recipe(receivers):
            variables = f"pod_a={shlex.quote(receivers[0])}\npod_b={shlex.quote(receivers[1])}\n"
            return wrapper + variables + match.group(1)

        delivered = self.run_cmd(["bash", "-c", recipe(pods)])
        directory = re.search(rb"^snapshot_path=(.+)$", delivered.stdout, re.M).group(1).decode()
        archive = re.search(rb"^archive_path=(.+)$", delivered.stdout, re.M).group(1).decode()
        self.assertEqual(delivered.stdout.count(b": copied "), 2)
        self.assertEqual(delivered.stdout.count((archive + ": OK").encode()), 2)
        for pod in pods:
            self.assertEqual(self.remote(pod, "cat", directory + "/name with spaces").stdout, payload)
            self.remote(pod, "test", "-d", directory + "/empty-directory")
            self.assertEqual(self.remote(pod, "cat", directory + "/empty-file").stdout, b"")
            self.assertEqual(self.remote(pod, directory + "/run.sh").stdout, b"snapshot-ok\n")
        (source / "name with spaces").write_bytes(b"later local edit")
        for pod in pods:
            self.assertEqual(self.remote(pod, "cat", directory + "/name with spaces").stdout, payload)
        missing = self.run_cmd(["bash", "-c", recipe([pods[0], "missing-receiver"])], check=False)
        self.assertNotEqual(missing.returncode, 0)
        self.assertNotIn(b"snapshot_path=", missing.stdout)

        probe = source / "empty-file"
        probe.write_bytes(b"copy probe\n")
        self.run_cmd(base + ["cp", str(probe), ":/tmp/target-only-probe"])
        found = []
        for pod in pods:
            check = self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev",
                                  "--", "test", "-f", "/tmp/target-only-probe"], check=False)
            found.append(check.returncode == 0)
        self.assertEqual(sum(found), 1, "default cp unexpectedly fanned out")
        self.run_cmd(base + ["cp", "--all", str(probe), ":/tmp/all-probe"])
        for pod in pods:
            self.assertEqual(self.remote(pod, "cat", "/tmp/all-probe").stdout, b"copy probe\n")

        # One destination cannot be created: partial fanout must not report success.
        self.remote(pods[0], "mkdir", "/tmp/copy-obstruction")
        self.remote(pods[1], "touch", "/tmp/copy-obstruction")
        failed = self.run_cmd(base + ["cp", "--all", str(probe), ":/tmp/copy-obstruction/probe"], check=False)
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn(b"1 of 2 pods failed", failed.stderr)
        self.assertIn(b": error:", failed.stdout)
        self.assertIn(b": copied ", failed.stdout)
        self.assertEqual(self.remote(pods[0], "cat", "/tmp/copy-obstruction/probe").stdout, b"copy probe\n")


if __name__ == "__main__":
    unittest.main()
