#!/usr/bin/env python3
"""Bounded XFS experiments using private files, never raw block devices.

Run on the Linux host. Data lives below --directory (which must be XFS);
--output is a new result directory. No mount options, global caches, or kernel
settings are changed. Each invocation owns and removes only its mkdtemp tree.
"""

import argparse
import concurrent.futures
import errno
import fcntl
import hashlib
import json
import mmap
import os
import platform
import random
import shutil
import statistics
import struct
import subprocess
import tempfile
import threading
import time
from pathlib import Path


MIB = 1024 * 1024
FICLONE = 0x40049409
FS_IOC_FIEMAP = 0xC020660B
FIEMAP_EXTENT_LAST = 0x1
FIEMAP_EXTENT_SHARED = 0x2000


def command(*args):
    return subprocess.check_output(args, text=True, timeout=30).strip()


def elapsed(start):
    return (time.perf_counter_ns() - start) / 1e6


def percentile(values, percent):
    ordered = sorted(values)
    position = (len(ordered) - 1) * percent / 100
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def summarize_records(records):
    """Summarize repeated measurements; short groups do not claim a p95.

    Parallel arrays contain individual requests, whereas total_ms/batch_ms
    describe whole batches. Keep their sample counts and units separate.
    """
    groups = {}
    for row in records:
        if "iteration" in row:
            groups.setdefault(row["scenario"], []).append(row)
    summary = []
    for scenario, rows in groups.items():
        item = {"scenario": scenario, "n": len(rows)}
        for key, value in rows[0].items():
            if key.endswith("_ms") or key in ("extent_count", "unique_added_bytes"):
                if isinstance(value, (int, float)):
                    values = [row[key] for row in rows]
                elif isinstance(value, list) and value:
                    values = [number for row in rows for number in row[key]]
                else:
                    continue
                item[key] = {"n": len(values), "p50": statistics.median(values),
                             "min": min(values), "max": max(values),
                             "p95": percentile(values, 95) if len(values) >= 20 else None}
        summary.append(item)
    return summary


def extents(path):
    """Read physical extents so shared blocks are counted once, unlike du.

    FIEMAP reports unwritten allocations too. The resulting union is physical
    data allocation, excluding filesystem metadata and temporary CoW reserves.
    """
    result = []
    start = 0
    with path.open("rb", buffering=0) as file:
        while True:
            data = bytearray(32 + 256 * 56)
            struct.pack_into("=QQIIII", data, 0, start, (1 << 64) - 1 - start,
                             0, 0, 256, 0)
            fcntl.ioctl(file.fileno(), FS_IOC_FIEMAP, data, True)
            count = struct.unpack_from("=I", data, 20)[0]
            for index in range(count):
                logical, physical, length, _, _, flags, _, _, _ = struct.unpack_from(
                    "=QQQQQIIII", data, 32 + index * 56)
                result.append({"logical": logical, "physical": physical,
                               "length": length, "shared": bool(flags & FIEMAP_EXTENT_SHARED)})
            if not count or flags & FIEMAP_EXTENT_LAST:
                return result
            start = logical + length


def physical_union(paths):
    intervals = sorted((item["physical"], item["physical"] + item["length"])
                       for path in paths for item in extents(path))
    total, end = 0, 0
    for begin, stop in intervals:
        total += max(0, stop - max(begin, end))
        end = max(end, stop)
    return total


def reflink(source, destination, durable=True):
    """Match KumaBox's FICLONE followed by destination fsync contract."""
    with source.open("rb", buffering=0) as inp, destination.open("xb", buffering=0) as out:
        start = time.perf_counter_ns()
        fcntl.ioctl(out.fileno(), FICLONE, inp.fileno())
        clone_ms = elapsed(start)
        start = time.perf_counter_ns()
        if durable:
            os.fsync(out.fileno())
        return {"ioctl_ms": clone_ms, "sync_ms": elapsed(start)}


def write_all(file, data):
    view = memoryview(data)
    while view:
        count = file.write(view)
        if not count:
            raise OSError("short file write")
        view = view[count:]


