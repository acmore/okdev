"""Checked SSH exec transport: identity, streams, authorization and no replay."""
from concurrent.futures import ThreadPoolExecutor
import json
import os
import select
import signal
import subprocess
import time
import unittest
from urllib.parse import urlsplit

from e2e_kind_regression_readiness import APIProxy
from e2e_kind_support import KindRegression


class ExecSSH(KindRegression):
    def test_checked_transport(self):
        config, _ = self.config("execssh", replicas=2)
        self.up(config, "execssh")
        base = self.base(config, "execssh")
        command = base + ["exec", "--transport=ssh"]
        pods = [p["metadata"]["name"] for p in self.pods("execssh")]
        # SSH authorization must already be provisioned on every selected pod.
        for pod in pods:
            self.run_cmd(base + ["target", "set", "--pod", pod])
            self.run_cmd(base + ["ssh", "--setup-key", "--cmd", "true"])
        first = command + ["--pod", pods[0]]
        cache = self.home / ".okdev/exec-ssh"
        for code in (0, 7, 255):
            result = self.run_cmd(first + ["--json", "--", "sh", "-c",
                                          f"printf 'out\\000put'; printf 'err\\000or' >&2; exit {code}"])
            row = json.loads(result.stdout)[0]
            self.assertEqual((row["exit"], row["stdout"], row["stderr"], row["status"]),
                             (code, "out\x00put", "err\x00or", "responded"))
        sockets = list(cache.glob("*.sock"))
        self.assertEqual(len(sockets), 1)
        socket = sockets[0]
        check = ["ssh", "-F", "/dev/null", "-S", socket, "-O", "check", "127.0.0.1"]
        master = self.run_cmd(check).stderr
        payload = bytes(range(256)) * 8192
        self.assertEqual(self.run_cmd(first + ["-i", "--", "cat"], data=payload).stdout, payload)
        quoted = "spaces ' quotes \" ; $(touch /tmp/should-not-exist)\nnext"
        self.assertEqual(self.run_cmd(first + ["--", "printf", "%s", quoted]).stdout.decode(), quoted)
        self.assertEqual(self.run_cmd(check).stderr, master)
        rows = json.loads(self.run_cmd(command + ["--all", "--json", "--", "hostname"]).stdout)
        self.assertEqual({r["pod"] for r in rows}, set(pods))
        for row in rows:
            self.assertEqual(row["stdout"].strip(), row["pod"], row)
        with ThreadPoolExecutor(max_workers=4) as pool:
            outputs = list(pool.map(lambda _: self.run_cmd(first + ["--", "hostname"]).stdout.strip().decode(), range(8)))
        self.assertEqual(outputs, [pods[0]] * 8)
        for pod in pods:
            self.run_cmd(base + ["target", "set", "--pod", pod])
            self.assertEqual(self.run_cmd(command + ["--", "hostname"]).stdout.strip().decode(), pod)
        denied = self.run_cmd(first + ["--container", "okdev-sidecar", "--", "touch", "/tmp/should-not-exist"], check=False)
        self.assertNotEqual(denied.returncode, 0)
        self.assertIn(b"configured dev container", denied.stderr)

        # A warm connection must not bypass changed owner metadata.
        self.run_cmd(["kubectl", "-n", self.namespace, "label", "pod", pods[0], "okdev.io/owner=someone-else", "--overwrite"])
        try:
            denied = self.run_cmd(first + ["--", "touch", "/tmp/should-not-exist"], check=False)
            self.assertNotEqual(denied.returncode, 0)
            self.assertIn(b"owned by", denied.stderr)
        finally:
            self.run_cmd(["kubectl", "-n", self.namespace, "label", "pod", pods[0], "okdev.io/owner=regression", "--overwrite"])
        self.assertNotEqual(self.run_cmd(["kubectl", "-n", self.namespace, "exec", pods[0], "-c", "dev", "--", "test", "-e", "/tmp/should-not-exist"], check=False).returncode, 0)

        # Streaming must not wait for stdin EOF; cancellation closes this channel.
        process = self.background(first + ["-i", "--", "cat"], stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        process.stdin.write(b"stream\n")
        process.stdin.flush()
        self.assertTrue(select.select([process.stdout], [], [], 15)[0])
        self.assertEqual(process.stdout.read(7), b"stream\n")
        process.send_signal(signal.SIGINT)
        process.wait(timeout=15)
        self.assertEqual(process.returncode, 69)
        diagnostic = self.background(first + ["--", "sh", "-c", "printf ready >&2; sleep 30"],
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertTrue(select.select([diagnostic.stderr], [], [], 15)[0], "stderr waits for command completion")
        self.assertEqual(diagnostic.stderr.read(5), b"ready")
        diagnostic.send_signal(signal.SIGINT)
        diagnostic.wait(timeout=15)
        self.assertEqual(diagnostic.returncode, 69)
        timed = json.loads(self.run_cmd(first + ["--json", "--timeout", "1s", "--", "sleep", "20"]).stdout)[0]
        self.assertEqual(timed["status"], "timeout")
        self.assertEqual(self.run_cmd(first + ["--", "hostname"]).stdout.strip().decode(), pods[0])

        script = self.root / "payload.sh"
        script.write_text("#!/bin/sh\nprintf '%s' \"$1\"\nprintf script-error >&2\nexit 7\n")
        row = json.loads(self.run_cmd(first + ["--json", "--script", script, "--", quoted]).stdout)[0]
        self.assertEqual((row["exit"], row["stdout"], row["stderr"]), (7, quoted, "script-error"))
        grouped = self.run_cmd(command + ["--group", ",".join(pods), "--no-prefix", "--", "hostname"])
        self.assertEqual(set(grouped.stdout.decode().splitlines()), set(pods))
        self.run_cmd(first + ["--detach", "--", "sh", "-c", "echo detached > /tmp/ssh-detached"])
        deadline = time.monotonic() + 10
        while self.run_cmd(["kubectl", "-n", self.namespace, "exec", pods[0], "-c", "dev", "--", "test", "-f", "/tmp/ssh-detached"], check=False).returncode:
            self.assertLess(time.monotonic(), deadline)
            time.sleep(.1)

        if os.environ.get("EXEC_BENCH_RUNS"):
            self.benchmark(base, pods[0])
        self.disconnect_after_delivery(base, config, pods[0])
        self.run_cmd(base + ["down", "--yes"])
        self.assertEqual(list(cache.glob("*.sock")), [])
        self.assertEqual(list(cache.glob("*.sock.json")), [])

    def test_rbac_revocation_invalidates_warm_access(self):
        config, _ = self.config("sshauth")
        self.up(config, "sshauth")
        pod = self.pods("sshauth")[0]["metadata"]["name"]
        self.run_cmd(["kubectl", "-n", self.namespace, "create", "serviceaccount", "exec-client"])
        role = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
                "metadata": {"name": "exec-client", "namespace": self.namespace},
                "rules": [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list", "watch"]},
                          {"apiGroups": [""], "resources": ["pods/exec"], "verbs": ["get", "create"]},
                          {"apiGroups": [""], "resources": ["pods/portforward"], "verbs": ["get", "create"]}]}
        self.run_cmd(["kubectl", "apply", "-f", "-"], data=json.dumps(role).encode())
        self.run_cmd(["kubectl", "-n", self.namespace, "create", "rolebinding", "exec-client",
                      "--role", "exec-client", "--serviceaccount", self.namespace + ":exec-client"])
        token = self.run_cmd(["kubectl", "-n", self.namespace, "create", "token", "exec-client"]).stdout.decode().strip()
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        for user in raw["users"]:
            user["user"] = {"token": token}
        restricted = self.root / "restricted-kubeconfig"
        restricted.write_text(json.dumps(raw))
        original = self.env["KUBECONFIG"]
        command = self.base(config, "sshauth") + ["exec", "--transport=ssh", "--pod", pod]
        try:
            self.env["KUBECONFIG"] = str(restricted)
            self.run_cmd(command + ["--", "true"])
            sockets = list((self.home / ".okdev/exec-ssh").glob("*.sock"))
            self.assertEqual(len(sockets), 1)
            check = ["ssh", "-F", "/dev/null", "-S", sockets[0], "-O", "check", "127.0.0.1"]
            master = self.run_cmd(check).stderr
            for revoked, verb in (("exec", "get"), ("exec", "create"), ("portforward", "get"), ("portforward", "create")):
                self.env["KUBECONFIG"] = original
                for rule in role["rules"][1:]:
                    rule["verbs"] = [v for v in ("get", "create") if not (rule["resources"] == ["pods/" + revoked] and v == verb)]
                self.run_cmd(["kubectl", "apply", "-f", "-"], data=json.dumps(role).encode())
                self.env["KUBECONFIG"] = str(restricted)
                denied = self.run_cmd(command + ["--", "touch", "/tmp/unauthorized-command"], check=False)
                self.assertNotEqual(denied.returncode, 0)
                self.assertIn(("requires verified " + verb + " access to pods/" + revoked).encode(), denied.stderr)
                self.assertEqual(self.run_cmd(check).stderr, master)
        finally:
            self.env["KUBECONFIG"] = original
        self.assertNotEqual(self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--",
                                          "test", "-e", "/tmp/unauthorized-command"], check=False).returncode, 0)

    def test_same_name_pod_replacement_and_container_identity(self):
        config, _ = self.config("sshreplace")
        self.up(config, "sshreplace")
        base = self.base(config, "sshreplace")
        command = base + ["exec", "--transport=ssh"]
        old = self.pods("sshreplace")[0]
        pod = old["metadata"]["name"]
        self.assertEqual(self.run_cmd(command + ["--", "hostname"]).stdout.strip().decode(), pod)
        old_socket = next((self.home / ".okdev/exec-ssh").glob("*.sock"))
        # Point the config at a real sidecar. Shared pod networking must not let
        # this execute in dev despite the claimed container selection.
        data = json.loads(config.read_text())
        other = next(c["name"] for c in old["spec"]["containers"] if c["name"] != "dev")
        data["spec"]["workload"]["attach"] = {"container": other}
        alternate = config.parent / "wrong-container.json"
        alternate.write_text(json.dumps(data))
        rejected = self.run_cmd(self.base(alternate, "sshreplace") + ["exec", "--transport=ssh", "--json", "--require-all", "--", "touch", "/tmp/wrong-container"], check=False)
        self.assertNotEqual(rejected.returncode, 0)
        row = json.loads(rejected.stdout)[0]
        self.assertIn("SSH server does not belong to the selected dev container", row["error"])
        self.assertNotEqual(self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--", "test", "-e", "/tmp/wrong-container"], check=False).returncode, 0)
        self.run_cmd(["kubectl", "-n", self.namespace, "delete", "pod", pod, "--wait=true"])
        self.up(config, "sshreplace")
        new = self.pods("sshreplace")[0]
        self.assertEqual(new["metadata"]["name"], pod)
        self.assertNotEqual(new["metadata"]["uid"], old["metadata"]["uid"])
        self.assertEqual(self.run_cmd(command + ["--", "hostname"]).stdout.strip().decode(), pod)
        sockets = list((self.home / ".okdev/exec-ssh").glob("*.sock"))
        self.assertEqual(len(sockets), 1)
        self.assertNotEqual(sockets[0], old_socket)

    def disconnect_after_delivery(self, base, config, pod):
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
        first = base + ["exec", "--transport=ssh", "--pod", pod]
        self.run_cmd(first + ["--", "true"])
        process = self.background(first + ["--json", "--require-all", "--", "sh", "-c",
                                           "echo once >> /tmp/ssh-delivered; sleep 30"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        plain = self.background(first + ["--", "sh", "-c", "echo once >> /tmp/ssh-delivered-text; sleep 30"],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.env["KUBECONFIG"] = original
        deadline = time.monotonic() + 15
        while self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--", "sh", "-c",
                           "test -f /tmp/ssh-delivered && test -f /tmp/ssh-delivered-text"], check=False).returncode:
            self.assertIsNone(process.poll())
            self.assertLess(time.monotonic(), deadline)
            time.sleep(.1)
        self.assertGreater(proxy.disconnect(), 0)
        stdout, stderr = process.communicate(timeout=20)
        self.assertNotEqual(process.returncode, 0, stderr)
        row = json.loads(stdout)[0]
        self.assertEqual((row["status"], row["exit"]), ("error", -1))
        self.assertIn("completion unknown (not replayed)", row["error"])
        self.assertEqual(self.remote(pod, "cat", "/tmp/ssh-delivered").stdout, b"once\n")
        _, plain_error = plain.communicate(timeout=20)
        self.assertEqual(plain.returncode, 69, plain_error)
        self.assertIn(b"completion unknown (not replayed)", plain_error)
        self.assertEqual(self.remote(pod, "cat", "/tmp/ssh-delivered-text").stdout, b"once\n")

    def benchmark(self, base, pod):
        count = int(os.environ["EXEC_BENCH_RUNS"])
        records = []
        for concurrency in (1, 4):
            for transport in ("kubernetes", "ssh"):
                def once(_):
                    start = time.monotonic()
                    row = json.loads(self.run_cmd(base + ["exec", "--transport", transport, "--json", "--pod", pod, "--", "true"]).stdout)[0]
                    self.assertEqual((row["exit"], row["status"]), (0, "responded"))
                    return time.monotonic() - start
                start = time.monotonic()
                with ThreadPoolExecutor(max_workers=concurrency) as pool:
                    durations = list(pool.map(once, range(count)))
                elapsed = time.monotonic() - start
                records.append(dict(transport=transport, concurrency=concurrency, runs=count,
                                    seconds=durations, wallSeconds=elapsed, commandsPerSecond=count / elapsed))
        print("EXEC_SSH_BENCH_RESULT=" + json.dumps(records), flush=True)


if __name__ == "__main__":
    unittest.main()
