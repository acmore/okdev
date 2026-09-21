"""Evaluate SSH reuse without treating it as an equivalent exec transport."""
from concurrent.futures import ThreadPoolExecutor
import json
import os
import platform
import signal
import subprocess
import time
import unittest
from urllib.parse import urlsplit

from e2e_kind_regression_readiness import APIProxy
from e2e_kind_support import KindRegression


class ExecTransport(KindRegression):
    def test_reuse_contract_and_api_disconnect(self):
        config, _ = self.config("transport")
        self.up(config, "transport")
        base = self.base(config, "transport")
        pod = self.pods("transport")[0]["metadata"]["name"]
        ssh = ["ssh", "-T", "-F", str(self.home / ".ssh/config"),
               "-o", "BatchMode=yes", "-o", "ConnectTimeout=5"]
        host = "okdev-transport"
        cold = ssh + ["-S", "none", host]
        socket = str(self.root / "master.sock")
        # A missing master must fail, never silently create another connection.
        warm = ssh + ["-S", socket, "-o", "ProxyCommand=false", host]
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        upstream = urlsplit(raw["clusters"][0]["cluster"]["server"])
        proxy = APIProxy((upstream.hostname, upstream.port or 443))
        self.addCleanup(proxy.close)
        raw["clusters"][0]["cluster"].update(server=f"https://127.0.0.1:{proxy.server_address[1]}",
                                            **{"tls-server-name": upstream.hostname})
        proxied = self.root / "proxy-kubeconfig"
        proxied.write_text(json.dumps(raw))
        original = self.env["KUBECONFIG"]
        self.env["KUBECONFIG"] = str(proxied)
        master_log = self.root / "master.log"
        with master_log.open("wb") as log:
            master = self.background(ssh + ["-M", "-N", "-S", socket, host],
                                     stdout=subprocess.DEVNULL, stderr=log)
        self.env["KUBECONFIG"] = original
        deadline = time.monotonic() + 20
        while not os.path.exists(socket):
            self.assertIsNone(master.poll(), master_log.read_text())
            self.assertLess(time.monotonic(), deadline, master_log.read_text())
            time.sleep(.1)
        self.run_cmd(ssh + ["-S", socket, "-O", "check", host])
        script = "printf stdout; printf stderr >&2; exit 7"
        result = self.run_cmd(warm + [script], check=False)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (7, b"stdout", b"stderr"))
        direct = self.run_cmd(base + ["exec", "--json", "--pod", pod, "--container", "dev", "--", "sh", "-c", script])
        row = json.loads(direct.stdout)[0]
        self.assertEqual((row["pod"], row["exit"], row["stdout"], row["stderr"]), (pod, 7, "stdout", "stderr"))
        self.assertEqual(self.run_cmd(warm + ["hostname"]).stdout.strip().decode(), pod)
        self.assertEqual(self.run_cmd(warm + ["cat"], data=b"input\x00data").stdout, b"input\x00data")
        if os.environ.get("EXEC_BENCH_RUNS"):
            self.benchmark(base, pod, cold, warm)

        # Cancel only the client channel; do not infer that its remote children died.
        process = self.background(warm + ["echo started > /tmp/cancel-start; sleep 30"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.wait_file(pod, "/tmp/cancel-start", process)
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=10)
        self.assertNotEqual(process.returncode, 0)
        self.run_cmd(warm + ["true"])

        # A delivered command must not be replayed across loss of the API stream.
        process = self.background(warm + ["echo once >> /tmp/delivered; sleep 30"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.wait_file(pod, "/tmp/delivered", process)
        self.assertGreater(proxy.disconnect(), 0)
        process.communicate(timeout=15)
        self.assertNotEqual(process.returncode, 0)
        master.wait(timeout=15)
        self.assertEqual(self.remote(pod, "cat", "/tmp/delivered").stdout, b"once\n")
        self.assertNotEqual(self.run_cmd(warm + ["echo replay >> /tmp/delivered"], check=False).returncode, 0)
        self.assertEqual(self.remote(pod, "cat", "/tmp/delivered").stdout, b"once\n")

    def wait_file(self, pod, path, process):
        deadline = time.monotonic() + 15
        while self.remote_probe(pod, path).returncode:
            self.assertIsNone(process.poll())
            self.assertLess(time.monotonic(), deadline)
            time.sleep(.1)

    def remote_probe(self, pod, path):
        return self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--",
                             "test", "-f", path], check=False)

    def benchmark(self, base, pod, cold, warm):
        count = int(os.environ["EXEC_BENCH_RUNS"])
        self.assertGreaterEqual(count, 2)
        commands = {
            "okdev-exec": base + ["exec", "--json", "--pod", pod, "--container", "dev", "--", "true"],
            "kubectl-exec": ["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--", "true"],
            "ssh-cold": cold + ["true"],
            "ssh-reused": warm + ["true"],
        }
        records = []
        for concurrency in (1, 4):
            for name, argv in commands.items():
                def once(_):
                    start = time.monotonic()
                    result = self.run_cmd(argv)
                    if name == "okdev-exec":
                        row = json.loads(result.stdout)[0]
                        self.assertEqual((row["exit"], row["status"]), (0, "responded"))
                    else:
                        self.assertEqual(result.stdout, b"")
                    return time.monotonic() - start
                start = time.monotonic()
                with ThreadPoolExecutor(max_workers=concurrency) as pool:
                    durations = list(pool.map(once, range(count)))
                elapsed = time.monotonic() - start
                records.append({"transport": name, "concurrency": concurrency, "runs": count,
                                "seconds": durations, "wallSeconds": elapsed, "commandsPerSecond": count / elapsed})
        print("EXEC_BENCH_RESULT=" + json.dumps({"platform": platform.platform(), "records": records}), flush=True)


if __name__ == "__main__":
    unittest.main()
