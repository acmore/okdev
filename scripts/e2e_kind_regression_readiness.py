"""Kind regressions for readiness reconnect and timeout continuation (#281/#287)."""
import json
from pathlib import Path
import select
import shlex
import socket
import socketserver
import threading
import time
import unittest
from urllib.parse import urlsplit

from e2e_kind_support import KindRegression


class APIProxy(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, upstream):
        self.upstream = upstream
        self.active = set()
        self.lock = threading.Lock()
        super().__init__(("127.0.0.1", 0), APIRelay)
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def disconnect(self):
        with self.lock:
            connections = list(self.active)
        for pair in connections:
            for stream in pair:
                try:
                    stream.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
        return len(connections)

    def close(self):
        self.disconnect()
        self.shutdown()
        self.server_close()
        self.thread.join(timeout=5)


class APIRelay(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            remote = socket.create_connection(self.server.upstream, timeout=5)
        except OSError:
            return
        pair = (self.request, remote)
        with self.server.lock:
            self.server.active.add(pair)
        try:
            # Opaque TCP relay: TLS client authentication and every Kubernetes
            # protocol, including WebSocket exec/port-forward, stay end-to-end.
            while True:
                ready, _, _ = select.select(pair, [], [], 5)
                for stream in ready:
                    data = stream.recv(65536)
                    if not data:
                        return
                    (remote if stream is self.request else self.request).sendall(data)
        except (OSError, ValueError):
            pass
        finally:
            remote.close()
            with self.server.lock:
                self.server.active.discard(pair)


class Readiness(KindRegression):
    def release_gate(self, session):
        pod = self.pods(session)[0]
        name = pod["metadata"]["name"]
        self.run_cmd(["kubectl", "-n", self.namespace, "patch", "pod", name, "--type=json",
                      "-p", '[{"op":"remove","path":"/spec/schedulingGates"}]'])
        self.run_cmd(["kubectl", "-n", self.namespace, "wait", "--for=condition=Ready",
                      "pod/" + name, "--timeout=90s"])
        return pod

    def test_timeout_resumes_same_pod_and_setup_once(self):
        config, _ = self.config("resume", gates=True, hooks={"postCreate": "echo setup >> /tmp/setup-marker"})
        timed_out = self.run_cmd(self.base(config, "resume") + ["up", "--no-tmux", "--wait-timeout", "1s"], check=False)
        self.assertNotEqual(timed_out.returncode, 0)
        text = (timed_out.stdout + timed_out.stderr).decode()
        self.assertIn("workload remains submitted", text)
        self.assertIn("no background watcher", text)
        before = self.release_gate("resume")
        self.remote(before["metadata"]["name"], "test", "!", "-e", "/tmp/setup-marker")
        command = next(line.strip() for line in text.splitlines()
                       if line.strip().startswith("okdev ") and " up --wait-timeout " in line)
        argv = shlex.split(command)
        argv[0] = self.binary
        self.run_cmd(argv)
        after = self.pods("resume")[0]
        self.assertEqual(after["metadata"]["uid"], before["metadata"]["uid"])
        self.assertEqual(self.remote(after["metadata"]["name"], "cat", "/tmp/setup-marker").stdout, b"setup\n")

    def test_readiness_reconnects_after_real_api_connection_loss(self):
        config, _ = self.config("reconnect", gates=True, hooks={"postCreate": "echo setup >> /tmp/setup-marker"})
        original_path = self.env["KUBECONFIG"]
        original = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        raw = json.loads(json.dumps(original))
        cluster = raw["clusters"][0]["cluster"]
        upstream = urlsplit(cluster["server"])
        proxy = APIProxy((upstream.hostname, upstream.port or 443))
        cluster["server"] = f"https://127.0.0.1:{proxy.server_address[1]}"
        cluster["tls-server-name"] = upstream.hostname
        proxy_path = self.root / "proxy-kubeconfig"
        proxy_path.write_text(json.dumps(raw))
        self.env["KUBECONFIG"] = str(proxy_path)
        log = self.root / "up.log"
        process = None
        try:
            with log.open("wb") as output:
                process = self.background(self.base(config, "reconnect") +
                                          ["up", "--no-tmux", "--wait-timeout", "90s"],
                                          stdout=output, stderr=output)
                deadline = time.monotonic() + 30
                while "== Wait ==" not in log.read_text():
                    self.assertIsNone(process.poll(), log.read_text())
                    self.assertLess(time.monotonic(), deadline, log.read_text())
                    time.sleep(.1)
                time.sleep(.3)
                self.assertGreater(proxy.disconnect(), 0, "no live API connection was interrupted")
                deadline = time.monotonic() + 15
                while "retrying readiness" not in log.read_text():
                    self.assertIsNone(process.poll(), log.read_text())
                    self.assertLess(time.monotonic(), deadline, log.read_text())
                    time.sleep(.1)
                before = self.release_gate("reconnect")
                process.wait(timeout=120)
                self.assertEqual(process.returncode, 0, log.read_text())
                self.assertEqual(self.pods("reconnect")[0]["metadata"]["uid"], before["metadata"]["uid"])
                self.assertEqual(self.remote(before["metadata"]["name"], "cat", "/tmp/setup-marker").stdout, b"setup\n")
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                process.wait(timeout=10)
            # Keep the saved kubeconfig usable for managed children and down.
            proxy_path.write_text(json.dumps(original))
            self.env["KUBECONFIG"] = original_path
            proxy.close()


if __name__ == "__main__":
    unittest.main()
