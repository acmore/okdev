"""Isolated real-Kind fixtures shared by CLI regression scenarios."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import uuid

REPO = Path(__file__).resolve().parent.parent


class KindRegression(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="okrg-", suffix="x", dir="/tmp"))
        self.home = self.root / "home"
        self.home.mkdir()
        self.cluster = os.environ.get("CLUSTER_NAME", "okdev-e2e")
        self.namespace = "okdev-regression-" + uuid.uuid4().hex[:10]
        self.created_namespace = False
        self.sessions = []
        self.processes = []
        self.env = dict(os.environ, HOME=str(self.home), KUBECONFIG=str(self.root / "kubeconfig"))
        self.env.pop("OKDEV_CONFIG", None)
        self.env["OKDEV_OWNER"] = "regression"
        self.binary = str(Path(os.environ.get("OKDEV_BIN", REPO / "bin/okdev")).resolve())
        self.addCleanup(self.cleanup)
        self.assertTrue(Path(self.binary).is_file(), "build the CLI or set OKDEV_BIN")
        cache = Path.home() / ".okdev/bin/syncthing"
        if cache.exists():
            shutil.copytree(cache, self.home / ".okdev/bin/syncthing")
        self.run_cmd(["kind", "export", "kubeconfig", "--name", self.cluster,
                      "--kubeconfig", self.env["KUBECONFIG"]])
        self.run_cmd(["git", "init", "-q"])
        self.run_cmd(["kubectl", "create", "namespace", self.namespace])
        self.created_namespace = True

    def run_cmd(self, argv, *, data=b"", check=True, timeout=240, cwd=None):
        result = subprocess.run(list(map(str, argv)), input=data, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=self.env, cwd=cwd or self.root,
                                timeout=timeout)
        if check and result.returncode:
            self.fail(f"command exited {result.returncode}: {argv}\n"
                      f"stdout={result.stdout.decode(errors='replace')}\n"
                      f"stderr={result.stderr.decode(errors='replace')}")
        return result

    def cli(self, *args, **kwargs):
        return self.run_cmd(self.base() + list(args), **kwargs)

    def base(self, config=None, session=None):
        args = [self.binary, "--context", "kind-" + self.cluster,
                "--namespace", self.namespace, "--owner", "regression"]
        if config is not None:
            args += ["--config", str(config)]
        if session is not None:
            args += ["--session", session]
        return args

    def config(self, name, *, relative=None, replicas=1, gates=False, hooks=None):
        path = self.root / (relative or f".okdev/{name}/okdev.yaml")
        path.parent.mkdir(parents=True, exist_ok=True)
        data = self.root / "data" / name
        data.mkdir(parents=True)
        (data / "seed").write_text("initial content\n")
        pod = {"containers": [{"name": "dev", "image": "ubuntu:22.04",
                               "command": ["sleep", "infinity"]}]}
        if gates:
            pod["schedulingGates"] = [{"name": "okdev.io/regression-wait"}]
        manifest = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name}, "spec": pod}
        workload = {"type": "pod", "manifestPath": "workload.json", "devContainerName": "dev"}
        if replicas > 1:
            manifest = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name},
                        "spec": {"replicas": replicas, "selector": {"matchLabels": {"app": name}},
                                 "template": {"metadata": {"labels": {"app": name}}, "spec": pod}}}
            workload = {"type": "generic", "manifestPath": "workload.json",
                        "inject": [{"path": "spec.template"}], "attach": {"container": "dev"}}
        (path.parent / "workload.json").write_text(json.dumps(manifest))
        config = {"apiVersion": "okdev.io/v1alpha1", "kind": "DevEnvironment", "metadata": {"name": name},
                  "spec": {"namespace": self.namespace, "session": {"defaultNameTemplate": name},
                           "workload": workload,
                           "sidecar": {"image": os.environ.get("SIDECAR_IMAGE", "okdev-sidecar:v0.0.0-e2e")},
                           "ssh": {"user": "root", "shell": "/bin/bash", "persistentSession": False},
                           "sync": {"paths": [{"local": str(data), "remote": "/workspace"}]}}}
        if hooks:
            config["spec"]["lifecycle"] = hooks
        path.write_text(json.dumps(config))
        self.sessions.append((path, name))
        return path, data

    def up(self, config, session):
        # The documented sshd startup race is retried only for this exact error.
        result = self.run_cmd(self.base(config, session) + ["up", "--no-tmux", "--wait-timeout", "2m"], check=False)
        if result.returncode and b"wait for sshd ready: command terminated with exit code 1" in result.stderr:
            result = self.run_cmd(self.base(config, session) + ["up", "--no-tmux", "--wait-timeout", "2m"], check=False)
        self.assertEqual(result.returncode, 0, result.stdout.decode(errors="replace") + result.stderr.decode(errors="replace"))
        return result

    def pods(self, session):
        result = self.run_cmd(["kubectl", "-n", self.namespace, "get", "pods", "-l",
                               "okdev.io/session=" + session, "-o", "json"])
        return json.loads(result.stdout)["items"]

    def remote(self, pod, *args):
        return self.run_cmd(["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--", *args])

    def background(self, argv, **kwargs):
        process = subprocess.Popen(argv, env=self.env, cwd=self.root, **kwargs)
        self.processes.append(process)
        return process

    def cleanup(self):
        failures = []
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            for stream in (process.stdin, process.stdout, process.stderr):
                if stream:
                    stream.close()
        for config, session in reversed(self.sessions):
            try:
                result = self.run_cmd(self.base(config, session) + ["down", "--yes"], check=False, timeout=90)
                if result.returncode:
                    failures.append(f"down {session}: {result.stderr.decode(errors='replace')}")
            except subprocess.TimeoutExpired:
                failures.append(f"down {session} timed out")
        if self.created_namespace:
            result = self.run_cmd(["kubectl", "delete", "namespace", self.namespace, "--wait=true", "--timeout=90s"],
                                  check=False, timeout=100)
            if result.returncode:
                failures.append(f"namespace {self.namespace} was not deleted")
        if failures:
            self.fail(f"cleanup incomplete; local state retained at {self.root}: " + "; ".join(failures))
        shutil.rmtree(self.root)
