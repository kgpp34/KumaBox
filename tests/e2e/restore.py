#!/usr/bin/env python3
"""Verify memory restore policy and immutable directory lifetime on Linux/KVM.

Requires root and a pre-imported image with the guest agent. Uses only uniquely
named test resources; never removes other VMs, images, or host caches.
"""

import argparse
import json
import os
import shutil
import subprocess
import tempfile
import time
import uuid
from pathlib import Path

from guest_operations import GuestTests


class RestoreTests(GuestTests):
    def __init__(self, args):
        super().__init__(args)
        self.prefix = "restore-e2e-" + uuid.uuid4().hex[:8]
        self.snapshots = []
        self.serial = 0

    def ready(self, name):
        deadline = time.monotonic() + 30
        while True:
            try:
                self.call("exec", name, "--", "/bin/true", timeout=5)
                return
            except (RuntimeError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.01)

    def prepare(self):
        self.name = self.prefix + "-source"
        self.created.append(self.name)
        self.call("run", self.args.image, "--name", self.name, "--cpus", "2",
                  "--memory", "512MiB", "--nics", str(self.args.nics),
                  "--data-disk", "name=bench,size=32MiB", "--json", timeout=120)
        self.ready(self.name)
        if self.args.nics:
            deadline = time.monotonic() + 30
            while b"default" not in self.guest("ip", "-4", "route"):
                if time.monotonic() >= deadline:
                    raise TimeoutError("source network did not become ready")
                time.sleep(0.05)
        self.guest("sh", "-ec", "mkdir -p /mnt/bench; "
                   "for i in $(seq 1 100); do "
                   "test -b /dev/disk/by-id/virtio-bench && break; sleep 0.1; done; "
                   "mount /dev/disk/by-id/virtio-bench /mnt/bench; "
                   "printf golden > /root/restore-e2e; "
                   "printf golden > /run/restore-e2e; "
                   "printf golden > /mnt/bench/restore-e2e; sync")
        self.capture = self.prefix + "-golden"
        self.snapshots.append(self.capture)
        self.call("snapshot", "save", self.name, "--name", self.capture, "--json", timeout=120)

    def assert_state(self, name):
        output = self.call("exec", name, "--", "sh", "-ec",
                           "cat /root/restore-e2e /run/restore-e2e /mnt/bench/restore-e2e")
        assert output == b"goldengoldengolden", output

    def modes(self):
        monitor = os.environ.get("KUMABOX_VMM_CLOUD_HYPERVISOR_BINARY", "cloud-hypervisor")
        version = subprocess.check_output([monitor, "--version"], text=True)
        modes = ["copy", "ondemand", "auto"]
        if "v54." in version:
            modes.append("mmap")
        for mode in modes:
            self.guest("sh", "-ec", "printf changed > /root/restore-e2e; "
                       "printf changed > /run/restore-e2e; "
                       "printf changed > /mnt/bench/restore-e2e; sync")
            self.call("restore", self.name, self.capture, "--restore-mode", mode, "--json", timeout=120)
            self.assert_state(self.name)

    def rejected_mode(self):
        before = json.loads(self.call("inspect", self.name))
        monitor = os.environ.get("KUMABOX_VMM_CLOUD_HYPERVISOR_BINARY", "cloud-hypervisor")
        version = subprocess.check_output([monitor, "--version"], text=True)
        # This lab runs v53, which cannot mmap snapshots. The unit suite covers
        # v54 selection separately; do not deliberately reject a supported mode.
        mode = "mmap" if "v53." in version else "invalid"
        self.call("restore", self.name, self.capture, "--restore-mode", mode, expect=None)
        after = json.loads(self.call("inspect", self.name))
        assert before["generation"] == after["generation"]
        assert after["state"] == "running"
        self.assert_state(self.name)

    def clone_directory(self, directory):
        self.serial += 1
        name = f"{self.prefix}-child-{self.serial}"
        self.created.append(name)
        record = json.loads(self.call("clone", "--from-dir", str(directory), "--name", name,
                                      "--wait-network", "--json", timeout=120))
        self.ready(name)
        if self.args.nics:
            assert b"default" in self.call("exec", name, "--", "ip", "-4", "route")
            assert self.call("exec", name, "--", "hostname").strip() == name.encode()
        return name, record

    def directory_lifetime(self, parent):
        directory = parent / "golden"
        self.call("snapshot", "export", self.capture, "--to-dir", str(directory))
        first, first_record = self.clone_directory(directory)
        second, second_record = self.clone_directory(directory)
        native = Path(self.args.root_dir) / "vmm"
        for memory in directory.glob("memory-range*"):
            for record in (first_record, second_record):
                target = native / record["id"] / "clone-state" / memory.name
                assert os.path.samefile(memory, target), f"memory cache not shared: {target}"
        self.call("exec", first, "--", "sh", "-ec", "printf child > /root/restore-e2e; "
                  "printf child > /run/restore-e2e; printf child > /mnt/bench/restore-e2e; sync")
        self.assert_state(second)
        self.assert_state(self.name)
        shutil.rmtree(directory)
        self.call("stop", first)
        self.call("rm", first)
        self.created.remove(first)
        self.call("exec", second, "--", "sh", "-ec", "echo 3 > /proc/sys/vm/drop_caches")
        self.assert_state(second)

    def cross_filesystem(self):
        with tempfile.TemporaryDirectory(prefix=self.prefix, dir="/dev/shm") as parent:
            directory = Path(parent) / "golden"
            self.call("snapshot", "export", self.capture, "--to-dir", str(directory), timeout=120)
            name, record = self.clone_directory(directory)
            for memory in directory.glob("memory-range*"):
                target = Path(self.args.root_dir) / "vmm" / record["id"] / "clone-state" / memory.name
                assert memory.stat().st_dev != target.stat().st_dev, "cross-filesystem fixture is invalid"
                assert target.is_file() and not target.is_symlink()
            shutil.rmtree(directory)
            self.assert_state(name)

    def malformed_source(self, parent):
        directory = parent / "incomplete"
        self.call("snapshot", "export", self.capture, "--to-dir", str(directory))
        (directory / "state.json").unlink()
        before = json.loads(self.call("inspect", self.name))
        self.call("restore", self.name, "--from-dir", str(directory), expect=None)
        after = json.loads(self.call("inspect", self.name))
        assert before["generation"] == after["generation"]
        self.assert_state(self.name)

    def shared_memory(self):
        name = self.prefix + "-shared"
        self.created.append(name)
        self.call("run", self.args.image, "--name", name, "--cpus", "2",
                  "--memory", "512MiB", "--nics", "0", "--shared-memory", "--json", timeout=120)
        self.ready(name)
        self.call("exec", name, "--", "sh", "-ec", "printf shared > /run/restore-e2e")
        capture = self.prefix + "-shared-golden"
        self.snapshots.append(capture)
        self.call("snapshot", "save", name, "--name", capture, "--json", timeout=120)
        self.call("restore", name, capture, "--restore-mode", "auto", "--json", timeout=120)
        assert self.call("exec", name, "--", "cat", "/run/restore-e2e") == b"shared"
        before = json.loads(self.call("inspect", name))
        self.call("restore", name, capture, "--restore-mode", "ondemand", expect=None)
        after = json.loads(self.call("inspect", name))
        assert before["generation"] == after["generation"]

    def run(self):
        failure = None
        try:
            self.check("prepare RAM, root disk and managed data disk", self.prepare)
            self.check("selected memory modes restore RAM and both disks", self.modes)
            self.check("unsupported mode leaves original sandbox running", self.rejected_mode)
            with tempfile.TemporaryDirectory(prefix=self.prefix, dir=self.args.workdir) as parent:
                self.check("directory siblings share memory, isolate writes and survive source removal",
                           lambda: self.directory_lifetime(Path(parent)))
                self.check("malformed directory does not stop original sandbox",
                           lambda: self.malformed_source(Path(parent)))
            self.check("cross-filesystem copy survives source removal", self.cross_filesystem)
            self.check("shared memory uses copy and rejects incompatible lazy restore", self.shared_memory)
        except Exception as error:
            failure = str(error)
        finally:
            try:
                super().cleanup()
                for name in self.snapshots:
                    self.call("snapshot", "rm", name)
            except Exception as error:
                failure = f"{failure or ''}; cleanup: {error}"
            Path(self.args.report).write_text(json.dumps({"checks": self.results, "error": failure}, indent=2) + "\n")
        if failure:
            raise RuntimeError(failure)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--nics", type=int, choices=(0, 1), default=0)
    parser.add_argument("--root-dir", required=True)
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--log-dir", required=True)
    parser.add_argument("--workdir", required=True, help="existing directory on the data filesystem")
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root on the KVM test host")
    RestoreTests(args).run()


if __name__ == "__main__":
    main()
