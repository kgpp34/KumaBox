#!/usr/bin/env python3
"""Measure real sandbox storage behavior in an explicitly isolated data root.

Uses lifecycle.py's CLI/readiness contract. Run as root on the KVM host with
an already imported image. Only this invocation's named resources are removed.
"""

import argparse
import concurrent.futures
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import threading
import time
import uuid
from pathlib import Path

from lifecycle import KumaBox
from xfs import command, extents, physical_union, summarize_records


MIB = 1024 * 1024


class SandboxExperiments:
    def __init__(self, args):
        self.args = args
        self.cli = KumaBox(args.binary, ["--root-dir", str(args.root), "--run-dir", str(args.run),
                                       "--log-dir", str(args.log)], 240)
        self.prefix = "kbxfslab-" + uuid.uuid4().hex[:8]
        self.sandboxes, self.snapshots = set(), set()
        self.rows = []
        self.raw = (args.output / "samples.jsonl").open("x")
        self.counter = 0
        self.lock = threading.Lock()

    def emit(self, scenario, **fields):
        row = {"scenario": scenario, **fields}
        self.rows.append(row)
        self.raw.write(json.dumps(row) + "\n")
        self.raw.flush()

    def name(self, kind):
        with self.lock:
            self.counter += 1
            name = f"{self.prefix}-{kind}-{self.counter}"
            (self.snapshots if kind == "snapshot" else self.sandboxes).add(name)
        return name

    def ready(self, name):
        deadline = time.monotonic() + 90
        while True:
            try:
                self.cli.call("exec", name, "--", "/bin/true", timeout=5)
                return
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.01)

    def settle(self, name):
        # Snapshotting as soon as exec works captures an OS that may still be
        # starting services. Keep golden preparation identical across variants;
        # this wait is setup, never part of cold-boot or clone timing.
        self.cli.call("exec", name, "--", "sh", "-ec",
                      "for attempt in $(seq 1 600); do "
                      "if systemctl is-active --quiet multi-user.target; then "
                      "udevadm settle --timeout=20; exit 0; fi; sleep 0.1; done; exit 1")

    def run(self, memory="512MiB", disk=None):
        name = self.name("sandbox")
        arguments = ["run", self.args.image, "--name", name, "--cpus", str(self.args.cpus), "--memory", memory,
                     "--storage", "10GiB", "--nics", "0", "--json"]
        if disk:
            arguments += ["--data-disk", disk]
        start = time.perf_counter_ns()
        record, operation = self.cli.json(*arguments)
        self.ready(name)
        return name, record, operation, (time.perf_counter_ns() - start) / 1e6

    def remove_sandbox(self, name):
        self.cli.call("stop", name)
        self.cli.call("rm", name)
        self.sandboxes.discard(name)

    def remove_snapshot(self, name):
        self.cli.call("snapshot", "rm", name)
        self.snapshots.discard(name)

    def save(self, name):
        snapshot = self.name("snapshot")
        record, total = self.cli.json("snapshot", "save", name, "--name", snapshot, "--json")
        return snapshot, record, total

    def clone(self, snapshot, barrier=None):
        name = self.name("clone")
        if barrier:
            barrier.wait(timeout=120)
        start = time.perf_counter_ns()
        record, operation = self.cli.json("clone", snapshot, "--name", name, "--json")
        self.ready(name)
        ready = (time.perf_counter_ns() - start) / 1e6
        return name, record, operation, ready

    def memory_evidence(self, record):
        directory = self.args.root / "vmm" / record["id"] / "clone-state"
        scope = Path("/sys/fs/cgroup/kumabox.slice") / f"sandbox-{record['id']}.scope" / "io.stat"
        cpu = scope.with_name("cpu.stat")
        process = json.loads((self.args.run / "sandboxes" / record["id"] / "process.json").read_text())
        process_io = Path("/proc") / str(process["pid"]) / "io"
        return {"files": [{"name": path.name, "inode": path.stat().st_ino,
                            "links": path.stat().st_nlink, "size": path.stat().st_size}
                           for path in directory.iterdir() if path.name.startswith("memory-range")],
                "io_stat": scope.read_text() if scope.exists() else None,
                "cpu_stat": cpu.read_text() if cpu.exists() else None,
                "process_io": process_io.read_text()}

    def cold(self):
        for index in range(self.args.samples):
            name, _, operation, ready = self.run()
            self.emit("cold", iteration=index, operation_ms=operation, ready_ms=ready)
            self.remove_sandbox(name)

    def clone_density(self):
        for populated in (0, 512):
            source, record, _, _ = self.run()
            self.settle(source)
            if populated:
                self.cli.call("exec", source, "--", "sh", "-c",
                              f"dd if=/dev/zero of=/root/xfs-lab-payload bs=1M count={populated} conv=fsync status=none; sync")
            snapshot, snap_record, capture_ms = self.save(source)
            self.cli.call("stop", source)
            snapshot_dir = self.args.root / "snapshots" / snap_record["id"]
            cow = snapshot_dir / "cow.raw"
            self.emit(f"golden_{populated}", save_ms=capture_ms,
                      image=self.args.image, logical_bytes=cow.stat().st_size,
                      allocated_bytes=cow.stat().st_blocks * 512, extent_count=len(extents(cow)),
                      files=[{"name": path.name, "logical_bytes": path.stat().st_size,
                              "allocated_bytes": path.stat().st_blocks * 512}
                             for path in snapshot_dir.iterdir() if path.is_file()])
            for index in range(self.args.samples):
                name, child, operation, ready = self.clone(snapshot)
                child_cow = self.args.root / "sandboxes" / child["id"] / "cow.raw"
                self.emit(f"clone_populated_{populated}", iteration=index, operation_ms=operation,
                          ready_ms=ready, child_allocated_bytes=child_cow.stat().st_blocks * 512)
                if index == 0:
                    self.emit(f"native_memory_{populated}", **self.memory_evidence(child))
                if index == 0 and populated:
                    # Reads and a persisted child overwrite must leave golden
                    # contents unchanged; a sibling checks through the guest.
                    self.cli.call("exec", name, "--", "sh", "-c",
                                  "printf child > /root/xfs-lab-payload; sync")
                    self.emit("clone_disk_sharing", unique_pair_bytes=physical_union([cow, child_cow]),
                              shared_extents=sum(item["shared"] for item in extents(child_cow)))
                self.remove_sandbox(name)
            if populated:
                name, _, _, _ = self.clone(snapshot)
                output, _ = self.cli.call("exec", name, "--", "sh", "-c",
                                          "od -An -tu1 -N5 /root/xfs-lab-payload")
                assert output.split() == ["0"] * 5, "clone write changed golden disk"
                self.emit("sandbox_clone_isolation", verified=True)
                self.remove_sandbox(name)
            else:
                for concurrency in (4, 16):
                    for index in range(self.args.batches):
                        barrier = threading.Barrier(concurrency + 1)
                        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                            futures = [pool.submit(self.clone, snapshot, barrier) for _ in range(concurrency)]
                            start = time.perf_counter_ns()
                            barrier.wait(timeout=120)
                            children = [future.result() for future in futures]
                            wall = (time.perf_counter_ns() - start) / 1e6
                        self.emit(f"clone_burst_{concurrency}", iteration=index, batch_ms=wall,
                                  individual_ready_ms=[child[3] for child in children], successes=len(children))
                        for child in children:
                            self.remove_sandbox(child[0])
            self.remove_sandbox(source)
            self.remove_snapshot(snapshot)

    def captures(self):
        for memory in ("512MiB", "2GiB"):
            name, _, _, _ = self.run(memory=memory)
            self.settle(name)
            for index in range(self.args.samples if memory == "512MiB" else 5):
                snapshot, record, total = self.save(name)
                directory = self.args.root / "snapshots" / record["id"]
                self.emit(f"snapshot_{memory}", iteration=index, total_ms=total,
                          allocated_bytes=sum(path.stat().st_blocks * 512 for path in directory.iterdir()))
                self.remove_snapshot(snapshot)
            self.remove_sandbox(name)

    def data_io(self):
        for direct in ("on", "off"):
            name, _, _, _ = self.run(disk=f"name=lab,size=1GiB,directio={direct}")
            self.settle(name)
            self.cli.call("exec", name, "--", "sh", "-ec",
                          "mkdir -p /mnt/xfs-lab; "
                          "for attempt in $(seq 1 200); do "
                          "dev=$(lsblk -dn -o NAME,SERIAL | awk '$2==\"lab\" {print \"/dev/\"$1}'); "
                          "test -n \"$dev\" && break; sleep 0.05; done; "
                          "test -n \"$dev\"; mount \"$dev\" /mnt/xfs-lab")
            for index in range(self.args.samples):
                for workload, arguments in (("sequential", "bs=1M count=128 conv=fsync"),
                                            ("sync4k", "bs=4K count=256 oflag=dsync")):
                    self.cli.call("exec", name, "--", "rm", "-f", "/mnt/xfs-lab/io.raw")
                    _, total = self.cli.call("exec", name, "--", "dd", "if=/dev/zero",
                                             "of=/mnt/xfs-lab/io.raw", *arguments.split(), "status=none")
                    self.emit(f"guest_{workload}_direct_{direct}", iteration=index, total_ms=total)
            self.remove_sandbox(name)

    def portable(self):
        name, _, _, _ = self.run()
        self.settle(name)
        environment, _ = self.cli.call("exec", name, "--", "sh", "-c",
                                        "uname -r; /usr/local/bin/kumabox-agent --version; df -T /")
        self.emit("guest_environment", output=environment)
        self.cli.call("exec", name, "--", "sh", "-c",
                      "dd if=/dev/zero of=/root/xfs-transfer bs=1M count=128 conv=fsync status=none; sync")
        snapshot, _, _ = self.save(name)
        self.cli.call("stop", name)
        # Parents stay private. Paths on XFS exercise a different directory;
        # tmpfs crosses a mount, forcing the sparse byte-copy fallback.
        with tempfile.TemporaryDirectory(prefix="kbxfslab-transfer-", dir=self.args.output) as xfs_parent, \
                tempfile.TemporaryDirectory(prefix="kbxfslab-transfer-") as tmp_parent:
            for parent, kind in ((xfs_parent, "xfs"), (tmp_parent, "tmpfs")):
                for index in range(self.args.samples):
                    destination = Path(parent) / "snapshot"
                    _, export_ms = self.cli.call("snapshot", "export", snapshot, "--to-dir", str(destination))
                    self.emit(f"export_directory_{kind}", iteration=index, total_ms=export_ms,
                              same_device=destination.stat().st_dev == self.args.root.stat().st_dev)
                    child = self.name("clone")
                    start = time.perf_counter_ns()
                    record, operation = self.cli.json("clone", "--from-dir", str(destination), "--name", child, "--json")
                    self.ready(child)
                    self.emit(f"clone_directory_{kind}", iteration=index, operation_ms=operation,
                              ready_ms=(time.perf_counter_ns() - start) / 1e6)
                    if index == 0:
                        self.emit(f"directory_memory_{kind}", **self.memory_evidence(record))
                    self.remove_sandbox(child)
                    shutil.rmtree(destination)
            for index in range(self.args.samples):
                archive = Path(xfs_parent) / "snapshot.tar"
                _, export_ms = self.cli.call("snapshot", "export", snapshot, "--output", str(archive))
                self.emit("export_tar_xfs", iteration=index, total_ms=export_ms, stored_bytes=archive.stat().st_size)
                archive.unlink()
        self.remove_sandbox(name)
        self.remove_snapshot(snapshot)

    def diagnostics(self):
        """Trace the CLI's durability boundary without tracing live VMMs.

        These are explanatory samples with tracing overhead, not benchmarks.
        Follow Go's OS threads with -f only for save/export, which do not launch
        a VMM. Never apply it to run/clone: following their detached VMM would
        keep strace alive after the CLI exits.
        """
        for memory in ("512MiB", "2GiB"):
            name, _, _, _ = self.run(memory=memory)
            self.settle(name)
            snapshot = self.name("snapshot")
            trace = self.args.output / f"snapshot-{memory}.strace"
            result = subprocess.run(
                ["strace", "-f", "-T", "-yy", "-e", "trace=fsync,fdatasync", "-o", str(trace),
                 *self.cli.prefix, "snapshot", "save", name, "--name", snapshot, "--json"],
                capture_output=True, text=True, timeout=240, check=True)
            saved = json.loads(result.stdout)
            self.cli.call("stop", name)
            if memory == "512MiB":
                directory = self.args.root / "snapshots" / saved["id"]
                self.emit("source_memory", files=[{"name": path.name, "inode": path.stat().st_ino}
                          for path in directory.iterdir() if path.name.startswith("memory-range")])
                child, record, operation, ready = self.clone(snapshot)
                self.emit("diagnostic_native_clone", operation_ms=operation, ready_ms=ready,
                          **self.memory_evidence(record))
                self.remove_sandbox(child)
                with tempfile.TemporaryDirectory(prefix="kbxfslab-diagnostic-", dir=self.args.output) as parent:
                    portable = Path(parent) / "snapshot"
                    self.cli.call("snapshot", "export", snapshot, "--to-dir", str(portable))
                    child = self.name("clone")
                    start = time.perf_counter_ns()
                    record, operation = self.cli.json("clone", "--from-dir", str(portable), "--name", child, "--json")
                    self.ready(child)
                    self.emit("diagnostic_directory_clone", operation_ms=operation,
                              ready_ms=(time.perf_counter_ns() - start) / 1e6,
                              **self.memory_evidence(record))
                    self.remove_sandbox(child)
                    archive = Path(parent) / "snapshot.tar"
                    subprocess.run(
                        ["/usr/bin/time", "-v", "-o", str(self.args.output / "tar-time.txt"),
                         "strace", "-f", "-T", "-yy", "-e", "trace=fsync,fdatasync", "-o", str(self.args.output / "tar.strace"),
                         *self.cli.prefix, "snapshot", "export", snapshot, "--output", str(archive)],
                        capture_output=True, text=True, timeout=240, check=True)
            self.remove_sandbox(name)
            self.remove_snapshot(snapshot)

    def cow_hint_guest(self):
        source, _, _, _ = self.run()
        self.settle(source)
        self.cli.call("exec", source, "--", "sh", "-ec",
                      "dd if=/dev/zero of=/root/xfs-cow-test bs=1M count=128 conv=fsync status=none; sync")
        snapshot, _, _ = self.save(source)
        self.cli.call("stop", source)
        for index in range(self.args.samples):
            hints = (0, MIB) if index % 2 == 0 else (MIB, 0)
            for hint in hints:
                name, record, _, _ = self.clone(snapshot)
                cow = self.args.root / "sandboxes" / record["id"] / "cow.raw"
                command("xfs_io", "-c", f"cowextsize {hint}", str(cow))
                for pass_index in (1, 2):
                    _, total = self.cli.call("exec", name, "--", "dd", "if=/dev/zero",
                                             "of=/root/xfs-cow-test", "bs=4K", "count=256",
                                             "conv=notrunc,fsync", "status=none")
                    self.emit(f"guest_cow_hint_{hint}_pass{pass_index}", iteration=index, total_ms=total)
                self.remove_sandbox(name)
        self.remove_sandbox(source)
        self.remove_snapshot(snapshot)

    def finish(self):
        errors = []
        for name in sorted(self.sandboxes):
            try:
                self.remove_sandbox(name)
            except RuntimeError as error:
                errors.append(str(error))
        for name in sorted(self.snapshots):
            try:
                self.remove_snapshot(name)
            except RuntimeError as error:
                errors.append(str(error))
        self.raw.close()
        summary = summarize_records(self.rows)
        (self.args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        remaining = {"sandboxes": sorted(self.sandboxes), "snapshots": sorted(self.snapshots), "errors": errors}
        (self.args.output / "remaining.json").write_text(json.dumps(remaining, indent=2) + "\n")
        if any(remaining.values()):
            raise RuntimeError(f"cleanup incomplete: {remaining}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--samples", type=int, default=20)
    parser.add_argument("--cpus", type=int, default=1)
    parser.add_argument("--batches", type=int, default=5)
    parser.add_argument("--stages", default="cold,clone_density,captures,data_io,portable",
                        help="comma-separated experiment methods")
    args = parser.parse_args()
    allowed = {"cold", "clone_density", "captures", "data_io", "portable", "diagnostics", "cow_hint_guest"}
    stages = args.stages.split(",")
    if os.geteuid() != 0 or min(args.samples, args.batches, args.cpus) < 1 or not stages or any(stage not in allowed for stage in stages):
        parser.error("requires root and positive sample/batch counts")
    # An explicit empty root avoids sharing metadata and resource ownership
    # with the host installation. Existing images are safe, sandboxes are not.
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    for script in ("xfs_sandbox.py", "lifecycle.py", "xfs.py"):
        shutil.copyfile(Path(__file__).with_name(script), args.output / script)
    lab = SandboxExperiments(args)
    before, _ = lab.cli.json("ps", "--all", "--json")
    if before:
        raise SystemExit("experimental root must contain no sandboxes")
    image, _ = lab.cli.json("image", "inspect", args.image)
    metadata = {"started_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "image": image, "parameters": vars(args),
                "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "timing_contract": "ready=CLI operation plus successful guest /bin/true; guest IO includes exec overhead; no NICs; warm host caches"}
    (args.output / "metadata.json").write_text(json.dumps(metadata, indent=2, default=str) + "\n")
    try:
        for stage in stages:
            print(f"{time.strftime('%H:%M:%S')} {stage}", flush=True)
            getattr(lab, stage)()
    finally:
        lab.finish()
    print(f"results: {args.output}", flush=True)


if __name__ == "__main__":
    main()
