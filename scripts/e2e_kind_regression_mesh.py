"""Real Kind mesh receiver disconnect, convergence gate and recovery (#274)."""
import json
import signal
import socket
import subprocess
import time
import unittest
from urllib.error import URLError
from urllib.request import Request, urlopen
import xml.etree.ElementTree as ET

from e2e_kind_support import KindRegression


class Mesh(KindRegression):
    def status(self, base):
        return json.loads(self.run_cmd(base + ["sync", "status", "--output", "json"]).stdout)

    def test_disconnected_receiver_blocks_completion_and_execution(self):
        config, data = self.config("mesh", replicas=2,
                                   hooks={"postSync": "test -f /workspace/seed && echo verified > /tmp/mesh-hook"})
        self.up(config, "mesh")
        base = self.base(config, "mesh")
        initial = self.status(base)
        self.assertTrue(initial["converged"], initial)
        coverage = initial["folders"][0]["receivers"]
        self.assertEqual((coverage["expected"], coverage["connected"], coverage["converged"]), (2, 2, 2))
        self.assertEqual(coverage["scope"], "target-and-private-workspace-mesh")
        hub, worker = [entry["pod"] for entry in coverage["pods"]]
        for pod in (hub, worker):
            self.assertEqual(self.remote(pod, "cat", "/tmp/mesh-hook").stdout, b"verified\n")

        xml = self.run_cmd(["kubectl", "-n", self.namespace, "exec", worker, "-c", "okdev-sidecar", "--",
                            "sh", "-c", "cat /var/syncthing/config.xml 2>/dev/null || cat /var/syncthing/config/config.xml"]).stdout
        key = ET.fromstring(xml).findtext("gui/apikey")
        self.assertTrue(key, "missing isolated sidecar API key")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        forward = self.background(["kubectl", "-n", self.namespace, "port-forward", "pod/" + worker,
                                   f"{port}:8384", "--address", "127.0.0.1"],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        def api(path, method="GET"):
            request = Request(f"http://127.0.0.1:{port}/rest/" + path,
                              data=b"" if method == "POST" else None,
                              headers={"X-API-Key": key}, method=method)
            with urlopen(request, timeout=5) as response:
                body = response.read()
            return json.loads(body) if body else None

        deadline = time.monotonic() + 15
        while True:
            try:
                own_id = api("system/status")["myID"]
                break
            except URLError:
                self.assertIsNone(forward.poll(), "sidecar port-forward stopped")
                self.assertLess(time.monotonic(), deadline, "sidecar API did not become reachable")
                time.sleep(.1)
        peers = [entry["deviceID"] for entry in api("config/devices") if entry["deviceID"] != own_id]
        self.assertEqual(len(peers), 1, "mesh worker should connect only to its hub")
        api("system/pause?device=" + peers[0], "POST")
        (data / "after-disconnect").write_bytes(b"mesh latest revision\x00\xff")
        blocked = self.run_cmd(base + ["sync", "wait", "--timeout", "5s"], check=False)
        self.assertNotEqual(blocked.returncode, 0)
        self.assertIn(worker.encode(), blocked.stdout + blocked.stderr)
        pending = self.status(base)
        self.assertFalse(pending["converged"], pending)
        receivers = pending["folders"][0]["receivers"]
        self.assertEqual(receivers["expected"], 2)
        self.assertEqual(receivers["connected"], 1)
        self.assertIn(worker, receivers["pending"])

        guarded = self.background(base + ["exec", "--require-sync", "--", "touch", "/tmp/must-not-run"],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        time.sleep(3)
        self.assertIsNone(guarded.poll(), "require-sync returned before receiver recovered")
        for pod in (hub, worker):
            self.remote(pod, "test", "!", "-e", "/tmp/must-not-run")
        guarded.send_signal(signal.SIGINT)
        guarded.wait(timeout=15)
        self.assertNotEqual(guarded.returncode, 0)
        api("system/resume?device=" + peers[0], "POST")
        self.run_cmd(base + ["sync", "wait", "--timeout", "60s"])
        recovered = self.status(base)
        self.assertTrue(recovered["converged"], recovered)
        self.assertEqual(recovered["folders"][0]["receivers"]["converged"], 2)
        for pod in (hub, worker):
            self.assertEqual(self.remote(pod, "cat", "/workspace/after-disconnect").stdout,
                             b"mesh latest revision\x00\xff")


if __name__ == "__main__":
    unittest.main()
