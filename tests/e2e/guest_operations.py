#!/usr/bin/env python3
"""Exercise guest files and background commands through a real Linux/KVM VM.

Run as root with an imported Ubuntu/systemd image containing the current agent.
Only sandboxes created by this invocation are stopped and removed. Use dedicated
--root-dir/--run-dir/--log-dir to isolate metadata from normal workloads.
"""

import argparse
import hashlib
import json
import os
import signal
import subprocess
import tempfile
import time
from pathlib import Path


class GuestTests:
    def __init__(self, args):
        self.args = args
        self.command = [args.binary]
        for option in ("root_dir", "run_dir", "log_dir"):
            if value := getattr(args, option):
                self.command.extend(["--" + option.replace("_", "-"), value])
        self.results = []
        self.created = []
        self.name = ""
        self.workspace = "/tmp/kumabox-agent-e2e"

    def call(self, *args, data=None, expect=0, timeout=30):
        reply = subprocess.run(
            self.command + list(args), input=data, capture_output=True, timeout=timeout,
        )
        valid = reply.returncode == expect if expect is not None else reply.returncode != 0
        if not valid:
            raise RuntimeError(
                f"{args}: exit {reply.returncode}, expected {expect}; "
                f"stderr={reply.stderr.decode(errors='replace')}; "
                f"stdout={reply.stdout[:1024]!r}"
            )
        return reply.stdout

    def guest(self, *args, **kwargs):
        return self.call("exec", self.name, "--", *args, **kwargs)

    def file(self, verb, *args, **kwargs):
        return self.call("file", verb, self.name, *args, **kwargs)

    def check(self, label, action):
        started = time.monotonic()
        try:
            action()
        except Exception as error:
            self.results.append({"name": label, "sandbox": self.name, "passed": False, "error": str(error)})
            print(f"FAIL {label}: {error}", flush=True)
            raise
        self.results.append({
            "name": label, "sandbox": self.name, "passed": True,
            "seconds": round(time.monotonic() - started, 3),
        })
        print(f"PASS {label}", flush=True)

    def create(self, nics):
        self.name = f"guest-e2e-{os.getpid()}-n{nics}"
        self.created.append(self.name)
        self.call(
            "run", self.args.image, "--name", self.name, "--cpus", "1",
            "--memory", "512MiB", "--nics", str(nics), "--json", timeout=120,
        )
        deadline = time.monotonic() + 60
        while True:
            try:
                self.guest("/bin/true", timeout=5)
                break
            except (RuntimeError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise TimeoutError(f"guest agent did not become ready: {self.name}")
                time.sleep(0.25)
        version = self.guest("kumabox-agent", "version").decode().strip()
        if self.args.commit not in version:
            raise RuntimeError(f"wrong guest agent version: {version}")
        print(version, flush=True)
        # A reachable agent must not race later boot-time /tmp removal.
        assert self.guest("systemctl", "is-active", "systemd-tmpfiles-setup.service").strip() == b"active"
        if nics:
            # Agent readiness is independent of networkd configuring the NIC.
            # Wait for the actual network state rather than racing first boot.
            deadline = time.monotonic() + 15
            while True:
                addresses = self.guest("ip", "-br", "-4", "addr")
                routes = self.guest("ip", "-4", "route")
                if b"eth0" in addresses and b"default" in routes:
                    break
                if time.monotonic() >= deadline:
                    raise TimeoutError(f"guest network not ready: addresses={addresses!r}, routes={routes!r}")
                time.sleep(0.05)
        self.file("mkdir", self.workspace, "--parents")

    def round_trip(self):
        payload = bytes(range(256)) * (32 * 1024)  # 8 MiB including NUL bytes.
        destination = self.workspace + "/binary"
        self.file("write", destination, "--mode", "0640", data=payload)
        received = self.file("read", destination)
        assert hashlib.sha256(received).digest() == hashlib.sha256(payload).digest()
        info = json.loads(self.file("stat", destination))
        assert info["size"] == len(payload) and info["mode"] == 0o640
        self.file("write", destination, data=b"replacement")
        info = json.loads(self.file("stat", destination))
        assert info["mode"] == 0o640
        assert self.file("read", destination) == b"replacement"

    def source_and_empty_files(self):
        name = self.workspace + "/nested/upload"
        with tempfile.NamedTemporaryFile() as source:
            source.write(b"local source\x00\xff")
            source.flush()
            self.file("write", name, source.name, "--parents")
        assert self.file("read", name) == b"local source\x00\xff"
        assert json.loads(self.file("stat", name))["mode"] == 0o600
        self.file("write", name, data=b"")
        assert self.file("read", name) == b""
        assert json.loads(self.file("stat", name))["size"] == 0
        special = self.workspace + "/space 'quote' $dollar;semi"
        self.file("write", special, data=b"literal filename")
        assert self.file("read", special) == b"literal filename"

    def directories(self):
        directory = self.workspace + "/many"
        self.file("mkdir", directory)
        self.guest("sh", "-c", 'for i in $(seq 1 300); do touch "$1/$i"; done', "sh", directory)
        entries = json.loads(self.file("ls", directory, "--json"))
        assert len(entries) == 300 and len({entry["name"] for entry in entries}) == 300
        assert b"MODE" in self.file("ls", directory)
        self.file("rm", directory, expect=None)
        self.file("rm", directory, "--recursive")
        self.file("stat", directory, expect=None)

    def rename_and_links(self):
        source, target = self.workspace + "/source", self.workspace + "/target"
        self.file("write", source, data=b"source")
        self.file("write", target, data=b"target")
        self.file("mv", source, target)
        assert self.file("read", target) == b"source"
        self.file("stat", source, expect=None)
        link = self.workspace + "/link"
        self.guest("ln", "-s", target, link)
        info = json.loads(self.file("stat", link))
        assert info["kind"] == "symlink" and info["link_target"] == target
        assert self.file("read", link) == b"source"
        self.file("write", link, data=b"bad", expect=None)
        self.file("rm", link, "--recursive")
        assert self.file("read", target) == b"source"

    def invalid_requests(self):
        for path in (self.workspace, "/dev/null", "/dev/zero"):
            self.file("read", path, expect=None)
        fifo = self.workspace + "/fifo"
        self.guest("mkfifo", fifo)
        self.file("read", fifo, expect=None, timeout=5)
        self.file("write", fifo, data=b"bad", expect=None, timeout=5)
        self.file("read", self.workspace + "/missing", expect=None)
        self.file("read", "relative", expect=None)
        self.file("rm", "/", "--recursive", expect=None)
        self.file("mv", self.workspace + "/target", "/", expect=None)
        self.file("write", self.workspace + "/target", "--mode", "1000", data=b"bad", expect=None)
        assert self.file("read", self.workspace + "/target") == b"source"

    def cancelled_upload(self):
        target = self.workspace + "/cancelled"
        self.file("write", target, data=b"original")
        process = subprocess.Popen(
            self.command + ["file", "write", self.name, target],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        try:
            process.stdin.write(b"incomplete\x00" * 8192)
            process.stdin.flush()
            deadline = time.monotonic() + 5
            while True:
                entries = json.loads(self.file("ls", self.workspace, "--json"))
                if any(entry["name"].startswith(".kumabox-upload-") and entry["size"] > 0 for entry in entries):
                    break
                if time.monotonic() >= deadline:
                    raise TimeoutError("upload never entered its staging phase")
                time.sleep(0.05)
            process.send_signal(signal.SIGINT)
            process.wait(timeout=10)  # Deliberately keep stdin open during cancellation.
            assert process.returncode != 0
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            for stream in (process.stdin, process.stdout, process.stderr):
                try:
                    stream.close()
                except BrokenPipeError:
                    pass
        deadline = time.monotonic() + 5
        while True:
            entries = json.loads(self.file("ls", self.workspace, "--json"))
            if not any(entry["name"].startswith(".kumabox-upload-") for entry in entries):
                break
            if time.monotonic() >= deadline:
                raise TimeoutError("aborted upload left a temporary file")
            time.sleep(0.05)
        assert self.file("read", target) == b"original"

    def process_options(self):
        output = self.call(
            "exec", "--cwd", self.workspace, "--user", "root", "-e", "KB_E2E=ok", self.name,
            "--", "sh", "-c", 'test "$KB_E2E" = ok; pwd; id -u',
        )
        assert output.splitlines() == [self.workspace.encode(), b"0"]
        assert self.call("exec", "--user", "nobody", self.name, "--", "id", "-u").strip() == b"65534"
        process = json.loads(self.call(
            "exec", "--detach", "--json", "--cwd", "/tmp", self.name,
            "--", "sh", "-c", "printf out; printf err >&2; sleep 0.2; exit 7",
        ))
        identifier = process["id"]
        reply = subprocess.run(
            self.command + ["process", "attach", self.name, identifier],
            capture_output=True, timeout=10,
        )
        assert reply.returncode == 7 and reply.stdout == b"out" and reply.stderr == b"err"
        records = json.loads(self.call("process", "ls", self.name, "--all", "--json"))
        assert any(record["id"] == identifier and record["exit_code"] == 7 for record in records)
        assert self.call("process", "logs", self.name, identifier) == b"out"
        process = json.loads(self.call(
            "exec", "--detach", "--json", self.name, "--", "sleep", "60",
        ))
        identifier = process["id"]
        assert any(record["id"] == identifier for record in json.loads(self.call("process", "ls", self.name, "--json")))
        observer = subprocess.Popen(
            self.command + ["process", "attach", self.name, identifier],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        try:
            time.sleep(0.2)
            assert observer.poll() is None
            observer.send_signal(signal.SIGINT)
            observer.communicate(timeout=5)
        finally:
            if observer.poll() is None:
                observer.kill()
                observer.communicate(timeout=5)
            observer.stdout.close()
            observer.stderr.close()
        assert any(record["id"] == identifier for record in json.loads(self.call("process", "ls", self.name, "--json")))
        self.call("process", "kill", self.name, identifier, "--signal", "KILL")
        self.call("process", "attach", self.name, identifier, expect=137)
        self.call("process", "kill", self.name, "123", expect=None)

    def restart_persists_files(self):
        # Ubuntu intentionally clears /tmp on every boot. Persistence belongs
        # on the sandbox's writable root filesystem outside temporary paths.
        persistent = "/root/kumabox-agent-e2e-persistent"
        self.file("write", persistent, data=b"persisted")
        # stop terminates the VMM rather than shutting the guest OS down. The
        # file API promises atomic visibility, not persistence after power loss.
        self.guest("sync")
        self.call("stop", self.name, "--json")
        self.file("read", persistent, expect=None)
        self.call("start", self.name, "--json")
        deadline = time.monotonic() + 60
        while True:
            try:
                self.guest("/bin/true", timeout=5)
                break
            except (RuntimeError, subprocess.TimeoutExpired):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.25)
        assert self.file("read", persistent) == b"persisted"

    def cleanup(self):
        failures = []
        for name in reversed(self.created):
            try:
                self.call("stop", name, "--json")
                self.call("rm", name)
            except Exception as error:
                failures.append(f"{name}: {error}")
        if failures:
            raise RuntimeError("cleanup failed: " + "; ".join(failures))

    def run(self):
        started = time.monotonic()
        failure = None
        try:
            print(self.call("version").decode().strip(), flush=True)
            for nics in self.args.nics:
                self.check(f"create/start/agent version ({nics} NIC)", lambda: self.create(nics))
                for name, action in (
                    ("8 MiB binary transfer and mode preservation", self.round_trip),
                    ("local source, stdin, empty and literal filenames", self.source_and_empty_files),
                    ("300-entry listing and recursive removal", self.directories),
                    ("rename replacement and symlink semantics", self.rename_and_links),
                    ("invalid paths, devices, FIFO and modes", self.invalid_requests),
                    ("SIGINT upload abort keeps original and removes staging", self.cancelled_upload),
                    ("cwd/user/env, detached jobs, attach/logs/list/kill", self.process_options),
                    ("guest sync then stop/start retains files", self.restart_persists_files),
                ):
                    self.check(f"{name} ({nics} NIC)", action)
        except Exception as error:
            failure = error
        finally:
            try:
                self.cleanup()
            except Exception as error:
                failure = failure or error
            report = {
                "image": self.args.image, "commit": self.args.commit,
                "seconds": round(time.monotonic() - started, 3),
                "passed": failure is None, "checks": self.results,
                "error": str(failure) if failure else None,
            }
            Path(self.args.report).write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2), flush=True)
        if failure:
            raise SystemExit(1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="kumabox")
    parser.add_argument("--image", required=True)
    parser.add_argument("--commit", required=True, help="expected guest agent commit")
    parser.add_argument("--nics", nargs="+", type=int, choices=(0, 1), default=[0, 1])
    parser.add_argument("--root-dir")
    parser.add_argument("--run-dir")
    parser.add_argument("--log-dir")
    parser.add_argument("--report", default="/tmp/kumabox-guest-e2e.json")
    GuestTests(parser.parse_args()).run()


if __name__ == "__main__":
    main()
