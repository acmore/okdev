"""Real Kind indexed-revision, empty-entry, exclusion and multi-path sync checks."""
import hashlib
import json
import unittest

from e2e_kind_support import KindRegression


class SyncRevision(KindRegression):
    def test_current_revision_and_all_local_paths_reach_remote(self):
        config, data = self.config("revision")
        extra = data / "extra"
        extra.mkdir()
        (extra / "seed").write_text("extra seed\n")
        (data / ".stignore").write_text("ignored/**\n")
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"].append({"local": str(extra), "remote": "/var/okdev/data"})
        config.write_text(json.dumps(raw))
        self.up(config, "revision")
        pod = self.pods("revision")[0]["metadata"]["name"]
        base = self.base(config, "revision")
        (data / "nested").mkdir()
        (data / "empty-dir").mkdir()
        (data / "empty-file").touch()
        (data / "ignored").mkdir()
        (data / "ignored/secret").write_text("excluded\n")
        (extra / "nested").mkdir()
        (extra / "nested/file").write_bytes(b"additional mapping\x00\xff")
        for payload in (bytes(range(256)) * 4096, bytes(reversed(range(256))) * 4096):
            # Equal byte/file counts do not imply that the latest revision arrived.
            (data / "nested/file").write_bytes(payload)
            self.run_cmd(base + ["sync", "wait", "--timeout", "60s"])
            digest = self.remote(pod, "sha256sum", "/workspace/nested/file").stdout.split()[0]
            self.assertEqual(digest.decode(), hashlib.sha256(payload).hexdigest())
            self.remote(pod, "test", "-d", "/workspace/empty-dir")
            self.remote(pod, "test", "-f", "/workspace/empty-file")
            self.assertEqual(self.remote(pod, "cat", "/workspace/empty-file").stdout, b"")
            self.remote(pod, "test", "!", "-e", "/workspace/ignored/secret")
            self.remote(pod, "test", "!", "-e", "/workspace/extra/nested/file")
            self.assertEqual(self.remote(pod, "cat", "/var/okdev/data/nested/file").stdout,
                             b"additional mapping\x00\xff")
        (data / "nested/file").unlink()
        (data / "empty-file").unlink()
        self.run_cmd(base + ["sync", "wait", "--timeout", "60s"])
        self.remote(pod, "test", "!", "-e", "/workspace/nested/file")
        self.remote(pod, "test", "!", "-e", "/workspace/empty-file")
        status = json.loads(self.run_cmd(base + ["sync", "status", "--output", "json"]).stdout)
        self.assertTrue(status["converged"], status)
        self.assertEqual(len(status["folders"]), 2)


if __name__ == "__main__":
    unittest.main()
