"""Real Kind exec stdin byte streaming and cancellation regressions (#290)."""
import select
import signal
import subprocess
import unittest

from e2e_kind_support import KindRegression


class Stdin(KindRegression):
    def test_foreground_stdin_bytes_exit_codes_and_open_pipe(self):
        config, _ = self.config("stdin")
        self.up(config, "stdin")
        base = self.base(config, "stdin") + ["exec"]
        pod = self.pods("stdin")[0]["metadata"]["name"]
        for payload in (b"", b"hello\n", bytes(range(256)) * 8192):
            with self.subTest(size=len(payload)):
                result = self.run_cmd(base + ["-i", "--", "cat"], data=payload)
                self.assertEqual(result.stdout, payload)
        result = self.run_cmd(base + ["--stdin", "--pod", pod, "--", "sh", "-c",
                                     "cat; printf diagnostic >&2; exit 7"], data=b"once", check=False)
        self.assertEqual(result.returncode, 7)
        self.assertEqual(result.stdout, b"once")
        self.assertIn(b"diagnostic", result.stderr)
        self.assertEqual(self.run_cmd(base + ["--", "cat"], data=b"ignored").stdout, b"")

        # Leave stdin open: output must arrive before EOF, and cancellation must
        # terminate the command without waiting for the input producer to close.
        process = self.background(base + ["-i", "--", "cat"], stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        process.stdin.write(b"streamed\n")
        process.stdin.flush()
        self.assertTrue(select.select([process.stdout], [], [], 15)[0], "no output before EOF")
        self.assertEqual(process.stdout.read(9), b"streamed\n")
        process.send_signal(signal.SIGINT)
        process.wait(timeout=15)
        self.assertEqual(process.returncode, 69)

        timed = self.background(base + ["-i", "--timeout", "1s", "--", "cat"],
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        timed.wait(timeout=15)
        self.assertEqual(timed.returncode, 69)
        for flags in (["--all"], ["--detach"], ["--json"], ["--pod", pod + ",other"]):
            rejected = self.background(base + ["-i", *flags, "--", "cat"],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            rejected.wait(timeout=10)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertIn(b"--stdin", rejected.stderr.read())


if __name__ == "__main__":
    unittest.main()
