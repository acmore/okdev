"""A healthy old instance cannot satisfy readiness for a new detached job."""
import json
import re
import signal
import subprocess
import time
import unittest

from e2e_kind_support import KindRegression


class JobsReady(KindRegression):
    def test_job_identity_exit_deadline_and_members(self):
        config, _ = self.config("ready", replicas=2)
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"] = []
        config.write_text(json.dumps(raw))
        self.up(config, "ready")
        base = self.base(config, "ready")
        pods = [p["metadata"]["name"] for p in self.pods("ready")]
        self.assertEqual(len(pods), 2)
        probe = "test -f /tmp/service-healthy && cat /tmp/service-instance"

        def launch(script):
            result = self.run_cmd(base + ["exec", "--all", "--detach", "--", "sh", "-c", script])
            return re.search(rb"job_id=([^ ]+)", result.stdout).group(1).decode()

        old = launch('printf "%s\\n" "$OKDEV_JOB_ID" > /tmp/service-instance; touch /tmp/service-healthy; sleep 120')
        self.run_cmd(base + ["jobs", "ready", old, "--probe", probe, "--timeout", "20s"])
        new = launch('while [ ! -f /tmp/allow-new ]; do sleep 1; done; '
                     'printf "%s\\n" "$OKDEV_JOB_ID" > /tmp/service-instance; sleep 120')
        refused = self.run_cmd(base + ["jobs", "ready", new, "--probe", probe, "--timeout", "2s"], check=False)
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn(b"expected job ID", refused.stderr)
        # One healthy member is insufficient for a multi-pod readiness result.
        self.remote(pods[0], "touch", "/tmp/allow-new")
        refused = self.run_cmd(base + ["jobs", "ready", new, "--probe", probe, "--timeout", "2s"], check=False)
        self.assertNotEqual(refused.returncode, 0)
        self.remote(pods[1], "touch", "/tmp/allow-new")
        result = self.run_cmd(base + ["jobs", "ready", new, "--probe", probe, "--timeout", "20s", "--output", "json"])
        payload = json.loads(result.stdout)
        self.assertEqual(payload["jobId"], new)
        self.assertTrue(payload["ready"])
        self.assertEqual(set(payload["pods"]), set(pods))
        # Readiness has not stopped either launch; stop only the old job ID.
        self.run_cmd(base + ["jobs", "stop", old])
        self.run_cmd(base + ["jobs", "ready", new, "--probe", probe, "--timeout", "10s"])
        short = launch("sleep 1; exit 9")
        started = time.monotonic()
        failed = self.run_cmd(base + ["jobs", "ready", short, "--probe", probe, "--timeout", "30s"], check=False)
        self.assertNotEqual(failed.returncode, 0)
        self.assertLess(time.monotonic() - started, 15)
        self.assertIn(b"not running", failed.stderr)
        hung = self.run_cmd(base + ["jobs", "ready", new, "--probe", "sleep 20", "--probe-timeout", "200ms", "--timeout", "1s"], check=False)
        self.assertNotEqual(hung.returncode, 0)
        process = self.background(base + ["jobs", "ready", new, "--probe", "false", "--timeout", "1m"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        time.sleep(1)
        self.assertIsNone(process.poll())
        process.send_signal(signal.SIGINT)
        process.communicate(timeout=10)
        self.assertNotEqual(process.returncode, 0)
        self.run_cmd(base + ["jobs", "stop", new])
        stopped = self.run_cmd(base + ["jobs", "ready", new, "--probe", probe], check=False)
        self.assertNotEqual(stopped.returncode, 0)
        # Ordinary completion wait retains its semantics.
        completed = launch("exit 0")
        self.run_cmd(base + ["jobs", "wait", completed])


if __name__ == "__main__":
    unittest.main()
