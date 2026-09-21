"""Only read-only preflight retries; a delivered command is never replayed."""
import json
import signal
import subprocess
import time
import unittest
from urllib.parse import urlsplit

from e2e_kind_regression_readiness import APIProxy
from e2e_kind_support import KindRegression


class ExecRetry(KindRegression):
    def test_budget_recovery_cancellation_and_ambiguous_delivery(self):
        config, _ = self.config("retry")
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"] = []
        config.write_text(json.dumps(raw))
        self.up(config, "retry")
        pod = self.pods("retry")[0]["metadata"]["name"]
        base = self.base(config, "retry")
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        upstream = urlsplit(raw["clusters"][0]["cluster"]["server"])
        live = (upstream.hostname, upstream.port or 443)
        proxy = APIProxy(("127.0.0.1", 1))
        self.addCleanup(proxy.close)
        raw["clusters"][0]["cluster"]["server"] = f"https://127.0.0.1:{proxy.server_address[1]}"
        raw["clusters"][0]["cluster"]["tls-server-name"] = upstream.hostname
        proxied = self.root / "proxy-kubeconfig"
        proxied.write_text(json.dumps(raw))
        original = self.env["KUBECONFIG"]

        def through_proxy(args, background=False, **kwargs):
            self.env["KUBECONFIG"] = str(proxied)
            try:
                return (self.background if background else self.run_cmd)(base + args, **kwargs)
            finally:
                self.env["KUBECONFIG"] = original

        started = time.monotonic()
        failed = through_proxy(["exec", "--preflight-retry-timeout", "1s", "--json", "--",
                                "touch", "/tmp/must-not-run"], check=False)
        self.assertEqual(failed.returncode, 78, failed.stderr.decode())
        self.assertEqual(failed.stdout, b"")
        self.assertLess(time.monotonic() - started, 6)
        self.remote(pod, "test", "!", "-e", "/tmp/must-not-run")
        for cancel in (True, False):
            log = self.root / f"preflight-{cancel}.log"
            with log.open("wb") as errors:
                process = through_proxy(["exec", "--preflight-retry-timeout", "30s", "--json", "--",
                                         "sh", "-c", "echo once >> /tmp/recovered; printf clean"],
                                        background=True, stdout=subprocess.PIPE, stderr=errors)
                deadline = time.monotonic() + 20
                while "retry 1" not in log.read_text():
                    self.assertIsNone(process.poll(), log.read_text())
                    self.assertLess(time.monotonic(), deadline, log.read_text())
                    time.sleep(.1)
                if cancel:
                    process.send_signal(signal.SIGINT)
                    process.communicate(timeout=10)
                    self.assertNotEqual(process.returncode, 0)
                    self.remote(pod, "test", "!", "-e", "/tmp/recovered")
                else:
                    proxy.upstream = live
                    stdout, _ = process.communicate(timeout=40)
                    self.assertEqual(process.returncode, 0, log.read_text())
                    result = json.loads(stdout)[0]
                    self.assertEqual((result["exit"], result["stdout"]), (0, "clean"))
                    self.assertEqual(self.remote(pod, "cat", "/tmp/recovered").stdout, b"once\n")

        # Observe a side effect before dropping the API stream. A reconnect must
        # not execute the command again, even though the server is reachable.
        process = through_proxy(["exec", "--preflight-retry-timeout", "10s", "--json", "--require-all", "--",
                                 "sh", "-c", "echo once >> /tmp/delivered; sleep 20"],
                                background=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        deadline = time.monotonic() + 20
        while True:
            probe = self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--",
                                  "test", "-f", "/tmp/delivered"], check=False)
            if probe.returncode == 0:
                break
            self.assertIsNone(process.poll())
            self.assertLess(time.monotonic(), deadline)
            time.sleep(.1)
        self.assertGreater(proxy.disconnect(), 0)
        stdout, stderr = process.communicate(timeout=30)
        self.assertNotEqual(process.returncode, 0, stderr.decode())
        self.assertNotEqual(json.loads(stdout)[0]["status"], "responded")
        self.assertEqual(self.remote(pod, "cat", "/tmp/delivered").stdout, b"once\n")
        result = through_proxy(["exec", "--preflight-retry-timeout", "10s", "--json", "--",
                                "sh", "-c", "echo once >> /tmp/nonzero; exit 7"])
        self.assertEqual(json.loads(result.stdout)[0]["exit"], 7)
        self.assertEqual(self.remote(pod, "cat", "/tmp/nonzero").stdout, b"once\n")


if __name__ == "__main__":
    unittest.main()