def copy_bytes(source, destination, sparse=False):
    """Force real byte copying; copy_file_range can itself share XFS blocks."""
    with source.open("rb", buffering=0) as inp, destination.open("xb", buffering=0) as out:
        size = source.stat().st_size
        out.truncate(size)
        offset = 0
        start = time.perf_counter_ns()
        while offset < size:
            if sparse:
                try:
                    offset = os.lseek(inp.fileno(), offset, os.SEEK_DATA)
                except OSError as error:
                    if error.errno == errno.ENXIO:
                        break
                    raise
                stop = os.lseek(inp.fileno(), offset, os.SEEK_HOLE)
            else:
                stop = size
            inp.seek(offset)
            out.seek(offset)
            while offset < stop:
                data = inp.read(min(MIB, stop - offset))
                if not data:
                    raise OSError("unexpected end of source")
                write_all(out, data)
                offset += len(data)
        copy_ms = elapsed(start)
        start = time.perf_counter_ns()
        os.fsync(out.fileno())
        return {"copy_ms": copy_ms, "sync_ms": elapsed(start)}


class Experiments:
    def __init__(self, args, work):
        self.args, self.work = args, work
        self.records = []
        self.raw = (args.output / "samples.jsonl").open("x")
        self.block = os.urandom(MIB)
        self.dense = work / "dense.raw"
        self.sparse = work / "sparse.raw"

    def emit(self, scenario, **fields):
        record = {"scenario": scenario, **fields}
        self.records.append(record)
        self.raw.write(json.dumps(record) + "\n")
        self.raw.flush()

    def seed(self):
        for source, logical in ((self.dense, 256 * MIB), (self.sparse, 10 * 1024 * MIB)):
            with source.open("xb", buffering=0) as file:
                file.truncate(logical)
                for index in range(256):
                    file.seek(index * (logical // 256))
                    write_all(file, self.block)
                os.fsync(file.fileno())
        self.emit("source_layout", files=[
            {"name": path.name, "logical_bytes": path.stat().st_size,
             "allocated_bytes": path.stat().st_blocks * 512,
             "extent_count": len(extents(path))} for path in (self.dense, self.sparse)])

    def clone_and_copy(self):
        for source in (self.dense, self.sparse):
            for index in range(self.args.samples):
                # Alternate order to avoid giving one method all early samples.
                methods = ("reflink", "copy") if index % 2 == 0 else ("copy", "reflink")
                for method in methods:
                    destination = self.work / "destination.raw"
                    start = time.perf_counter_ns()
                    phases = (reflink(source, destination) if method == "reflink"
                              else copy_bytes(source, destination, sparse=source == self.sparse))
                    total = elapsed(start)
                    self.emit(f"{source.stem}_{method}", iteration=index, total_ms=total,
                              allocated_bytes=destination.stat().st_blocks * 512, **phases)
                    destination.unlink()

    def sharing(self):
        clones = []
        for index in range(16):
            destination = self.work / f"shared-{index}.raw"
            reflink(self.dense, destination)
            clones.append(destination)
        before = physical_union([self.dense, *clones])
        # Change a different 1 MiB range in each child and verify isolation.
        for index, path in enumerate(clones):
            with path.open("r+b", buffering=0) as file:
                file.seek(index * MIB)
                write_all(file, bytes([index + 1]) * MIB)
                os.fsync(file.fileno())
        after = physical_union([self.dense, *clones])
        with self.dense.open("rb") as file:
            assert file.read(MIB) == self.block, "child write changed source"
        with clones[1].open("rb") as file:
            assert file.read(MIB) == self.block, "sibling write isolation failed"
        self.emit("sharing_16", apparent_sum_bytes=sum(path.stat().st_size for path in [self.dense, *clones]),
                  du_sum_bytes=sum(path.stat().st_blocks * 512 for path in [self.dense, *clones]),
                  unique_before_bytes=before, unique_after_bytes=after, isolation_ok=True)
        for path in clones:
            path.unlink()

    def extent_shapes(self):
        # Keep both logical and written size fixed. Only the number of
        # separated data ranges changes; reflink must process their mappings.
        for count in (1, 16, 256, 1024):
            source = self.work / "shaped.raw"
            chunk = 256 * MIB // count
            with source.open("xb", buffering=0) as file:
                file.truncate(10 * 1024 * MIB)
                for index in range(count):
                    file.seek(index * (10 * 1024 * MIB // count))
                    remaining = chunk
                    while remaining:
                        data = self.block[:min(remaining, MIB)]
                        write_all(file, data)
                        remaining -= len(data)
                os.fsync(file.fileno())
            mapped = len(extents(source))
            for index in range(self.args.samples):
                destination = self.work / "shaped-clone.raw"
                start = time.perf_counter_ns()
                phases = reflink(source, destination)
                self.emit(f"shape_{count}", iteration=index, total_ms=elapsed(start),
                          extent_count=mapped, **phases)
                destination.unlink()
            source.unlink()

        other = self.work / "other-root"
        other.mkdir()
        destination = other / "cross-directory.raw"
        reflink(self.dense, destination)
        self.emit("cross_directory", same_device=self.dense.stat().st_dev == destination.stat().st_dev,
                  shared_extents=sum(item["shared"] for item in extents(destination)))
        destination.unlink()

    def cow_writes(self):
        offsets = random.Random(42).sample(range(256 * MIB // 4096), 256)
        payload = b"w" * 4096
        for iteration in range(self.args.samples):
            hints = [0, 4096, MIB]
            random.Random(iteration).shuffle(hints)
            for hint in hints:
                path = self.work / "cow-write.raw"
                reflink(self.dense, path)
                if hint:
                    command("xfs_io", "-c", f"cowextsize {hint}", str(path))
                for pass_index in (1, 2):
                    with path.open("r+b", buffering=0) as file:
                        start = time.perf_counter_ns()
                        for offset in offsets:
                            if os.pwrite(file.fileno(), payload, offset * 4096) != len(payload):
                                raise OSError("short random write")
                        write_ms = elapsed(start)
                        start = time.perf_counter_ns()
                        os.fsync(file.fileno())
                        sync_ms = elapsed(start)
                    self.emit(f"cow_hint_{hint}_pass{pass_index}", iteration=iteration,
                              write_ms=write_ms, sync_ms=sync_ms, total_ms=write_ms + sync_ms,
                              extent_count=len(extents(path)),
                              unique_added_bytes=physical_union([self.dense, path]) - 256 * MIB)
                path.unlink()

    def sync_and_direct(self):
        with mmap.mmap(-1, MIB) as aligned:
            aligned[:] = self.block
            for iteration in range(self.args.samples):
                for direct in (False, True):
                    path = self.work / "sequential.raw"
                    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
                    fd = os.open(path, flags | (os.O_DIRECT if direct else 0), 0o600)
                    try:
                        start = time.perf_counter_ns()
                        for index in range(256):
                            if os.pwrite(fd, aligned, index * MIB) != MIB:
                                raise OSError("short sequential write")
                        write_ms = elapsed(start)
                        start = time.perf_counter_ns()
                        os.fsync(fd)
                        sync_ms = elapsed(start)
                    finally:
                        os.close(fd)
                    self.emit(f"sequential_{'direct' if direct else 'buffered'}", iteration=iteration,
                              write_ms=write_ms, sync_ms=sync_ms, total_ms=write_ms + sync_ms)
                    path.unlink()
                # Same bytes and final durability; vary only flush frequency.
                for batch in (1, 64, 256):
                    path = self.work / "flush.raw"
                    with path.open("xb", buffering=0) as file:
                        start = time.perf_counter_ns()
                        for index in range(256):
                            write_all(file, self.block[:4096])
                            if (index + 1) % batch == 0:
                                os.fsync(file.fileno())
                        self.emit(f"fsync_batch_{batch}", iteration=iteration, total_ms=elapsed(start))
                    path.unlink()

    def reads(self):
        with mmap.mmap(-1, MIB) as aligned:
            for iteration in range(self.args.samples):
                for mode in ("warm", "evict_advisory", "direct"):
                    fd = os.open(self.dense, os.O_RDONLY | (os.O_DIRECT if mode == "direct" else 0))
                    try:
                        if mode == "warm":
                            for offset in range(0, 256 * MIB, MIB):
                                os.preadv(fd, [aligned], offset)
                        elif mode == "evict_advisory":
                            os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
                        start = time.perf_counter_ns()
                        for offset in range(0, 256 * MIB, MIB):
                            if os.preadv(fd, [aligned], offset) != MIB:
                                raise OSError("short sequential read")
                        self.emit(f"read_{mode}", iteration=iteration, total_ms=elapsed(start))
                    finally:
                        os.close(fd)

    def parallel_clones(self):
        for count in (1, 4, 16):
            for iteration in range(self.args.samples):
                barrier = threading.Barrier(count + 1)

                def clone(index):
                    path = self.work / f"parallel-{index}.raw"
                    barrier.wait(timeout=30)
                    start = time.perf_counter_ns()
                    reflink(self.dense, path)
                    return elapsed(start)

                with concurrent.futures.ThreadPoolExecutor(max_workers=count) as pool:
                    futures = [pool.submit(clone, index) for index in range(count)]
                    start = time.perf_counter_ns()
                    barrier.wait(timeout=30)
                    times = [future.result() for future in futures]
                    self.emit(f"parallel_reflink_{count}", iteration=iteration,
                              total_ms=elapsed(start), individual_ms=times)
                for index in range(count):
                    (self.work / f"parallel-{index}.raw").unlink()

    def cache_identity(self):
        # Warm the source, then compare two ways to refer to the same bytes.
        # Hard links preserve the inode/page cache; reflinks get a new inode.
        with mmap.mmap(-1, MIB) as aligned:
            for iteration in range(self.args.samples):
                for kind in ("hardlink", "reflink"):
                    with self.dense.open("rb", buffering=0) as source:
                        for offset in range(0, 256 * MIB, MIB):
                            os.preadv(source.fileno(), [aligned], offset)
                    destination = self.work / "cache-identity.raw"
                    if kind == "hardlink":
                        os.link(self.dense, destination)
                    else:
                        reflink(self.dense, destination)
                    with destination.open("rb", buffering=0) as file:
                        start = time.perf_counter_ns()
                        for offset in range(0, 256 * MIB, MIB):
                            if os.preadv(file.fileno(), [aligned], offset) != MIB:
                                raise OSError("short identity read")
                        self.emit(f"cache_identity_{kind}", iteration=iteration, total_ms=elapsed(start),
                                  same_inode=destination.stat().st_ino == self.dense.stat().st_ino)
                    destination.unlink()

    def cross_mount(self):
        with tempfile.TemporaryDirectory(prefix="kumabox-xfs-cross-") as temporary:
            destination = Path(temporary) / "cross.raw"
            if destination.parent.stat().st_dev == self.work.stat().st_dev:
                self.emit("cross_mount", skipped="temporary directory is on the same filesystem")
                return
            try:
                reflink(self.dense, destination)
            except OSError as error:
                self.emit("cross_mount", errno=error.errno, message=str(error))
            else:
                raise AssertionError("unexpected cross-filesystem FICLONE success")

    def finish(self):
        self.raw.close()
        summary = summarize_records(self.records)
        (self.args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True, help="existing XFS directory")
    parser.add_argument("--output", type=Path, required=True, help="new report directory")
    parser.add_argument("--samples", type=int, default=20)
    stages = ("clone_and_copy", "sharing", "extent_shapes", "cow_writes", "sync_and_direct",
              "reads", "cache_identity", "parallel_clones", "cross_mount")
    parser.add_argument("--stages", default=",".join(stages), help="comma-separated experiments")
    args = parser.parse_args()
    if platform.system() != "Linux" or args.samples < 1 or any(stage not in stages for stage in args.stages.split(",")):
        parser.error("requires Linux and a positive sample count")
    if command("findmnt", "-n", "-o", "FSTYPE", "-T", str(args.directory)) != "xfs":
        parser.error("--directory must reside on XFS")
    if shutil.disk_usage(args.directory).free < 8 * 1024 * MIB:
        parser.error("requires at least 8 GiB free space")
    args.output.mkdir(parents=True, exist_ok=False, mode=0o700)
    shutil.copyfile(__file__, args.output / "harness.py")
    metadata = {"started_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "platform": platform.platform(), "samples": args.samples,
                "stages": args.stages,
                "mount": command("findmnt", "-T", str(args.directory)),
                "xfs_info": command("xfs_info", command("findmnt", "-n", "-o", "TARGET", "-T", str(args.directory))),
                "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "contract": "monotonic wall time; destination fsync included; setup and deletion excluded; no global cache drop"}
    (args.output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    with tempfile.TemporaryDirectory(prefix="kumabox-xfs-private-", dir=args.directory) as temporary:
        experiments = Experiments(args, Path(temporary))
        try:
            print(f"{time.strftime('%H:%M:%S')} seed", flush=True)
            experiments.seed()
            for stage in args.stages.split(","):
                print(f"{time.strftime('%H:%M:%S')} {stage}", flush=True)
                getattr(experiments, stage)()
        finally:
            experiments.finish()
    print(f"results: {args.output}", flush=True)


if __name__ == "__main__":
    main()
