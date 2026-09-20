"""Real API interruption and Pod replacement during foreground forwarding."""
import json
import signal
import socket
import time
import unittest
from urllib.parse import urlsplit

from e2e_kind_regression_readiness import APIProxy
from e2e_kind_support import KindRegression


class ForwardRecovery(KindRegression):
    def wait_for(self, predicate, process, log, timeout=60):
        deadline = time.monotonic() + timeout
        while not predicate():
            self.assertIsNone(process.poll(), log.read_text())
            self.assertLess(time.monotonic(), deadline, log.read_text())
            time.sleep(.1)

    def test_setup_failure_stream_loss_replacement_and_cancellation(self):
        config, _ = self.config("forward", replicas=2)
        self.up(config, "forward")
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        upstream = urlsplit(raw["clusters"][0]["cluster"]["server"])
        live_upstream = (upstream.hostname, upstream.port or 443)
        proxy = APIProxy(("127.0.0.1", 1))
        self.addCleanup(proxy.close)
        raw["clusters"][0]["cluster"]["server"] = f"https://127.0.0.1:{proxy.server_address[1]}"
        raw["clusters"][0]["cluster"]["tls-server-name"] = upstream.hostname
        proxied_config = self.root / "forward-kubeconfig"
        proxied_config.write_text(json.dumps(raw))
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        log = self.root / "forward.log"
        original = self.env["KUBECONFIG"]
        with log.open("wb") as output:
            try:
                self.env["KUBECONFIG"] = str(proxied_config)
                process = self.background(self.base(config, "forward") +
                                          ["port-forward", "--address", "127.0.0.1", f"{port}:8384"],
                                          stdout=output, stderr=output)
            finally:
                self.env["KUBECONFIG"] = original
            self.wait_for(lambda: "retrying in" in log.read_text(), process, log)
            proxy.upstream = live_upstream

            def responds():
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=1) as connection:
                        connection.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                        return connection.recv(128).startswith(b"HTTP/1.1")
                except OSError:
                    return False

            self.wait_for(responds, process, log)
            retries = log.read_text().count("retrying in")
            proxy.upstream = ("127.0.0.1", 1)
            self.assertGreater(proxy.disconnect(), 0)
            self.wait_for(lambda: log.read_text().count("retrying in") > retries, process, log)
            self.assertIn("local listeners are closed", log.read_text())
            with socket.socket() as listener:
                listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                listener.bind(("127.0.0.1", port))
            proxy.upstream = live_upstream
            self.wait_for(responds, process, log)
            target = [line.split("pod=", 1)[1].split()[0] for line in log.read_text().splitlines()
                      if line.startswith("Forwarding to pod=")][-1]
            self.run_cmd(["kubectl", "-n", self.namespace, "delete", "pod", target, "--wait=true"])
            self.wait_for(lambda: any(line.startswith("Forwarding to pod=") and "pod=" + target + " " not in line
                                     for line in log.read_text().splitlines()), process, log)
            self.wait_for(responds, process, log)
            process.send_signal(signal.SIGINT)
            process.wait(timeout=10)
            with socket.socket() as listener:
                listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                listener.bind(("127.0.0.1", port))

    def test_dns_failure_remains_cancellable(self):
        config, _ = self.config("dns")
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        raw["clusters"][0]["cluster"]["server"] = "https://okdev-api-does-not-exist.invalid:443"
        (self.root / "dns-kubeconfig").write_text(json.dumps(raw))
        log = self.root / "dns.log"
        original = self.env["KUBECONFIG"]
        with log.open("wb") as output:
            try:
                self.env["KUBECONFIG"] = str(self.root / "dns-kubeconfig")
                process = self.background(self.base(config, "dns") + ["port-forward", "18080:8080"], stdout=output, stderr=output)
            finally:
                self.env["KUBECONFIG"] = original
            self.wait_for(lambda: "retrying in" in log.read_text(), process, log)
            process.send_signal(signal.SIGINT)
            process.wait(timeout=10)


if __name__ == "__main__":
    unittest.main()
