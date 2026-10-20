#!/usr/bin/env python3
"""Measure KumaBox lifecycle latency on the Linux/KVM host itself.

Only resources with this invocation's unique ``kbbench-*`` prefix are removed.
The image must already be imported; image preparation is never timed as boot.
"""

import argparse
import concurrent.futures
import hashlib
import json
import os
import platform
import re
import shutil
import statistics
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path


MODES = ("cold", "restart", "clone", "restore", "exec")


def percentile(values, percent):
    """Return an interpolated percentile without inventing a p99 from few runs."""
    ordered = sorted(values)
    position = (len(ordered) - 1) * percent / 100
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def optional_output(*argv):
    try:
        return subprocess.check_output(argv, text=True, stderr=subprocess.DEVNULL, timeout=5).strip()
    except (OSError, subprocess.SubprocessError):
        return None


def host_metadata():
    cpu = None
    memory_kib = None
    if Path("/proc/cpuinfo").exists():
        match = re.search(r"^model name\s*:\s*(.+)$", Path("/proc/cpuinfo").read_text(), re.M)
        cpu = match.group(1) if match else None
    if Path("/proc/meminfo").exists():
        match = re.search(r"^MemTotal:\s*(\d+) kB$", Path("/proc/meminfo").read_text(), re.M)
        memory_kib = int(match.group(1)) if match else None
    return {
        "platform": platform.platform(),
        "logical_cpus": os.cpu_count(),
        "cpu_model": cpu,
        "memory_kib": memory_kib,
        "cloud_hypervisor": optional_output("cloud-hypervisor", "--version"),
        "data_filesystem": optional_output("findmnt", "-no", "SOURCE,FSTYPE", "--target", "/var/lib/kumabox"),
        "cpu_governor": (
            Path("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor").read_text().strip()
            if Path("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor").exists() else None
        ),
        "git_commit": optional_output("git", "-C", str(Path(__file__).resolve().parents[2]), "rev-parse", "HEAD"),
    }


def cgroup_metrics(sandbox_id):
    """Read a VM's cgroup counters after its readiness stopwatch has stopped."""
    if not sandbox_id:
        return None
    scope = Path("/sys/fs/cgroup/kumabox.slice") / f"sandbox-{sandbox_id}.scope"
    if not scope.is_dir():
        return None
    result = {}
    for filename in ("memory.current", "memory.peak"):
        try:
            result[filename.replace(".", "_")] = int((scope / filename).read_text().strip())
        except (OSError, ValueError):
            pass
    try:
        cpu = (scope / "cpu.stat").read_text()
        match = re.search(r"^usage_usec\s+(\d+)$", cpu, re.M)
        if match:
            result["cpu_usage_usec"] = int(match.group(1))
    except OSError:
        pass
    return result or None


class CommandError(RuntimeError):
    pass


class KumaBox:
    """Run independent CLI processes and keep their complete elapsed time."""

    def __init__(self, binary, global_args, timeout):
        self.prefix = [binary, *global_args]
        self.timeout = timeout

    def call(self, *args, timeout=None):
        argv = [*self.prefix, *args]
        started = time.monotonic_ns()
        try:
            process = subprocess.run(
                argv, capture_output=True, text=True,
                timeout=timeout or self.timeout, check=False,
            )
        except subprocess.TimeoutExpired as error:
            raise CommandError(f"{args[0]} timed out after {error.timeout}s") from error
        elapsed_ms = (time.monotonic_ns() - started) / 1e6
        if process.returncode:
            detail = (process.stderr or process.stdout).strip()[-800:]
            raise CommandError(f"{args[0]} exited {process.returncode}: {detail}")
        return process.stdout, elapsed_ms

    def json(self, *args):
        output, elapsed_ms = self.call(*args)
        try:
            return json.loads(output), elapsed_ms
        except json.JSONDecodeError as error:
            raise CommandError(f"{args[0]} returned invalid JSON: {output[:300]!r}") from error


