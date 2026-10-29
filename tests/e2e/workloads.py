#!/usr/bin/env python3
"""Verify separate Python/Node images and application-ready golden clones.

Requires Linux/KVM, the daemon, eBPF networking and the two imported images.
It removes only its own sandboxes and snapshots.
"""

import argparse
import json
import time
import uuid
from pathlib import Path

from guest_operations import GuestTests


class WorkloadTests(GuestTests):
    def __init__(self, args):
        super().__init__(args)
        self.prefix = "workload-e2e-" + uuid.uuid4().hex[:8]
        self.snapshots = []

    def http_ready(self):
        deadline = time.monotonic() + 20
        while True:
            try:
                assert self.guest("curl", "-fsS", "--max-time", "2", "http://127.0.0.1:8000/") == b"application ready"
                return
            except (RuntimeError, AssertionError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.02)

    def workload(self, language, image):
        source = f"{self.prefix}-{language}"
        self.name = source
        self.created.append(source)
        self.call("run", image, "--name", source, "--cpus", "1", "--memory", "512MiB",
                  "--nics", "1", "--network-backend", "ebpf", "--cow-io", "buffered", "--json")
        deadline = time.monotonic() + 20
        while True:
            try:
                self.guest("/bin/true", timeout=3)
                break
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.02)
        if language == "python":
            self.guest("python", "-m", "venv", "/root/test-venv")
            self.guest("/root/test-venv/bin/python", "-c", "import ssl,sys; assert sys.version_info[:2] == (3,12)")
            self.guest("sh", "-ec", "mkdir -p /root/http; printf 'application ready' > /root/http/index.html")
            command = ["python", "-m", "http.server", "8000", "--bind", "127.0.0.1", "--directory", "/root/http"]
        else:
            self.guest("node", "-e", "if (!process.version.startsWith('v22.')) process.exit(1)")
            self.guest("npm", "--version")
            command = ["node", "-e", "require('http').createServer((req,res)=>res.end('application ready')).listen(8000,'127.0.0.1')"]
        self.call("exec", "--detach", "--json", source, "--", *command)
        self.http_ready()
        snapshot = f"{source}-golden"
        self.snapshots.append(snapshot)
        self.call("snapshot", "save", source, "--name", snapshot, "--json", timeout=90)
        clone = f"{source}-clone"
        self.created.append(clone)
        self.call("clone", snapshot, "--name", clone, "--wait-network", "--json", timeout=90)
        self.name = clone
        # Do not restart the application: the captured server must be usable.
        self.http_ready()
        self.guest("sh", "-ec", "getent ahostsv4 example.com; curl -fsSI --max-time 15 https://example.com")
        self.call("stop", clone, "--timeout", "10", "--json", timeout=20)
        self.name = source
        self.http_ready()

    def run(self):
        failure = None
        try:
            for language, image in (("python", self.args.python_image), ("node", self.args.node_image)):
                self.check(f"{language} runtime, warm application golden, clone and network", lambda: self.workload(language, image))
        except Exception as error:
            failure = error
        finally:
            errors = []
            try:
                self.cleanup()
            except Exception as error:
                errors.append(str(error))
            for snapshot in self.snapshots:
                try:
                    self.call("snapshot", "rm", snapshot)
                except Exception as error:
                    errors.append(str(error))
            report = {"passed": failure is None and not errors, "checks": self.results,
                      "error": str(failure) if failure else None, "cleanup_errors": errors}
            Path(self.args.report).write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2), flush=True)
        if not report["passed"]:
            raise SystemExit(1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="kumabox")
    parser.add_argument("--python-image", required=True)
    parser.add_argument("--node-image", required=True)
    parser.add_argument("--socket")
    parser.add_argument("--run-dir")
    parser.add_argument("--report", default="/tmp/kumabox-workloads-e2e.json")
    WorkloadTests(parser.parse_args()).run()


if __name__ == "__main__":
    main()
