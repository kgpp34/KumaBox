#!/usr/bin/env python3
"""Verify a real local node, including VM survival across daemon restarts.

Run as root on Linux with KVM, an imported matching guest image, and an explicit
node config. The script starts its own daemon and refuses to reuse another one.
Only invocation-owned sandboxes, snapshots and temporary files are removed.
"""

import argparse
import concurrent.futures
import json
import os
import signal
import select
import socket
import pty
import urllib.request
import subprocess
import tempfile
import time
import uuid
from pathlib import Path


class NodeTest:
    def __init__(self, args, work):
        self.args = args
        self.work = Path(work)
        self.cli = [args.binary, "--config", args.config]
        self.node = None
        self.log = None
        self.sandboxes = []
        self.snapshots = []
        self.results = []
        self.prefix = "node-e2e-" + uuid.uuid4().hex[:8]

    def call(self, *args, data=None, expect=0, timeout=120):
        reply = subprocess.run(self.cli + list(args), input=data, capture_output=True, timeout=timeout)
        if reply.returncode != expect:
            raise RuntimeError(f"{args}: exit={reply.returncode}, stdout={reply.stdout[:2000]!r}, stderr={reply.stderr[-4000:]!r}")
        return reply.stdout

    def record(self, name, action):
        start = time.monotonic()
        try:
            action()
        except Exception as error:
            self.results.append({"check": name, "status": "failed", "error": str(error)})
            raise
        result = {"check": name, "status": "passed", "seconds": round(time.monotonic() - start, 3)}
        self.results.append(result)
        print(json.dumps(result), flush=True)

    def start(self):
        self.log = (self.work / "daemon.log").open("ab")
        self.node = subprocess.Popen(self.cli + ["daemon", "--reconcile-interval", "1s"],
                                     stdin=subprocess.DEVNULL, stdout=self.log, stderr=self.log)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self.node.poll() is not None:
                raise RuntimeError(f"daemon startup failed: {(self.work / 'daemon.log').read_text()[-4000:]}")
            # A different node can already answer requests; require this process
            # to announce ownership before accepting any client response.
            if b"KumaBox daemon listening" in (self.work / "daemon.log").read_bytes():
                self.call("image", "ls", "--json")
                return
            time.sleep(0.05)
        raise TimeoutError("daemon startup timed out")

    def stop_node(self, abrupt=False):
        if self.node and self.node.poll() is None:
            self.node.send_signal(signal.SIGKILL if abrupt else signal.SIGTERM)
            self.node.wait(timeout=45)
        if self.log:
            self.log.close()
        if (self.work / "daemon.log").exists():
            with Path(self.args.report + ".daemon.log").open("ab") as output:
                output.write((self.work / "daemon.log").read_bytes())
        self.node = None
        # Each startup must announce readiness independently.
        (self.work / "daemon.log").write_bytes(b"")

    def ready(self, name):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            try:
                self.call("exec", name, "--", "/bin/true", timeout=3)
                return
            except (RuntimeError, subprocess.TimeoutExpired):
                time.sleep(0.025)
        raise TimeoutError(f"guest {name} did not become executable")

    def scenario(self, nics):
        name = f"{self.prefix}-{nics}"
        self.sandboxes.append(name)
        run = ["run", self.args.image, "--name", name, "--cpus", "2", "--memory", "512MiB", "--nics", str(nics), "--json"]
        if nics:
            run.extend(["--network-backend", self.args.network_backend])
        self.call(*run)
        self.ready(name)
        self.call("exec", name, "--", "sh", "-c", "exit 17", expect=17)
        payload = bytes(range(256)) * 16384
        self.call("file", "write", name, "/tmp/node-data", "-", data=payload)
        assert self.call("file", "read", name, "/tmp/node-data") == payload
        with concurrent.futures.ThreadPoolExecutor(max_workers=16) as executor:
            records = list(executor.map(lambda _: json.loads(self.call("inspect", name)), range(32)))
        assert all(record["name"] == name and record["state"] == "running" for record in records)

        pid = json.loads(self.call("status", name, "--json"))[0]["pid"]
        for abrupt in (False, True):
            self.stop_node(abrupt=abrupt)
            self.start()
            assert json.loads(self.call("status", name, "--json"))[0]["pid"] == pid
            assert self.call("file", "read", name, "/tmp/node-data") == payload

        self.record(f"console and forwarding: {nics} NIC", lambda: self.streams(name))
        golden = name + "-golden"
        self.snapshots.append(golden)
        self.call("snapshot", "save", name, "--name", golden, "--json")
        clone = name + "-clone"
        self.sandboxes.append(clone)
        self.call("clone", golden, "--name", clone, "--wait-network", "--json")
        self.ready(clone)
        assert self.call("file", "read", clone, "/tmp/node-data") == payload
        source_id = self.call("exec", name, "--", "cat", "/etc/machine-id").strip()
        clone_id = self.call("exec", clone, "--", "cat", "/etc/machine-id").strip()
        assert source_id and clone_id and source_id != clone_id

        archive = self.work / (golden + ".tar.gz")
        self.call("snapshot", "export", golden, "--gzip", "--output", str(archive))
        imported = golden + "-imported"
        self.snapshots.append(imported)
        self.call("snapshot", "import", str(archive), "--name", imported, "--json")
        self.call("file", "write", name, "/tmp/node-data", "-", data=b"changed")
        self.call("stop", name, "--force", "--json")
        self.call("restore", name, imported, "--json")
        self.ready(name)
        assert self.call("file", "read", name, "/tmp/node-data") == payload
        sleeping = name + "-hibernate"
        self.snapshots.append(sleeping)
        self.call("hibernate", name, "--name", sleeping, "--json")
        self.call("restore", name, sleeping, "--json")
        self.ready(name)
        if nics:
            self.call("exec", clone, "--", "curl", "--fail", "--max-time", "15", "-I", "https://example.com")
        for sandbox in (clone, name):
            self.call("stop", sandbox, "--force", "--json")
            self.call("rm", sandbox, "--json")
            self.sandboxes.remove(sandbox)
        for snapshot in (sleeping, imported, golden):
            self.call("snapshot", "rm", snapshot)
            self.snapshots.remove(snapshot)

    def streams(self, name):
        probe = Path(self.args.probe).read_bytes()
        self.call("file", "write", name, "/tmp/node-probe", "-", "--mode", "0755", data=probe)
        self.call("exec", "--detach", name, "--", "/tmp/node-probe")
        for port in (8080, 8081):
            process = subprocess.Popen(self.cli + ["forward", name, f"0:{port}", "--json"],
                                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                readable, _, _ = select.select([process.stdout], [], [], 10)
                if not readable:
                    raise TimeoutError("forward listener did not start")
                header = bytearray()
                deadline = time.monotonic() + 10
                while True:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("forward did not report its address")
                    header.extend(process.stdout.readline())
                    try:
                        endpoint = json.loads(header)["address"]
                        break
                    except json.JSONDecodeError:
                        if process.poll() is not None:
                            raise RuntimeError(process.stderr.read().decode(errors="replace"))
                payload = b"duplex stream test" * 1024
                deadline = time.monotonic() + 10
                while True:
                    try:
                        if port == 8080:
                            request = urllib.request.Request("http://" + endpoint + "/", data=payload)
                            with urllib.request.urlopen(request, timeout=5) as response:
                                assert response.read() == payload
                        else:
                            host, host_port = endpoint.rsplit(":", 1)
                            with socket.create_connection((host, int(host_port)), timeout=5) as connection:
                                connection.sendall(payload)
                                connection.shutdown(socket.SHUT_WR)
                                chunks = []
                                while data := connection.recv(65536):
                                    chunks.append(data)
                                assert b"".join(chunks) == b"reply:" + payload
                        break
                    except (OSError, AssertionError):
                        if time.monotonic() >= deadline:
                            raise
                        time.sleep(0.05)
            finally:
                process.send_signal(signal.SIGINT)
                process.communicate(timeout=10)
        master, slave = pty.openpty()
        process = subprocess.Popen(self.cli + ["console", name], stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        try:
            output = b""
            deadline = time.monotonic() + 10
            while b"Connected to" not in output:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not select.select([master], [], [], remaining)[0]:
                    raise TimeoutError("console did not attach")
                output += os.read(master, 65536)
            process.send_signal(signal.SIGWINCH)
            os.write(master, b"\x1d.")
            assert process.wait(timeout=10) == 0
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            os.close(master)
        self.ready(name)

    def cleanup(self):
        if self.node and self.node.poll() is None:
            for name in reversed(self.sandboxes):
                for args in (("stop", name, "--force"), ("rm", name)):
                    try:
                        self.call(*args, timeout=30)
                    except (RuntimeError, subprocess.TimeoutExpired) as error:
                        print(f"cleanup {name}: {error}", flush=True)
            for name in reversed(self.snapshots):
                try:
                    self.call("snapshot", "rm", name)
                except (RuntimeError, subprocess.TimeoutExpired) as error:
                    print(f"cleanup {name}: {error}", flush=True)
        self.stop_node()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--config", required=True, help="explicit config for an isolated node with an already imported image")
    parser.add_argument("--image", required=True)
    parser.add_argument("--probe", required=True, help="static Linux amd64 node_probe binary built from tests/e2e/testdata/node_probe.go")
    parser.add_argument("--nics", nargs="+", type=int, choices=(0, 1), default=[0, 1])
    parser.add_argument("--network-backend", choices=("cni", "ebpf"), default="ebpf")
    parser.add_argument("--report", default="/tmp/kumabox-daemon-e2e.json")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run as root on the test host")
    with tempfile.TemporaryDirectory(prefix="kumabox-node-e2e-") as work:
        test = NodeTest(args, work)
        try:
            test.start()
            for nics in args.nics:
                test.record(f"lifecycle, files, snapshots and restart: {nics} NIC", lambda nics=nics: test.scenario(nics))
        finally:
            test.cleanup()
            Path(args.report).write_text(json.dumps(test.results, indent=2) + "\n")


if __name__ == "__main__":
    main()
