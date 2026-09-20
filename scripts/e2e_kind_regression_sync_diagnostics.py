"""Ready must describe the live sync channel, including early child death."""
import json
import os
import signal
import subprocess
import time
import unittest

from e2e_kind_support import KindRegression


class SyncDiagnostics(KindRegression):
    def test_early_death_warning_gate_and_repair(self):
        config, data = self.config("early", hooks={"postCreate":
            "touch /tmp/hook-entered; while [ ! -f /tmp/hook-release ]; do sleep 1; done"})
        base = self.base(config, "early")
        log = self.root / "up.log"
        with log.open("wb") as output:
            process = self.background(base + ["up", "--no-tmux", "--wait-timeout", "2m"],
                                      stdout=output, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 180
            pod = None
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    self.fail(log.read_text())
                pods = self.pods("early")
                if pods:
                    pod = pods[0]["metadata"]["name"]
                    probe = self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev",
                                          "--", "test", "-f", "/tmp/hook-entered"], check=False)
                    if probe.returncode == 0:
                        break
                time.sleep(1)
            else:
                self.fail("hook did not start: " + log.read_text())
            home = self.home / ".okdev/sessions/early/syncthing"
            pid = int((home / "sync.pid").read_text())
            command = self.run_cmd(["ps", "-p", str(pid), "-o", "command="]).stdout.decode()
            self.assertIn(str(config), command)
            self.assertIn("sync", command)
            os.kill(pid, signal.SIGTERM)
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                alive = self.run_cmd(["ps", "-p", str(pid), "-o", "stat="], check=False)
                if alive.returncode or b"Z" in alive.stdout:
                    break
                time.sleep(.2)
            else:
                self.fail("owned sync process did not stop")
            self.remote(pod, "touch", "/tmp/hook-release")
            code = process.wait(timeout=60)
        text = log.read_text()
        self.assertNotEqual(code, 0, text)
        self.assertIn("sync readiness degraded", text)
        self.assertNotIn("== Ready ==", text)
        record = json.loads((home / "sync.last-exit.json").read_text())
        self.assertEqual(record["pid"], pid)
        self.assertIn("received", record["reason"])
        details = self.run_cmd(base + ["status", "--details"]).stdout.decode()
        self.assertIn("last recorded sync exit", details)
        self.assertIn(record["reason"], details)
        payload = json.loads(self.run_cmd(base + ["status", "--details", "--output", "json"]).stdout)
        self.assertEqual(payload["sync"]["lastExit"]["pid"], pid)
        self.assertEqual(payload["sync"]["health"], "stopped")
        first = self.run_cmd(base + ["exec", "--", "printf", "clean"])
        second = self.run_cmd(base + ["exec", "--", "printf", "clean"])
        self.assertEqual(first.stdout, b"clean")
        self.assertEqual(second.stdout, b"clean")
        self.assertIn(b"warning:", first.stderr)
        self.assertIn(b"sync still unhealthy", second.stderr)
        self.assertIn(b"may run stale code", second.stderr)
        blocked = self.run_cmd(base + ["exec", "--require-sync", "--", "touch", "/tmp/forbidden"], check=False)
        self.assertNotEqual(blocked.returncode, 0)
        self.remote(pod, "test", "!", "-e", "/tmp/forbidden")
        self.up(config, "early")
        time.sleep(3)
        (data / "after-parent-exit").write_text("survived\n")
        self.run_cmd(base + ["sync", "wait", "--timeout", "60s"])
        self.assertEqual(self.remote(pod, "cat", "/workspace/after-parent-exit").stdout, b"survived\n")
        healthy = self.run_cmd(base + ["exec", "--", "true"])
        self.assertNotIn(b"warning: sync", healthy.stderr)
        new_pid = int((home / "sync.pid").read_text())
        self.assertNotEqual(new_pid, pid)
        command = self.run_cmd(["ps", "-p", str(new_pid), "-o", "command="]).stdout.decode()
        self.assertIn(str(config), command)
        os.kill(new_pid, signal.SIGKILL)
        time.sleep(1)
        details = self.run_cmd(base + ["status", "--details"]).stdout.decode()
        self.assertIn("exit cause unavailable", details)


if __name__ == "__main__":
    unittest.main()
