"""Contract tests for the benchmark's stateful setup and timed operations."""

import json
import tempfile
import threading
import unittest
from pathlib import Path

import lifecycle


class FakeKumaBox:
    def __init__(self):
        self.lock = threading.Lock()
        self.sandboxes = {}
        self.snapshots = {}
        self.probes = {}
        self.commands = []
        self.fail_next_run = False

    def call(self, *args, timeout=None):
        with self.lock:
            self.commands.append(args)
            if args[0] == "exec":
                name = args[1]
                if self.sandboxes[name] != "running":
                    raise lifecycle.CommandError("guest stopped")
                self.probes[name] += 1
                if self.probes[name] == 1:
                    raise lifecycle.CommandError("agent starting")
            elif args[0] == "stop":
                self.sandboxes[args[1]] = "stopped"
            elif args[0] == "rm":
                del self.sandboxes[args[1]]
            elif args[:2] == ("snapshot", "rm"):
                del self.snapshots[args[2]]
            elif args[0] == "logs":
                return "vmm test log", 1.0
        return "", 1.0

    def json(self, *args):
        with self.lock:
            self.commands.append(args)
            if args[0] == "run":
                if self.fail_next_run:
                    self.fail_next_run = False
                    raise lifecycle.CommandError("test setup failure")
                name = args[args.index("--name") + 1]
                self.sandboxes[name] = "running"
                self.probes[name] = 0
                return {"id": name, "state": "running"}, 1.0
            if args[0] in ("start", "restore"):
                name = args[1]
                self.sandboxes[name] = "running"
                self.probes[name] = 0
                return {"id": name, "state": "running"}, 1.0
            if args[0] == "clone":
                assert args[1] in self.snapshots
                name = args[args.index("--name") + 1]
                self.sandboxes[name] = "running"
                self.probes[name] = 0
                return {"id": name, "state": "running"}, 1.0
            if args[0] == "hibernate" or args[:2] == ("snapshot", "save"):
                name = args[args.index("--name") + 1]
                self.snapshots[name] = args[1] if args[0] == "hibernate" else args[2]
                if args[0] == "hibernate":
                    self.sandboxes[args[1]] = "stopped"
                return {"name": name}, 1.0
            if args[:2] == ("ps", "--all"):
                return [{"name": name, "state": state} for name, state in self.sandboxes.items()], 1.0
            if args[:2] == ("snapshot", "ls"):
                return [{"name": name} for name in self.snapshots], 1.0
            if args[0] == "inspect":
                return {"name": args[1], "state": self.sandboxes[args[1]]}, 1.0
            raise AssertionError(args)


class BenchmarkTest(unittest.TestCase):
    def test_all_modes_and_bursts_exclude_setup_and_cleanup(self):
        args = lifecycle.parse_args([
            "--image", "fixture", "--lanes", "0", "--warmups", "1",
            "--samples", "2", "--concurrency", "1,2", "--batches", "1",
            "--poll-interval", "0.001",
        ])
        fake = FakeKumaBox()
        with tempfile.TemporaryDirectory() as directory:
            bench = lifecycle.Benchmark(args, fake, "test", Path(directory))
            try:
                snapshot = bench.prepare_clone(0)
                for mode in lifecycle.MODES:
                    exec_source = bench.create_ready("exec-source", 0) if mode == "exec" else None
                    bench.run_serial(mode, 0, snapshot, exec_source)
                    if exec_source:
                        bench.cleanup_sandbox(exec_source)
                    if mode in ("cold", "clone"):
                        bench.run_bursts(mode, 0, snapshot)
            finally:
                remaining = bench.cleanup()
                bench.report()
            self.assertEqual(remaining, {"sandboxes": [], "snapshots": []})
            self.assertFalse(fake.sandboxes)
            self.assertFalse(fake.snapshots)
            self.assertEqual(len(bench.batches), 2)
            self.assertEqual({record["mode"] for record in bench.samples}, set(lifecycle.MODES))
            self.assertTrue(all(record["success"] for record in bench.samples))
            self.assertTrue(all(record["probe_attempts"] == 2 for record in bench.samples
                                if record["mode"] != "exec"))
            summary = json.loads((Path(directory) / "summary.json").read_text())
            self.assertEqual(len(summary), 7)
            self.assertTrue(all(item["ready_ms"]["p95"] is None for item in summary))
            self.assertEqual(len(json.loads((Path(directory) / "setup_summary.json").read_text())), 2)
            self.assertEqual(sum(args[0] == "run" and "exec-source" in args[args.index("--name") + 1]
                                 for args in fake.commands if "--name" in args), 1)

    def test_failure_is_recorded_and_only_owned_name_is_cleaned(self):
        args = lifecycle.parse_args(["--image", "fixture", "--lanes", "0", "--warmups", "0",
                                     "--samples", "1", "--ready-timeout", "0.001",
                                     "--poll-interval", "0.01"])
        fake = FakeKumaBox()
        fake.sandboxes["someone-else"] = "running"
        with tempfile.TemporaryDirectory() as directory:
            bench = lifecycle.Benchmark(args, fake, "test", Path(directory))
            try:
                bench.run_serial("cold", 0, None)
            finally:
                bench.cleanup()
                bench.report()
            self.assertEqual(fake.sandboxes, {"someone-else": "running"})
            self.assertFalse(bench.samples[0]["success"])
            self.assertGreater(bench.samples[0]["failed_ms"], 0)
            diagnostics = [json.loads(row) for row in (Path(directory) / "samples.jsonl").read_text().splitlines()
                           if '"type": "diagnostic"' in row]
            self.assertEqual(diagnostics[0]["vmm_log"], "vmm test log")
            self.assertEqual(json.loads((Path(directory) / "summary.json").read_text())[0]["failure_rate"], 1)

    def test_setup_failure_retries_without_counting_as_restore_attempt(self):
        args = lifecycle.parse_args(["--image", "fixture", "--lanes", "0", "--warmups", "0",
                                     "--samples", "1", "--poll-interval", "0.001"])
        fake = FakeKumaBox()
        fake.fail_next_run = True
        with tempfile.TemporaryDirectory() as directory:
            bench = lifecycle.Benchmark(args, fake, "test", Path(directory))
            try:
                bench.run_serial("restore", 0, None)
            finally:
                bench.cleanup()
                bench.report()
            summary = json.loads((Path(directory) / "summary.json").read_text())[0]
            self.assertEqual((summary["attempts"], summary["successes"], summary["setup_failures"]),
                             (1, 1, 1))
            self.assertEqual(bench.samples[0]["failure_phase"], "setup")


if __name__ == "__main__":
    unittest.main()