class Benchmark:
    """Own benchmark artifacts and separate setup, timing, and teardown."""

    def __init__(self, args, cli, run_id, output_dir):
        self.args = args
        self.cli = cli
        self.run_id = run_id
        self.output_dir = output_dir
        self.samples = []
        self.batches = []
        self.setups = []
        self.owned_sandboxes = set()
        self.owned_snapshots = set()
        self.lock = threading.Lock()
        self.serial = 0
        self.raw = (output_dir / "samples.jsonl").open("w", encoding="utf-8")

    def emit(self, record):
        with self.lock:
            self.raw.write(json.dumps(record, sort_keys=True) + "\n")
            self.raw.flush()
            if record["type"] == "sample":
                self.samples.append(record)
            elif record["type"] == "batch":
                self.batches.append(record)
            elif record["type"] == "setup":
                self.setups.append(record)

    def name(self, kind, lane):
        with self.lock:
            self.serial += 1
            return f"kbbench-{self.run_id}-{kind}-n{lane}-{self.serial:04d}"

    def sandbox_name(self, kind, lane):
        name = self.name(kind, lane)
        with self.lock:
            self.owned_sandboxes.add(name)
        return name

    def snapshot_name(self, kind, lane):
        name = self.name(kind, lane)
        with self.lock:
            self.owned_snapshots.add(name)
        return name

    def run_args(self, name, lane):
        return (
            "run", self.args.image, "--name", name,
            "--cpus", str(self.args.cpus), "--memory", self.args.memory,
            "--storage", self.args.storage, "--nics", str(lane), "--json",
        )

    def ready(self, name):
        """Probe the guest contract; CLI 'running' alone does not prove readiness."""
        deadline = time.monotonic() + self.args.ready_timeout
        attempts = 0
        last_error = ""
        while True:
            if attempts and time.monotonic() >= deadline:
                raise CommandError(f"guest agent not ready after {attempts} probes: {last_error}")
            attempts += 1
            try:
                self.cli.call("exec", name, "--", "/bin/true", timeout=self.args.probe_timeout)
                return attempts
            except CommandError as error:
                last_error = str(error)
            if time.monotonic() >= deadline:
                raise CommandError(f"guest agent not ready after {attempts} probes: {last_error}")
            time.sleep(min(self.args.poll_interval, max(0, deadline - time.monotonic())))

    def create_ready(self, kind, lane):
        name = self.sandbox_name(kind, lane)
        self.cli.json(*self.run_args(name, lane))
        self.ready(name)
        return name

    def prepare_clone(self, lane):
        source = self.create_ready("source", lane)
        snapshot = self.snapshot_name("golden", lane)
        _, save_ms = self.cli.json("snapshot", "save", source, "--name", snapshot, "--json")
        self.emit({"type": "setup", "lane": lane, "operation": "snapshot_save",
                   "warmup": False, "elapsed_ms": save_ms})
        self.cli.call("stop", source)
        return snapshot

    def sample(self, mode, lane, warmup, concurrency, batch_index,
               snapshot=None, barrier=None, exec_source=None):
        name = None
        started = None
        phase = "setup"
        try:
            if mode == "restart":
                name = self.create_ready("restart", lane)
                self.cli.call("stop", name)
            elif mode == "restore":
                name = self.create_ready("restore", lane)
                capture = self.snapshot_name("hibernate", lane)
                _, hibernate_ms = self.cli.json("hibernate", name, "--name", capture, "--json")
                self.emit({"type": "setup", "lane": lane, "operation": "hibernate",
                           "warmup": warmup, "elapsed_ms": hibernate_ms})
            elif mode == "exec":
                name = exec_source or self.create_ready("exec", lane)
            else:
                name = self.sandbox_name(mode, lane)
            if barrier is not None:
                barrier.wait(timeout=self.args.command_timeout)
            started = time.monotonic_ns()
            phase = "operation"
            if mode == "cold":
                response, operation_ms = self.cli.json(*self.run_args(name, lane))
            elif mode == "restart":
                response, operation_ms = self.cli.json("start", name, "--json")
            elif mode == "clone":
                response, operation_ms = self.cli.json("clone", snapshot, "--name", name, "--json")
            elif mode == "restore":
                response, operation_ms = self.cli.json("restore", name, capture, "--json")
            else:
                response = {}
                _, operation_ms = self.cli.call("exec", name, "--", "/bin/true")
            if mode == "exec":
                attempts = 1
            else:
                if response.get("state") != "running":
                    raise CommandError(f"{mode} returned state {response.get('state')!r}")
                phase = "readiness"
                attempts = self.ready(name)
            ready_ms = (time.monotonic_ns() - started) / 1e6
            record = {
                "type": "sample", "mode": mode, "lane": lane, "warmup": warmup,
                "concurrency": concurrency, "batch": batch_index, "name": name,
                "sandbox_id": response.get("id"), "success": True,
                "operation_ms": operation_ms, "ready_ms": ready_ms,
                "agent_wait_ms": max(0, ready_ms - operation_ms) if mode != "exec" else None,
                "probe_attempts": attempts, "cgroup": cgroup_metrics(response.get("id")),
            }
        except (CommandError, threading.BrokenBarrierError) as error:
            record = {
                "type": "sample", "mode": mode, "lane": lane, "warmup": warmup,
                "concurrency": concurrency, "batch": batch_index, "name": name,
                "success": False, "failure_phase": phase, "error": str(error),
                "failed_ms": (time.monotonic_ns() - started) / 1e6 if started is not None else None,
            }
        self.emit(record)
        return record

    def cleanup_sandbox(self, name):
        try:
            state, _ = self.cli.json("ps", "--all", "--json")
            match = next((item for item in state if item["name"] == name), None)
            if match is None:
                return
            if match["state"] in ("running", "starting", "stopping"):
                self.cli.call("stop", name)
            self.cli.call("rm", name)
            with self.lock:
                self.owned_sandboxes.discard(name)
        except CommandError as error:
            print(f"cleanup sandbox {name}: {error}", file=sys.stderr)

    def diagnose(self, record):
        """Preserve retained VMM logs before removing a failed sandbox."""
        name = record["name"]
        if not name:
            return
        diagnostic = {"type": "diagnostic", "mode": record["mode"], "lane": record["lane"],
                      "concurrency": record["concurrency"], "batch": record["batch"], "name": name}
        try:
            diagnostic["inspect"], _ = self.cli.json("inspect", name)
        except CommandError as error:
            diagnostic["inspect_error"] = str(error)
        try:
            diagnostic["vmm_log"], _ = self.cli.call("logs", name, "--tail", "100")
        except CommandError as error:
            diagnostic["logs_error"] = str(error)
        self.emit(diagnostic)

    def cleanup_snapshot(self, name):
        try:
            state, _ = self.cli.json("snapshot", "ls", "--json")
            if not any(item.get("name") == name for item in state):
                return
            self.cli.call("snapshot", "rm", name)
            with self.lock:
                self.owned_snapshots.discard(name)
        except CommandError as error:
            print(f"cleanup snapshot {name}: {error}", file=sys.stderr)

    def cleanup(self):
        with self.lock:
            sandboxes = sorted(self.owned_sandboxes)
            snapshots = sorted(self.owned_snapshots)
        for name in sandboxes:
            self.cleanup_sandbox(name)
        for name in snapshots:
            self.cleanup_snapshot(name)
        remaining = {"sandboxes": sorted(self.owned_sandboxes), "snapshots": sorted(self.owned_snapshots)}
        (self.output_dir / "remaining.json").write_text(json.dumps(remaining, indent=2) + "\n")
        self.raw.close()
        return remaining

    def run_serial(self, mode, lane, snapshot, exec_source=None):
        failures = 0
        completed = 0
        attempted = 0
        target = self.args.warmups + self.args.samples
        while completed < target:
            warmup = completed < self.args.warmups
            record = self.sample(mode, lane, warmup, 1, attempted, snapshot,
                                 exec_source=exec_source)
            attempted += 1
            if record.get("failure_phase") != "setup":
                completed += 1
            print(f"{mode} nics={lane} {completed}/{target}: "
                  f"{'warmup' if warmup else 'sample'} {'ok' if record['success'] else 'FAIL'}", flush=True)
            if not record["success"]:
                self.diagnose(record)
            if record["name"] and record["name"] != exec_source:
                self.cleanup_sandbox(record["name"])
            if not record["success"]:
                failures += 1
                if failures >= 3:
                    raise CommandError(f"{mode} failed three times consecutively; see samples.jsonl")
            else:
                failures = 0

    def run_bursts(self, mode, lane, snapshot):
        for concurrency in self.args.concurrency:
            if concurrency == 1:
                continue  # The serial lane already supplies uncontended samples.
            for batch_index in range(self.args.batches):
                barrier = threading.Barrier(concurrency + 1)
                with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                    futures = [pool.submit(self.sample, mode, lane, False, concurrency, batch_index,
                                           snapshot, barrier) for _ in range(concurrency)]
                    started = time.monotonic_ns()
                    barrier.wait(timeout=self.args.command_timeout)
                    records = [future.result() for future in futures]
                    wall_ms = (time.monotonic_ns() - started) / 1e6
                successes = sum(record["success"] for record in records)
                self.emit({"type": "batch", "mode": mode, "lane": lane,
                           "concurrency": concurrency, "batch": batch_index,
                           "wall_ms": wall_ms, "successes": successes, "total": concurrency,
                           "throughput_per_second": successes / (wall_ms / 1000)})
                print(f"{mode} nics={lane} burst={concurrency} batch={batch_index + 1}: "
                      f"{successes}/{concurrency} ready in {wall_ms:.1f}ms", flush=True)
                for record in records:
                    if not record["success"]:
                        self.diagnose(record)
                    if record["name"]:
                        self.cleanup_sandbox(record["name"])

    def report(self):
        groups = {}
        for record in self.samples:
            if record["warmup"]:
                continue
            key = (record["mode"], record["lane"], record["concurrency"])
            groups.setdefault(key, []).append(record)
        summary = []
        for (mode, lane, concurrency), records in sorted(groups.items()):
            measured = [record for record in records if record.get("failure_phase") != "setup"]
            good = [record for record in measured if record["success"]]
            item = {"mode": mode, "lane": lane, "concurrency": concurrency,
                    "attempts": len(measured), "successes": len(good),
                    "setup_failures": len(records) - len(measured),
                    "failure_rate": (len(measured) - len(good)) / len(measured) if measured else None}
            for field in ("operation_ms", "ready_ms", "agent_wait_ms"):
                values = [record[field] for record in good if record.get(field) is not None]
                if values:
                    item[field] = {"p50": statistics.median(values),
                                   "p95": percentile(values, 95) if len(values) >= 20 else None,
                                   "min": min(values), "max": max(values)}
            resources = {}
            for field in ("memory_current", "memory_peak", "cpu_usage_usec"):
                values = [record["cgroup"][field] for record in good
                          if record.get("cgroup") and field in record["cgroup"]]
                if values:
                    resources[field] = {"samples": len(values), "p50": statistics.median(values),
                                        "p95": percentile(values, 95) if len(values) >= 20 else None}
            if resources:
                item["cgroup_at_ready"] = resources
            walls = [batch["wall_ms"] for batch in self.batches
                     if (batch["mode"], batch["lane"], batch["concurrency"]) == (mode, lane, concurrency)]
            if walls:
                item["batch_wall_ms"] = {"p50": statistics.median(walls),
                                         "p95": percentile(walls, 95) if len(walls) >= 20 else None}
                item["throughput_per_second"] = statistics.median(
                    batch["throughput_per_second"] for batch in self.batches
                    if (batch["mode"], batch["lane"], batch["concurrency"]) == (mode, lane, concurrency)
                )
            summary.append(item)
        (self.output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        setup_groups = {}
        for record in self.setups:
            if record.get("warmup"):
                continue
            setup_groups.setdefault((record["operation"], record["lane"]), []).append(record["elapsed_ms"])
        setup_summary = [
            {"operation": operation, "lane": lane, "samples": len(values),
             "p50_ms": statistics.median(values),
             "p95_ms": percentile(values, 95) if len(values) >= 20 else None}
            for (operation, lane), values in sorted(setup_groups.items())
        ]
        (self.output_dir / "setup_summary.json").write_text(json.dumps(setup_summary, indent=2) + "\n")
        for item in summary:
            ready = item.get("ready_ms", {}).get("p50")
            if ready is None:
                print(f"{item['mode']}: no successful samples")
            else:
                print(f"{item['mode']:7} nics={item['lane']} c={item['concurrency']} "
                      f"ok={item['successes']}/{item['attempts']} ready_p50={ready:.1f}ms")


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Example (run on the KumaBox host):\n"
            "  sudo python3 tests/benchmark/lifecycle.py --image smoke-ubuntu\n\n"
            "cold is a fresh image boot; restart boots a stopped disk; clone restores a\n"
            "prebuilt snapshot; restore resumes a hibernated VM; exec uses one live VM.\n"
            "The image must be imported before the run. Warmups, snapshot creation,\n"
            "hibernate, and cleanup are outside startup timing. Exit status is nonzero\n"
            "if any measured request fails or benchmark artifacts remain."
        ),
    )
    parser.add_argument("--image", required=True, help="already-imported image alias or digest")
    parser.add_argument("--binary", default="/usr/local/bin/kumabox")
    parser.add_argument("--config", help="optional KumaBox config file")
    parser.add_argument("--output", type=Path, help="new output directory; default /var/tmp/kumabox-bench-RUNID")
    parser.add_argument("--modes", default=",".join(MODES), help="comma-separated: " + ",".join(MODES))
    parser.add_argument("--lanes", default="0,1", help="comma-separated NIC counts, normally 0,1")
    parser.add_argument("--cpus", type=int, default=2)
    parser.add_argument("--memory", default="2GiB")
    parser.add_argument("--storage", default="10GiB")
    parser.add_argument("--warmups", type=int, default=3)
    parser.add_argument("--samples", type=int, default=30)
    parser.add_argument("--concurrency", default="1,2,4,8")
    parser.add_argument("--batches", type=int, default=5)
    parser.add_argument("--ready-timeout", type=float, default=120)
    parser.add_argument("--probe-timeout", type=float, default=5)
    parser.add_argument("--poll-interval", type=float, default=0.05)
    parser.add_argument("--command-timeout", type=float, default=240)
    args = parser.parse_args(argv)
    args.modes = tuple(part.strip() for part in args.modes.split(","))
    args.lanes = tuple(int(part) for part in args.lanes.split(","))
    args.concurrency = tuple(int(part) for part in args.concurrency.split(","))
    if (not args.modes or any(mode not in MODES for mode in args.modes)
            or not args.lanes or any(lane not in (0, 1) for lane in args.lanes)
            or not args.concurrency or any(count < 1 for count in args.concurrency)
            or min(args.warmups, args.batches) < 0 or args.samples < 1
            or args.cpus < 1 or min(args.ready_timeout, args.probe_timeout,
                                   args.poll_interval, args.command_timeout) <= 0):
        parser.error("invalid modes, lanes, concurrency, sample count, or timeout")
    if len(set(args.modes)) != len(args.modes) or len(set(args.lanes)) != len(args.lanes):
        parser.error("duplicate modes or lanes")
    return args


