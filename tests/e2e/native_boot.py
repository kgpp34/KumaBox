#!/usr/bin/env python3
"""Verify native PID 1, supervision, ACPI shutdown and snapshot networking.

Run as root on the Linux/KVM test host with a native-v1 runtime image.
Only this run's uniquely named VMs and snapshots are removed.
"""

import argparse
import json
import os
import pty
import select
import subprocess
import time
import uuid
from pathlib import Path

from guest_operations import GuestTests


class NativeBootTests(GuestTests):
    def __init__(self, args):
        super().__init__(args)
        self.prefix = "native-e2e-" + uuid.uuid4().hex[:8]
        self.snapshots = []

    def create(self, nics):
        super().create(nics, "--data-disk", "name=runtime-data,size=32MiB")
        self.mount_data()

    def mount_data(self):
        self.guest("sh", "-ec", "mkdir -p /mnt/runtime-data; "
                   "mountpoint -q /mnt/runtime-data || mount /dev/disk/by-id/virtio-runtime-data /mnt/runtime-data")

    def ready(self):
        deadline = time.monotonic() + 30
        while True:
            try:
                self.guest("/bin/true", timeout=3)
                return
            except (RuntimeError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.01)

    def native_contract(self):
        assert self.native, "requires native-v1 image"
        self.guest("sh", "-ec", "test ! -d /run/systemd; test ! -x /usr/lib/systemd/systemd; "
                   "test -c /dev/ptmx; test -r /sys/fs/cgroup/cgroup.controllers; "
                   "test -d /dev/shm; test -d /run/kumabox; "
                   "mount | grep -q 'tmpfs on /tmp'; "
                   "test $(wc -c < /etc/machine-id) -eq 33")
        self.guest("script", "-qec", "test -t 0 && test -t 1", "/dev/null")
        self.file("write", "/tmp/early-native-upload", data=b"ready without cleanup")
        assert self.file("read", "/tmp/early-native-upload") == b"ready without cleanup"

    def orphan_reaping(self):
        pid = self.guest("sh", "-c", "(sleep 0.2) </dev/null >/dev/null 2>&1 & echo $!").strip().decode()
        deadline = time.monotonic() + 5
        while True:
            exists = self.guest("sh", "-c", 'test ! -e "/proc/$1" && printf gone || true', "sh", pid)
            if exists == b"gone":
                return
            if time.monotonic() >= deadline:
                raise AssertionError(f"orphan {pid} retained, including as zombie")
            time.sleep(0.02)

    def console(self):
        master, slave = pty.openpty()
        process = subprocess.Popen(self.command + ["console", self.name], stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        output = b""
        try:
            deadline = time.monotonic() + 5
            while b"Connected to" not in output:
                if time.monotonic() >= deadline:
                    raise AssertionError(f"console attach: {output!r}")
                if select.select([master], [], [], 0.1)[0]:
                    output += os.read(master, 65536)
            os.write(master, b"printf 'native-console-%s\\n' ready\n")
            while b"native-console-ready" not in output:
                if time.monotonic() >= deadline:
                    raise AssertionError(f"console shell: {output!r}")
                if select.select([master], [], [], 0.1)[0]:
                    output += os.read(master, 65536)
            os.write(master, b"\x1d.")
            assert process.wait(timeout=5) == 0
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            os.close(master)

    def agent_restart(self):
        pid = self.guest("sh", "-ec", 'for f in /proc/[0-9]*/comm; do '
                         'read n < "$f" || continue; if [ "$n" = kumabox-agent ]; then '
                         'p=${f#/proc/}; echo "${p%/comm}"; break; fi; done').strip()
        assert pid.isdigit()
        # The killer must outlive its exec connection and the dying agent.
        self.guest("sh", "-c", '(sleep 0.2; kill -KILL "$1") </dev/null >/dev/null 2>&1 &', "sh", pid.decode())
        time.sleep(0.4)
        self.ready()
        self.guest("sh", "-ec", 'test ! -e "/proc/$1"', "sh", pid.decode())
        self.orphan_reaping()

    def normal_shutdown(self):
        identity = self.guest("cat", "/etc/machine-id")
        self.guest("sh", "-ec", "printf persisted > /root/native-persistence; "
                   "printf data > /mnt/runtime-data/persistence; "
                   "printf '#!/bin/sh\nprintf installed' > /usr/local/bin/native-installed; "
                   "chmod 755 /usr/local/bin/native-installed; "
                   "rm -f /root/native-clean /mnt/runtime-data/clean; "
                   "(trap 'printf clean > /root/native-clean; printf clean > /mnt/runtime-data/clean; exit 0' TERM; "
                   "touch /run/native-shutdown-armed; while :; do sleep 1; done) "
                   "</dev/null >/dev/null 2>&1 &")
        self.guest("sh", "-ec", "while [ ! -f /run/native-shutdown-armed ]; do sleep 0.01; done")
        self.call("stop", self.name, "--timeout", "10", "--json", timeout=20)
        self.call("start", self.name, "--json")
        self.ready()
        self.mount_data()
        assert self.guest("cat", "/mnt/runtime-data/clean") == b"clean"
        assert self.guest("cat", "/mnt/runtime-data/persistence") == b"data"
        assert self.guest("/usr/local/bin/native-installed") == b"installed"
        assert self.guest("cat", "/root/native-clean") == b"clean", "normal stop escalated without guest shutdown"
        assert self.guest("cat", "/root/native-persistence") == b"persisted"
        assert self.guest("cat", "/etc/machine-id") == identity
        self.guest("sh", "-ec", "test ! -e /tmp/early-native-upload; test ! -e /run/native-shutdown-armed")

    def snapshots_and_network(self, nics):
        source = self.name
        snapshot = f"{self.prefix}-golden-n{nics}"
        self.snapshots.append(snapshot)
        identity = self.guest("cat", "/etc/machine-id")
        self.guest("sh", "-ec", "printf captured > /root/native-snapshot; sync")
        if nics:
            # Capture a stale address to force the agent's direct netlink
            # reconciliation, rather than only its unchanged-identity fast path.
            self.guest("ip", "addr", "add", "198.18.0.2/32", "dev", "eth0")
        self.call("snapshot", "save", source, "--name", snapshot, "--json", timeout=90)
        if nics == 0:
            # A golden may be older than its clone. No time daemon is present:
            # verify wall time and timers rather than silently freezing at capture.
            time.sleep(8)
        clone = f"{self.prefix}-clone-n{nics}"
        self.created.append(clone)
        self.call("clone", snapshot, "--name", clone, "--wait-network", "--json", timeout=90)
        self.name = clone
        self.ready()
        guest_time = int(self.guest("date", "+%s").strip())
        assert abs(guest_time - time.time()) < 3, (guest_time, time.time())
        assert self.guest("cat", "/root/native-snapshot") == b"captured"
        assert self.guest("cat", "/etc/machine-id") != identity
        if nics:
            self.guest("sh", "-ec", "! ip -4 addr show dev eth0 | grep -q 198.18.0.2")
            self.guest("sh", "-ec", "ip -4 route | grep -q default; "
                       "getent ahostsv4 example.com; curl -fsSI --max-time 15 https://example.com")
        self.guest("sh", "-ec", "printf private > /root/native-snapshot")
        self.orphan_reaping()
        self.name = source
        assert self.guest("cat", "/root/native-snapshot") == b"captured"
        self.guest("sh", "-ec", "printf changed > /root/native-snapshot; sync")
        self.call("restore", source, snapshot, "--json", timeout=90)
        self.ready()
        guest_time = int(self.guest("date", "+%s").strip())
        assert abs(guest_time - time.time()) < 3, (guest_time, time.time())
        assert self.guest("cat", "/root/native-snapshot") == b"captured"
        self.normal_shutdown()

    def run(self):
        failure = None
        try:
            for nics in self.args.nics:
                self.check(f"create native image ({nics} NIC)", lambda: self.create(nics))
                for label, action in (("mounts, PTY and early uploads", self.native_contract),
                                      ("console shell and detach", self.console),
                                      ("adopt and reap orphans", self.orphan_reaping),
                                      ("restart crashed agent", self.agent_restart),
                                      ("ACPI shutdown, sync and persistence", self.normal_shutdown),
                                      ("golden clone, restore, network and shutdown", lambda: self.snapshots_and_network(nics))):
                    self.check(f"{label} ({nics} NIC)", action)
        except Exception as error:
            failure = error
        finally:
            try:
                self.cleanup()
                for snapshot in self.snapshots:
                    self.call("snapshot", "rm", snapshot)
            except Exception as error:
                failure = failure or error
            report = {"passed": failure is None, "checks": self.results, "error": str(failure) if failure else None}
            Path(self.args.report).write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2), flush=True)
        if failure:
            raise SystemExit(1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="kumabox")
    parser.add_argument("--image", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--nics", nargs="+", type=int, choices=(0, 1), default=[0, 1])
    parser.add_argument("--network-backend", default="ebpf", choices=("ebpf",))
    parser.add_argument("--socket")
    parser.add_argument("--run-dir")
    parser.add_argument("--report", default="/tmp/kumabox-native-boot-e2e.json")
    NativeBootTests(parser.parse_args()).run()


if __name__ == "__main__":
    main()
