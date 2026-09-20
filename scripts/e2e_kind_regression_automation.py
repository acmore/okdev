"""Execute the documented automation recipes on real Kind pods (#293)."""
import json
import re
import shlex
import socket
import time
import unittest

from e2e_kind_support import KindRegression, REPO


class Automation(KindRegression):
    def recipe(self, name):
        text = (REPO / "docs/automation.md").read_text()
        match = re.search(r"<!-- kind-recipe: " + re.escape(name) + r" -->\n```bash\n(.*?)\n```", text, re.S)
        self.assertIsNotNone(match, "missing documented recipe: " + name)
        return match.group(1)

    def wrapper(self, config, *, replace=False):
        command = "exec " if replace else "command "
        return 'okdev() { ' + command + shlex.join(self.base(config, "recipes")) + ' "$@"; }\n'

    def test_documented_recipes_and_failure_checks(self):
        config, data = self.config("recipes", replicas=2)
        content = b"print('verified content')\n"
        (data / "train.py").write_bytes(content)
        (self.root / "train.py").write_bytes(content)
        self.up(config, "recipes")
        wrapper = self.wrapper(config)
        verify = wrapper + self.recipe("verify")
        self.run_cmd(["bash", "-c", verify])
        (self.root / "train.py").write_bytes(b"different local reference\n")
        mismatch = self.run_cmd(["bash", "-c", verify], check=False)
        self.assertNotEqual(mismatch.returncode, 0)
        self.assertIn(b"Content mismatch", mismatch.stderr)
        (self.root / "train.py").write_bytes(content)

        # A remote nonzero exit is still a response: consumers must inspect it.
        remote_failure = self.run_cmd(self.base(config, "recipes") +
                                      ["exec", "--all", "--json", "--require-all", "--", "sh", "-c", "exit 7"])
        rows = json.loads(remote_failure.stdout)
        self.assertEqual([(row["status"], row["exit"]) for row in rows], [("responded", 7)] * 2)
        job_script = wrapper + self.recipe("job") + '\nprintf "recipe_job_id=%s\\n" "$job_id"\n'
        first = self.run_cmd(["bash", "-c", job_script])
        first_id = re.search(rb"recipe_job_id=(\S+)", first.stdout).group(1)
        launched = self.run_cmd(self.base(config, "recipes") + ["exec", "--detach", "--", "printf", "NO_MARKER\\n"])
        new_id = re.search(rb"job_id=([^ ]+)", launched.stdout).group(1)
        self.assertNotEqual(first_id, new_id)
        absent = self.run_cmd(self.base(config, "recipes") + ["jobs", "wait", new_id.decode(), "--grep", "^READY$"], check=False)
        self.assertNotEqual(absent.returncode, 0, "an earlier job's marker satisfied the new job wait")
        self.env["HOSTNAME"] = "local-host-must-not-expand"
        quoted = self.run_cmd(["bash", "-c", wrapper + self.recipe("quoting")])
        pod_names = [pod["metadata"]["name"].encode() for pod in self.pods("recipes")]
        self.assertIn(quoted.stdout.strip(), pod_names)

        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        # Reuse the real SSH service as the remote endpoint; the recipe's
        # documented port variables allow testing without installing a web app.
        log = self.root / "forward.log"
        with log.open("wb") as output:
            script = self.wrapper(config, replace=True) + f"local_port={port}\nremote_port=2222\n" + self.recipe("forward")
            forward = self.background(["bash", "-c", script], stdout=output, stderr=output)
            deadline = time.monotonic() + 20
            while True:
                self.assertIsNone(forward.poll(), log.read_text())
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=5) as connection:
                        self.assertTrue(connection.recv(256).startswith(b"SSH-2.0-"))
                    break
                except ConnectionRefusedError:
                    self.assertLess(time.monotonic(), deadline, log.read_text())
                    time.sleep(.1)
            forward.terminate()
            forward.wait(timeout=10)
        self.assertIn(f"127.0.0.1:{port}".encode(), log.read_bytes())

        kubeconfig = self.root / "kubeconfig"
        original = kubeconfig.read_text()
        raw = json.loads(self.run_cmd(["kubectl", "config", "view", "--raw", "-o", "json"]).stdout)
        raw["clusters"][0]["cluster"]["server"] = "https://127.0.0.1:1"
        try:
            kubeconfig.write_text(json.dumps(raw))
            unavailable = self.run_cmd(["bash", "-c", verify], check=False, timeout=30)
            self.assertEqual(unavailable.returncode, 78)
            self.assertEqual(unavailable.stdout, b"")
            self.assertNotIn(b"JSONDecodeError", unavailable.stderr)
        finally:
            kubeconfig.write_text(original)


if __name__ == "__main__":
    unittest.main()
