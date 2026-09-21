"""Copy counters and an opt-in reproducible Kind throughput investigation."""
import gzip
import hashlib
import json
import os
import platform
import subprocess
import threading
import time
import unittest

from e2e_kind_support import KindRegression


class CopyStats(KindRegression):
    def stats(self, result):
        records = [json.loads(line) for line in result.stderr.splitlines() if line.startswith(b'{"event":"cp_stats"')]
        self.assertEqual(len(records), 1, result.stderr.decode())
        self.assertNotIn(b"cp_stats", result.stdout)
        self.assertNotIn(b"\x1b[", result.stdout)
        return records[0]

    def test_counts_reuse_failures_and_hashes(self):
        config, _ = self.config("cpstats", replicas=2)
        raw = json.loads(config.read_text())
        raw["spec"]["sync"]["paths"] = []
        config.write_text(json.dumps(raw))
        self.up(config, "cpstats")
        base = self.base(config, "cpstats")
        source = self.root / "payload"
        payload = os.urandom(2 * 1024 * 1024)
        source.write_bytes(payload)
        digest = hashlib.sha256(payload).hexdigest()
        default = self.run_cmd(base + ["cp", str(source), ":/tmp/default"])
        self.assertNotIn(b"cp_stats", default.stdout + default.stderr)
        uploaded = self.run_cmd(base + ["cp", "--all", "--stats", str(source), ":/tmp/payload"])
        stats = self.stats(uploaded)
        self.assertEqual((stats["streamBytes"], stats["reusedBytes"], stats["targets"], stats["success"]),
                         (2 * len(payload), 0, 2, True))
        self.assertGreater(stats["elapsedSeconds"], 0)
        self.assertGreater(stats["averageBytesPerSecond"], 0)
        for pod in self.pods("cpstats"):
            self.assertEqual(self.remote(pod["metadata"]["name"], "sha256sum", "/tmp/payload").stdout.split()[0].decode(), digest)
        target = self.root / "download"
        downloaded = self.run_cmd(base + ["cp", "--stats", ":/tmp/payload", str(target)])
        self.assertEqual(self.stats(downloaded)["streamBytes"], len(payload))
        self.assertEqual(hashlib.sha256(target.read_bytes()).hexdigest(), digest)
        reused = self.run_cmd(base + ["cp", "--stats", ":/tmp/payload", str(target)])
        stats = self.stats(reused)
        self.assertEqual((stats["streamBytes"], stats["reusedBytes"]), (0, len(payload)))
        verified = self.run_cmd(base + ["cp", "--stats", "--verify", ":/tmp/payload", str(target)])
        self.assertEqual(self.stats(verified)["reusedBytes"], len(payload))
        target.write_bytes(payload[:len(payload) // 2])
        resumed = self.run_cmd(base + ["cp", "--stats", ":/tmp/payload", str(target)])
        stats = self.stats(resumed)
        self.assertEqual((stats["streamBytes"], stats["reusedBytes"]), (len(payload) // 2, len(payload) // 2))
        self.assertEqual(hashlib.sha256(target.read_bytes()).hexdigest(), digest)
        failed = self.run_cmd(base + ["cp", "--stats", str(source), ":/proc/okdev-impossible"], check=False)
        self.assertNotEqual(failed.returncode, 0)
        self.assertFalse(self.stats(failed)["success"])
        if os.environ.get("CP_BENCH_MIB"):
            self.benchmark(base, source)

    def benchmark(self, base, source):
        size = int(os.environ["CP_BENCH_MIB"]) * 1024 * 1024
        repeats = int(os.environ.get("CP_BENCH_REPEATS", "3"))
        pod = json.loads(self.run_cmd(base + ["status", "--details", "--output", "json"]).stdout)["target"]["selectedPod"]
        records = []
        serial = 0

        def measure(name, argv, stdin=None):
            nonlocal serial
            serial += 1
            stdout_path, stderr_path = self.root / f"out-{serial}", self.root / f"err-{serial}"
            with stdout_path.open("wb") as out, stderr_path.open("wb") as err:
                start = time.monotonic()
                process = self.background(list(map(str, argv)), stdin=stdin, stdout=out, stderr=err)
                watchdog = threading.Timer(180, lambda: process.kill() if process.poll() is None else None)
                watchdog.start()
                try:
                    _, status, usage = os.wait4(process.pid, 0)
                    process.returncode = os.waitstatus_to_exitcode(status)
                finally:
                    watchdog.cancel()
                wall = time.monotonic() - start
            self.assertEqual(process.returncode, 0, stderr_path.read_text())
            result = subprocess.CompletedProcess(argv, process.returncode, stdout_path.read_bytes(), stderr_path.read_bytes())
            record = {"operation": name, "bytes": size, "wallSeconds": wall,
                      "MiBPerSecond": size / wall / 1048576,
                      "clientCPUSeconds": usage.ru_utime + usage.ru_stime,
                      "clientMaxRSSMiB": usage.ru_maxrss / (1048576 if platform.system() == "Darwin" else 1024)}
            if name.startswith("okdev"):
                record["stats"] = self.stats(result)
            records.append(record)
            return result

        for content in os.environ.get("CP_BENCH_CONTENTS", "random,repeated").split(","):
            self.assertIn(content, ("random", "repeated"))
            block = os.urandom(1024 * 1024) if content == "random" else b"okdev-benchmark\n" * 65536
            h = hashlib.sha256()
            with source.open("wb") as stream:
                remaining = size
                while remaining:
                    chunk = block[:min(remaining, len(block))]
                    stream.write(chunk)
                    h.update(chunk)
                    remaining -= len(chunk)
            compression = []
            for level in (1, 6):
                start = time.process_time()
                compressed = gzip.compress(block, compresslevel=level)
                compression.append({"level": level, "sampleBytes": len(block),
                                    "ratio": len(compressed) / len(block), "cpuSeconds": time.process_time() - start})
            for repeat in range(repeats):
                begin = len(records)
                measure("okdev-upload", base + ["cp", "--stats", str(source), ":/tmp/bench"])
                downloaded = self.root / "bench-down"
                downloaded.unlink(missing_ok=True)
                measure("okdev-download", base + ["cp", "--stats", ":/tmp/bench", str(downloaded)])
                with downloaded.open("rb") as stream:
                    self.assertEqual(hashlib.file_digest(stream, "sha256").hexdigest(), h.hexdigest())
                with source.open("rb") as stream:
                    measure("kubectl-stream-to-null", ["kubectl", "-n", self.namespace, "exec", "-i", pod, "-c", "dev", "--", "sh", "-c", "cat >/dev/null"], stream)
                measure("remote-cp-via-exec", ["kubectl", "-n", self.namespace, "exec", pod, "-c", "dev", "--", "cp", "/tmp/bench", "/tmp/bench-copy"])
                batch = self.root / "sftp-batch"
                batch.write_text(f'put "{source}" /tmp/bench-sftp\n')
                measure("sftp-upload", ["sftp", "-q", "-F", self.home / ".ssh/config", "-b", batch, "okdev-cpstats"])
                for path in ("/tmp/bench", "/tmp/bench-copy", "/tmp/bench-sftp"):
                    self.assertEqual(self.remote(pod, "sha256sum", path).stdout.split()[0].decode(), h.hexdigest())
                for record in records[begin:]:
                    record.update(content=content, repeat=repeat + 1)
            records.append({"content": content, "compressionSample": compression})
        print("CP_BENCH_RESULT=" + json.dumps({"platform": platform.platform(), "bytes": size,
              "repeats": repeats, "records": records}), flush=True)


if __name__ == "__main__":
    unittest.main()
