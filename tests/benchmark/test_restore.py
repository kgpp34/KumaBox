"""Safety checks for benchmark-owned resource cleanup; no KVM required."""

import io
import tempfile
import unittest
from pathlib import Path

from restore import RestoreBenchmark


class FailingStop:
    def __init__(self):
        self.calls = []

    def call(self, *arguments):
        self.calls.append(arguments)
        if arguments[0] == "stop":
            raise RuntimeError("injected stop failure")


class RestoreBenchmarkTests(unittest.TestCase):
    def test_cleanup_continues_after_stop_failure_and_only_removes_owned_exports(self):
        with tempfile.TemporaryDirectory() as parent:
            owned = Path(parent) / "owned"
            owned.mkdir()
            (owned / "memory-range-0").write_bytes(b"memory")
            other = Path(parent) / "unrelated"
            other.write_bytes(b"keep")
            benchmark = object.__new__(RestoreBenchmark)
            benchmark.owner = FailingStop()
            benchmark.sandboxes = {"owned-sandbox"}
            benchmark.snapshots = {"owned-snapshot"}
            benchmark.exports = {owned}
            benchmark.raw = io.StringIO()
            failures = benchmark.cleanup()
            self.assertEqual(failures, ["injected stop failure"])
            self.assertEqual(benchmark.owner.calls, [
                ("stop", "owned-sandbox"), ("snapshot", "rm", "owned-snapshot"),
            ])
            self.assertFalse(owned.exists())
            self.assertEqual(other.read_bytes(), b"keep")
            self.assertTrue(benchmark.raw.closed)


if __name__ == "__main__":
    unittest.main()
