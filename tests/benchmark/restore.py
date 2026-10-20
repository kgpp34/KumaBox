#!/usr/bin/env python3
"""Compare restore binaries serially on the same immutable golden snapshot.

Run as root on Linux/KVM with an imported image in an explicit test root.
Variants alternate order each iteration. Readiness ends at successful guest
/bin/true, before diagnostics and file checks. No host-wide cache is dropped.
Only resources created by this invocation are removed.
"""

import argparse
import hashlib
import json
import os
import shutil
import time
import uuid
from pathlib import Path

from lifecycle import KumaBox, host_metadata, optional_output, percentile


class RestoreBenchmark:
    def __init__(self, args):
        self.args = args
        self.prefix = "kbrestore-" + uuid.uuid4().hex[:8]
        common = ["--root-dir", str(args.root), "--run-dir", str(args.run),
                  "--log-dir", str(args.log)]
        self.variants = [(name, KumaBox(binary, common, 120))
                         for name, binary in args.variant]
        self.owner = self.variants[0][1]
        self.modes = dict(args.restore_mode)
        self.sandboxes, self.snapshots, self.exports = set(), set(), set()
        self.rows = []
        self.raw = (args.output / "samples.jsonl").open("x")

    def ready(self, cli, name):
        deadline = time.monotonic() + 30
        while True:
            try:
                cli.call("exec", name, "--", "/bin/true", timeout=5)
                return
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.005)

    def evidence(self, record):
        scope = Path("/sys/fs/cgroup/kumabox.slice") / f"sandbox-{record['id']}.scope"
        native = self.args.root / "vmm" / record["id"]
        # The durable VMM area is separate from transient /run sockets.
        memory = list(native.rglob("memory-range*")) if native.exists() else []
        return {
            "cpu_max": (scope / "cpu.max").read_text().strip(),
            "cpu_stat": (scope / "cpu.stat").read_text(),
            "memory_files": [{"inode": item.stat().st_ino, "name": item.name,
                              "links": item.stat().st_nlink} for item in memory],
        }

    def remove(self, name):
        self.owner.call("stop", name)
        self.owner.call("rm", name)
        self.sandboxes.discard(name)

    def run_shape(self, cpus):
        source = f"{self.prefix}-source-{cpus}"
        capture = f"{self.prefix}-golden-{cpus}"
        self.sandboxes.add(source)
        self.owner.json("run", self.args.image, "--name", source,
                        "--cpus", str(cpus), "--memory", self.args.memory,
                        "--nics", "0", "--json")
        self.ready(self.owner, source)
        self.owner.call("exec", source, "--", "sh", "-ec",
                        "printf golden > /root/restore-benchmark-marker; sync; "
                        "for i in $(seq 1 300); do "
                        "systemctl is-active --quiet multi-user.target && exit 0; "
                        "sleep 0.1; done; exit 1")
        time.sleep(5)  # Golden preparation, outside every measured operation.
        self.snapshots.add(capture)
        self.owner.call("snapshot", "save", source, "--name", capture, "--json")
        self.owner.call("stop", source)
        portable = self.args.output / f"golden-{cpus}"
        self.exports.add(portable)
        self.owner.call("snapshot", "export", capture, "--to-dir", str(portable))
        self.measure(source, capture, portable, cpus)
        self.remove(source)
        self.owner.call("snapshot", "rm", capture)
        self.snapshots.discard(capture)
        shutil.rmtree(portable)
        self.exports.discard(portable)

    def measure(self, source, capture, portable, cpus):
        for scenario in self.args.scenarios:
            for iteration in range(self.args.warmups + self.args.samples):
                variants = self.variants if iteration % 2 == 0 else self.variants[::-1]
                for label, cli in variants:
                    name = (source if scenario == "restore" else
                            f"{self.prefix}-{scenario}-{cpus}-{iteration}-{label}")
                    arguments = ["restore", source, capture, "--json"]
                    if scenario != "restore":
                        self.sandboxes.add(name)
                        arguments = ["clone", "--name", name, "--json"]
                        arguments += ([capture] if scenario == "clone" else
                                      ["--from-dir", str(portable)])
                    if label in self.modes:
                        arguments += ["--restore-mode", self.modes[label]]
                    started = time.perf_counter_ns()
                    restored, operation = cli.json(*arguments)
                    self.ready(cli, name)
                    ready = (time.perf_counter_ns() - started) / 1e6
                    evidence = self.evidence(restored)
                    expected = f"{cpus * 100000} 100000"
                    if evidence["cpu_max"] != expected:
                        raise AssertionError(f"runtime quota {evidence['cpu_max']} != {expected}")
                    marker, _ = cli.call("exec", name, "--", "cat", "/root/restore-benchmark-marker")
                    if marker != "golden":
                        raise AssertionError(f"disk rollback/isolation failed: {marker!r}")
                    row = {"variant": label, "scenario": scenario, "cpus": cpus, "sandbox_id": restored["id"],
                           "memory": self.args.memory, "iteration": iteration,
                           "warmup": iteration < self.args.warmups,
                           "operation_ms": operation, "first_exec_ms": ready, **evidence}
                    self.rows.append(row)
                    self.raw.write(json.dumps(row) + "\n")
                    self.raw.flush()
                    print(f"{label} {scenario} cpu={cpus} #{iteration}: "
                          f"operation={operation:.2f} first_exec={ready:.2f}ms", flush=True)
                    if scenario == "restore":
                        cli.call("exec", source, "--", "sh", "-ec",
                                 "printf changed > /root/restore-benchmark-marker; sync")
                        cli.call("stop", source)
                    else:
                        self.remove(name)

    def cleanup(self):
        errors = []
        for name in sorted(self.sandboxes):
            try:
                self.remove(name)
            except RuntimeError as error:
                errors.append(str(error))
        for name in sorted(self.snapshots):
            try:
                self.owner.call("snapshot", "rm", name)
            except RuntimeError as error:
                errors.append(str(error))
        for directory in sorted(self.exports):
            try:
                shutil.rmtree(directory)
            except OSError as error:
                errors.append(str(error))
        self.raw.close()
        return errors

    def summary(self):
        groups = {}
        for row in self.rows:
            if not row["warmup"]:
                key = (row["variant"], row["scenario"], row["cpus"])
                groups.setdefault(key, []).append(row)
        return [{"variant": key[0], "scenario": key[1], "cpus": key[2], "n": len(rows),
                 **{metric: {"p50": percentile([r[metric] for r in rows], 50),
                             "p95": percentile([r[metric] for r in rows], 95)}
                    for metric in ("operation_ms", "first_exec_ms")}}
                for key, rows in sorted(groups.items())]


