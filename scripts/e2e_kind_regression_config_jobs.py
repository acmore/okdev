"""Kind regressions for #278, #283, and #285 (PR #294)."""
import json
import re
import unittest

from e2e_kind_support import KindRegression


class ConfigAndJobs(KindRegression):
    def test_config_selection_and_job_log_bytes(self):
        first, _ = self.config("fresh-a", relative=".okdev/okdev.yaml")
        self.cli("--session", "fresh-a", "up", "--no-tmux", "--wait-timeout", "2m")
        before = self.pods("fresh-a")[0]["metadata"]["uid"]
        second, _ = self.config("fresh-b", replicas=2)
        self.run_cmd(self.base(second) + ["up", "--no-tmux", "--wait-timeout", "2m"])
        self.assertEqual(len(self.pods("fresh-b")), 2)
        self.assertEqual(self.pods("fresh-a")[0]["metadata"]["uid"], before)
        collision, _ = self.config("collision")
        raw = json.loads(collision.read_text())
        raw["spec"]["session"]["defaultNameTemplate"] = "fresh-a"
        collision.write_text(json.dumps(raw))
        rejected = self.run_cmd(self.base(collision) + ["up", "--dry-run"], check=False)
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn(b"config association", rejected.stdout + rejected.stderr)
        self.assertEqual(self.pods("fresh-a")[0]["metadata"]["uid"], before)

        for config, session, selector, script, expected in [
            (first, "fresh-a", [], r"printf 'log\000\377tail'", b"log\x00\xfftail"),
            (second, "fresh-b", ["--all"], "printf 'multi\\n'", b"multi\nmulti\n"),
        ]:
            base = self.base(config, session)
            launched = self.run_cmd(base + ["exec", *selector, "--detach", "--", "sh", "-c", script])
            job = re.search(rb"job_id=([^ ]+)", launched.stdout).group(1).decode()
            self.run_cmd(base + ["jobs", "wait", job])
            for follow in ([], ["--follow"]):
                raw = self.run_cmd(base + ["jobs", "logs", job, *follow, "--no-prefix"]).stdout
                self.assertEqual(raw, expected)
                default = self.run_cmd(base + ["jobs", "logs", job, *follow]).stdout
                if session == "fresh-a":
                    self.assertEqual(default, expected)
                else:
                    self.assertEqual(len(default.splitlines()), 2)
                    for line in default.splitlines():
                        self.assertRegex(line, rb"^\[[^]]+\] multi$")


if __name__ == "__main__":
    unittest.main()