def main(argv=None):
    args = parse_args(argv)
    if sys.platform != "linux" or os.geteuid() != 0:
        raise SystemExit("run this benchmark as root on the Linux/KVM host")
    binary = shutil.which(args.binary)
    if binary is None:
        raise SystemExit(f"KumaBox binary not found: {args.binary}")
    run_id = uuid.uuid4().hex[:8]
    output_dir = args.output or Path(f"/var/tmp/kumabox-bench-{run_id}")
    cli = KumaBox(binary, ["--config", args.config] if args.config else [], args.command_timeout)
    image, _ = cli.json("image", "inspect", args.image)
    output_dir.mkdir(mode=0o700, parents=True, exist_ok=False)
    metadata = {"run_id": run_id, "started_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "image_reference": args.image, "image": image, "host": host_metadata(),
                "binary_version": optional_output(binary, "version", "--json"),
                "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "parameters": vars(args).copy(),
                "timing_contract": "operation_ms=CLI call; ready_ms=CLI call plus first successful guest /bin/true; setup, cgroup reads, and cleanup excluded"}
    metadata["parameters"]["output"] = str(output_dir)
    (output_dir / "metadata.json").write_text(json.dumps(metadata, indent=2, default=str) + "\n")
    bench = Benchmark(args, cli, run_id, output_dir)
    try:
        for lane in args.lanes:
            snapshot = bench.prepare_clone(lane) if "clone" in args.modes else None
            for mode in args.modes:
                exec_source = bench.create_ready("exec-source", lane) if mode == "exec" else None
                try:
                    bench.run_serial(mode, lane, snapshot, exec_source)
                finally:
                    if exec_source:
                        bench.cleanup_sandbox(exec_source)
                if mode in ("cold", "clone"):
                    bench.run_bursts(mode, lane, snapshot)
    finally:
        remaining = bench.cleanup()
        bench.report()
        print(f"results: {output_dir}", flush=True)
        if any(remaining.values()):
            print(f"some benchmark artifacts remain: {remaining}", file=sys.stderr)
    if any(not record["success"] for record in bench.samples) or any(remaining.values()):
        raise SystemExit("benchmark completed with failures; inspect samples.jsonl and remaining.json")


if __name__ == "__main__":
    main()
