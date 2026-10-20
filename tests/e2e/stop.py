#!/usr/bin/env python3
"""Verify normal shutdown, forced stop, cancellation and network recovery.

Uses uniquely named VMs on a Linux/KVM host. The image must contain systemd,
systemd-logind and the guest agent. No guest-agent rebuild is required. Only
resources created by this run are removed; existing images/VMs are untouched.
"""

import argparse
import json
import os
import signal
import subprocess
import time
import uuid
from pathlib import Path

from guest_operations import GuestTests


class StopTests(GuestTests):
    def __init__(self, args):
        super().__init__(args)
        self.prefix = "stop-e2e-" + uuid.uuid4().hex[:8]
        self.timings = []
        self.record = None

    def create(self, nics):
        self.name = f"{self.prefix}-n{nics}"
        self.created.append(self.name)
        self.record = json.loads(self.call(
            "run", self.args.image, "--name", self.name, "--cpus", "2",
            "--memory", "512MiB", "--nics", str(nics),
            "--data-disk", "name=shutdown,size=32MiB", "--json", timeout=120,
        ))
        self.wait_ready()
        self.guest("sh", "-ec", "systemctl start systemd-logind; "
                   "for i in $(seq 1 100); do "
                   "systemctl is-active --quiet multi-user.target && exit 0; sleep 0.1; done; exit 1")
        self.guest("sh", "-ec", "mkdir -p /mnt/shutdown; "
                   "mount /dev/disk/by-id/virtio-shutdown /mnt/shutdown; "
                   "printf persistent > /root/stop-persistent; "
                   "printf persistent > /mnt/shutdown/stop-persistent; "
                   "printf '#!/bin/sh\\nprintf installed' > /usr/local/bin/stop-installed; "
                   "chmod 755 /usr/local/bin/stop-installed; sync")
        unit = (
            "[Unit]\nDescription=KumaBox shutdown verification\n"
            "[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\n"
            "ExecStop=/bin/sh -c 'printf clean > /root/stop-clean; "
            "printf clean > /mnt/shutdown/stop-clean'\n"
        )
        self.guest("sh", "-ec", 'printf "%s" "$1" > /etc/systemd/system/stop-verification.service; '
                   "systemctl daemon-reload; systemctl start stop-verification.service", "sh", unit)

    def wait_ready(self):
        started = time.perf_counter()
        deadline = time.monotonic() + 45
        while True:
            try:
                self.guest("/bin/true", timeout=5)
                break
            except (RuntimeError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.01)
        exec_ms = (time.perf_counter() - started) * 1000
        network_ms = None
        if self.record["nics"]:
            gateway = self.record["network"]["interfaces"][0]["ipv4"]["gateway"]
            while True:
                try:
                    self.guest("sh", "-ec", 'ip -4 route | grep -q default; ping -c 1 -W 1 "$1"', "sh", gateway, timeout=5)
                    break
                except (RuntimeError, subprocess.TimeoutExpired):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.05)
            network_ms = (time.perf_counter() - started) * 1000
        return exec_ms, network_ms

    def restart(self):
        started = time.perf_counter()
        self.call("start", self.name, "--json", timeout=60)
        returned_ms = (time.perf_counter() - started) * 1000
        exec_ms, network_ms = self.wait_ready()
        self.timings.append({"operation": "start", "nics": self.record["nics"],
                             "return_ms": returned_ms, "first_exec_ms": returned_ms + exec_ms,
                             "network_ms": None if network_ms is None else returned_ms + network_ms})
        self.guest("sh", "-ec", "mount /dev/disk/by-id/virtio-shutdown /mnt/shutdown; "
                   "test \"$(cat /root/stop-persistent)\" = persistent; "
                   "test \"$(cat /mnt/shutdown/stop-persistent)\" = persistent; "
                   "test \"$(stop-installed)\" = installed")

    def stopped(self):
        record = json.loads(self.call("inspect", self.name))
        assert record["state"] == "stopped", record
        scope = Path("/sys/fs/cgroup/kumabox.slice") / f"sandbox-{record['id']}.scope"
        assert not scope.exists(), f"retained cgroup: {scope}"
        for interface in self.record.get("network", {}).get("interfaces", []):
            reply = subprocess.run(["ip", "-n", "kumabox-" + record["id"], "-j", "link", "show", interface["name"]], capture_output=True)
            assert reply.returncode == 0, reply.stderr
            assert "UP" not in json.loads(reply.stdout)[0]["flags"], reply.stdout
        return record

    def arm_marker(self):
        self.guest("sh", "-ec", "rm -f /root/stop-clean /mnt/shutdown/stop-clean; "
                   "systemctl start stop-verification.service; sync")

    def normal(self):
        started = time.perf_counter()
        self.call("stop", self.name, "--json", timeout=45)
        self.timings.append({"operation": "stop-normal", "nics": self.record["nics"],
                             "return_ms": (time.perf_counter() - started) * 1000})
        self.stopped()
        self.restart()
        self.guest("sh", "-ec", "test \"$(cat /root/stop-clean)\" = clean; "
                   "test \"$(cat /mnt/shutdown/stop-clean)\" = clean")

    def force(self):
        self.arm_marker()
        self.call("stop", self.name, "--force", "--timeout", "30", "--json", timeout=12)
        self.stopped()
        self.restart()
        self.guest("sh", "-ec", "test ! -e /root/stop-clean; test ! -e /mnt/shutdown/stop-clean")

    def block_power_button(self):
        self.guest("systemctl", "start", "systemd-logind")
        self.call("exec", "-d", self.name, "--", "systemd-inhibit", "--what=handle-power-key",
                  "--mode=block", "--who=kumabox-stop-e2e", "sleep", "infinity")
        deadline = time.monotonic() + 10
        while b"kumabox-stop-e2e" not in self.guest("systemd-inhibit", "--list", "--no-pager"):
            if time.monotonic() >= deadline:
                raise TimeoutError("power-key inhibitor did not activate")
            time.sleep(0.02)

    def timeout(self):
        self.arm_marker()
        self.block_power_button()
        started = time.perf_counter()
        self.call("stop", self.name, "--timeout", "1", "--json", timeout=12)
        elapsed = time.perf_counter() - started
        assert elapsed >= 0.9, f"normal shutdown window was skipped: {elapsed}"
        self.timings.append({"operation": "stop-timeout-1s", "nics": self.record["nics"], "return_ms": elapsed * 1000})
        self.stopped()
        self.restart()
        self.guest("test", "!", "-e", "/root/stop-clean")

    def cancel(self):
        self.arm_marker()
        self.block_power_button()
        process = subprocess.Popen(self.command + ["stop", self.name, "--timeout", "30", "--json"],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 10
            while True:
                records = json.loads(self.call("ps", "--all", "--json"))
                target = next(r for r in records if r["name"] == self.name)
                if target["state"] == "stopping":
                    break
                if process.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError("stop did not enter Stopping")
                time.sleep(0.02)
            time.sleep(0.1)
            process.send_signal(signal.SIGINT)
            _, stderr = process.communicate(timeout=5)
            assert process.returncode != 0, stderr
            record = json.loads(self.call("inspect", self.name))
            assert record["state"] == "stopping", record
            scope = Path("/sys/fs/cgroup/kumabox.slice") / f"sandbox-{record['id']}.scope"
            assert (scope / "cgroup.procs").read_text().strip(), "cancelled stop killed VMM"
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()
        self.call("stop", self.name, "--force", "--json", timeout=12)
        self.stopped()
        self.restart()
        self.guest("test", "!", "-e", "/root/stop-clean")

    def idempotent(self):
        self.call("stop", self.name, "--force", "--json", timeout=12)
        before = self.stopped()
        self.call("stop", self.name, "--json")
        after = self.stopped()
        assert before["generation"] == after["generation"]

    def run(self):
        try:
            for nics in self.args.nics:
                self.check(f"create-{nics}nic", lambda n=nics: self.create(n))
                for label, action in (("normal shutdown and persistence", self.normal),
                                      ("force skips guest hooks", self.force),
                                      ("timeout escalates", self.timeout),
                                      ("cancellation and retry", self.cancel),
                                      ("idempotent stop", self.idempotent)):
                    self.check(f"{label}-{nics}nic", action)
        finally:
            for name in reversed(self.created):
                try:
                    self.call("stop", name, "--force", timeout=12)
                    self.call("rm", name, timeout=30)
                except Exception as error:
                    self.results.append({"name": "cleanup", "sandbox": name, "passed": False, "error": str(error)})
            Path(self.args.report).write_text(json.dumps({"results": self.results, "timings": self.timings}, indent=2) + "\n")
        if any(not r["passed"] for r in self.results):
            raise RuntimeError("stop E2E had failures")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("binary", "image", "root-dir", "run-dir", "log-dir", "report"):
        parser.add_argument("--" + option, required=True)
    parser.add_argument("--nics", type=int, nargs="+", choices=(0, 1), default=[0, 1])
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root on the KVM test host")
    StopTests(args).run()


if __name__ == "__main__":
    main()