def variant(value):
    name, separator, binary = value.partition("=")
    if not separator or not name.isalnum() or not Path(binary).is_absolute():
        raise argparse.ArgumentTypeError("use an alphanumeric NAME=/absolute/binary/path")
    return name, binary


def restore_mode(value):
    name, separator, mode = value.partition("=")
    if not separator or mode not in ("auto", "copy", "ondemand", "mmap"):
        raise argparse.ArgumentTypeError("use NAME=auto|copy|ondemand|mmap")
    return name, mode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--variant", type=variant, action="append", required=True)
    parser.add_argument("--restore-mode", type=restore_mode, action="append", default=[],
                        help="override the mode of one variant: NAME=MODE")
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--samples", type=int, default=20)
    parser.add_argument("--warmups", type=int, default=2)
    parser.add_argument("--cpus", type=int, nargs="+", default=[1, 2])
    parser.add_argument("--memory", default="512MiB")
    parser.add_argument("--scenarios", nargs="+", choices=("restore", "clone", "directory"),
                        default=["restore", "clone", "directory"])
    args = parser.parse_args()
    if os.geteuid() != 0 or args.samples < 2 or args.warmups < 0 or any(n < 1 for n in args.cpus):
        parser.error("requires root, samples >= 2, warmups >= 0 and positive CPU counts")
    if len({name for name, _ in args.variant}) != len(args.variant):
        parser.error("variant names must be unique")
    if any(name not in dict(args.variant) for name, _ in args.restore_mode):
        parser.error("restore-mode names must match a variant")
    args.output.mkdir(parents=True, exist_ok=False)
    benchmark = RestoreBenchmark(args)
    monitor = os.environ.get("KUMABOX_VMM_CLOUD_HYPERVISOR_BINARY", "cloud-hypervisor")
    host = host_metadata()
    host["cloud_hypervisor"] = optional_output(monitor, "--version")
    host["data_filesystem"] = optional_output("findmnt", "-no", "SOURCE,FSTYPE", "--target", str(args.root))
    report = {"host": host, "monitor_binary": monitor, "image": args.image,
              "variants": args.variant, "restore_modes": args.restore_mode,
              "binary_sha256": {name: hashlib.sha256(Path(binary).read_bytes()).hexdigest()
                                for name, binary in args.variant},
              "resource_prefix": benchmark.prefix}
    try:
        for cpus in args.cpus:
            benchmark.run_shape(cpus)
    except BaseException as error:
        report["error"] = str(error)
        raise
    finally:
        report["cleanup_errors"] = benchmark.cleanup()
        report["summary"] = benchmark.summary()
        (args.output / "result.json").write_text(json.dumps(report, indent=2) + "\n")
    if report["cleanup_errors"]:
        raise RuntimeError(report["cleanup_errors"])


if __name__ == "__main__":
    main()
