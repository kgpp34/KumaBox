#!/usr/bin/env python3
"""Test reflink ENOSPC behavior inside a disposable 1.5 GiB loopback XFS.

Requires root. Formats only a newly created regular file, never a block device.
The host filesystem, mount configuration, and existing files stay unchanged.
This is a capacity/correctness experiment, not a physical-device benchmark.
"""

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

from xfs import MIB, command, reflink, write_all


def digest(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root to mount the disposable loopback image")
    if shutil.disk_usage(args.directory).free < 4 * 1024 * MIB:
        parser.error("requires 4 GiB free host space")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix="kumabox-xfs-pressure-", dir=args.directory))
    image, mount = work / "private.img", work / "mount"
    mounted = False
    result = {"loopback_image_bytes": 1536 * MIB, "physical_device_benchmark": False}
    try:
        with image.open("xb") as file:
            file.truncate(1536 * MIB)
        mount.mkdir(mode=0o700)
        result["mkfs"] = command("mkfs.xfs", "-m", "reflink=1", str(image))
        subprocess.run(["mount", "-o", "loop,nodev,nosuid,noexec", str(image), str(mount)],
                       check=True, timeout=30)
        mounted = True
        source = mount / "golden.raw"
        payload = os.urandom(MIB)
        with source.open("xb", buffering=0) as file:
            for _ in range(128):
                write_all(file, payload)
            os.fsync(file.fileno())
        original = digest(source)
        result["initial_free_bytes"] = shutil.disk_usage(mount).free
        with (mount / "filler.raw").open("xb", buffering=0) as file:
            # Reserve capacity without generating a full-device write load.
            # Explicit unwritten extents consume loopback filesystem space.
            os.posix_fallocate(file.fileno(), 0, shutil.disk_usage(mount).free - 32 * MIB)
            os.fsync(file.fileno())
        result["free_before_clones_bytes"] = shutil.disk_usage(mount).free
        clones = []
        for index in range(16):
            path = mount / f"clone-{index}.raw"
            try:
                reflink(source, path)
            except OSError as error:
                result["clone_error"] = {"errno": error.errno, "message": str(error)}
                break
            clones.append(path)
        result["successful_clones"] = len(clones)
        result["free_after_clones_bytes"] = shutil.disk_usage(mount).free
        written = 0
        if clones:
            try:
                with clones[0].open("r+b", buffering=0) as file:
                    for _ in range(64):
                        # Flush each MiB so delayed allocation cannot postpone
                        # ENOSPC until after the test declares a write success.
                        write_all(file, b"x" * MIB)
                        os.fsync(file.fileno())
                        written += MIB
            except OSError as error:
                result["write_error"] = {"errno": error.errno, "message": str(error)}
        result["requested_overwrite_bytes"] = 64 * MIB
        result["completed_durable_overwrite_bytes"] = written
        result["golden_unchanged"] = digest(source) == original
        result["free_after_write_bytes"] = shutil.disk_usage(mount).free
        assert result["golden_unchanged"], "CoW write modified the golden file"
    finally:
        (args.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        if mounted:
            # If unmount fails, retain image and directory for recovery rather
            # than deleting a live mount's backing storage.
            subprocess.run(["umount", str(mount)], check=True, timeout=30)
        shutil.rmtree(work)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
