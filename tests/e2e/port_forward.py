#!/usr/bin/env python3
"""Verify TCP forwarding through real VMs with zero and one NIC.

Run as root using an imported image with the matching agent and Python 3.
Only invocation-owned VMs and forwarding processes are removed. The guest
fixture uses Python's standard library; no WebSocket package is required.
"""

import argparse
import base64
import concurrent.futures
import contextlib
import hashlib
import http.client
import json
import os
import signal
import socket
import struct
import subprocess
import tempfile
import time
from pathlib import Path

from guest_operations import GuestTests


GUEST_SERVER = r'''
import base64, hashlib, http.server, json, socketserver, struct, threading

def read_exact(stream, count):
    result = b""
    while len(result) < count:
        chunk = stream.read(count - len(result))
        if not chunk:
            raise EOFError()
        result += chunk
    return result

class HTTP(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *args):
        pass
    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        if not 0 <= size <= 4 * 1024 * 1024:
            self.send_error(413)
            return
        data = read_exact(self.rfile, size)
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_GET(self):
        if self.path != "/ws":
            self.send_error(404)
            return
        key = self.headers["Sec-WebSocket-Key"]
        accept = base64.b64encode(hashlib.sha1(
            (key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        self.send_response(101)
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", accept)
        self.end_headers()
        try:
            while True:
                first, second = read_exact(self.rfile, 2)
                size = second & 127
                if size == 126:
                    size = struct.unpack("!H", read_exact(self.rfile, 2))[0]
                elif size == 127:
                    size = struct.unpack("!Q", read_exact(self.rfile, 8))[0]
                if size > 1024 * 1024 or not second & 128:
                    return
                mask = read_exact(self.rfile, 4)
                data = bytes(b ^ mask[i % 4] for i, b in enumerate(read_exact(self.rfile, size)))
                opcode = first & 15
                if opcode == 8:
                    self.wfile.write(b"\x88\x00")
                    return
                header = bytes([0x80 | (10 if opcode == 9 else opcode)])
                if size < 126:
                    header += bytes([size])
                elif size < 65536:
                    header += b"\x7e" + struct.pack("!H", size)
                else:
                    header += b"\x7f" + struct.pack("!Q", size)
                self.wfile.write(header + data)
                self.wfile.flush()
        except (EOFError, OSError):
            pass
        finally:
            self.close_connection = True

class HalfClose(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(30)
        try:
            data = self.rfile.read(2 * 1024 * 1024)
            self.wfile.write(data)
        except OSError:
            pass

class TCP(socketserver.ThreadingTCPServer):
    daemon_threads = True

http = http.server.ThreadingHTTPServer(("127.0.0.1", 0), HTTP)
half = TCP(("127.0.0.1", 0), HalfClose)
for server in (http, half):
    threading.Thread(target=server.serve_forever, daemon=True).start()
print(json.dumps({"http": http.server_port, "half": half.server_address[1]}), flush=True)
threading.Event().wait()
'''


def exact(connection, count):
    result = bytearray()
    while len(result) < count:
        chunk = connection.recv(count - len(result))
        if not chunk:
            raise EOFError(f"received {len(result)} of {count} bytes")
        result.extend(chunk)
    return bytes(result)


class ForwardTests(GuestTests):
    def start_fixture(self):
        self.guest("python3", "-c", "import http.server, socketserver, hashlib")
        self.file("write", self.workspace + "/forward_server.py", data=GUEST_SERVER.encode())
        process = json.loads(self.call(
            "exec", "--detach", "--json", self.name, "--", "python3", "-u",
            self.workspace + "/forward_server.py",
        ))
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            output = self.call("process", "logs", self.name, process["id"])
            try:
                self.ports = json.loads(output)
                return
            except json.JSONDecodeError:
                time.sleep(0.1)
        raise RuntimeError(f"guest fixture did not start: {output!r}")

    @contextlib.contextmanager
    def forward(self, guest_port):
        with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
            process = subprocess.Popen(
                self.command + ["forward", self.name, f"0:{guest_port}", "--json"],
                stdout=output, stderr=errors,
            )
            try:
                deadline = time.monotonic() + 15
                while True:
                    output.seek(0)
                    try:
                        info = json.loads(output.read())
                        host, port = info["address"].rsplit(":", 1)
                        break
                    except json.JSONDecodeError:
                        if process.poll() is not None or time.monotonic() > deadline:
                            errors.seek(0)
                            raise RuntimeError(f"forward did not listen: {errors.read()!r}")
                        time.sleep(0.05)
                yield host, int(port), process
            finally:
                if process.poll() is None:
                    process.send_signal(signal.SIGINT)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise RuntimeError("forward did not exit after SIGINT")
                if process.returncode != 0:
                    errors.seek(0)
                    raise RuntimeError(f"forward exited {process.returncode}: {errors.read()!r}")

    def http_and_concurrency(self):
        payload = os.urandom(1024 * 1024)
        with self.forward(self.ports["http"]) as (host, port, _):
            def request(_):
                connection = http.client.HTTPConnection(host, port, timeout=15)
                try:
                    for _ in range(2):
                        connection.request("POST", "/echo", payload)
                        response = connection.getresponse()
                        assert response.status == 200 and response.read() == payload
                finally:
                    connection.close()
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as clients:
                list(clients.map(request, range(8)))

    def websocket(self):
        with self.forward(self.ports["http"]) as (host, port, _):
            with socket.create_connection((host, port), timeout=10) as connection:
                key = base64.b64encode(os.urandom(16)).decode()
                connection.sendall((
                    f"GET /ws HTTP/1.1\r\nHost: guest\r\nUpgrade: websocket\r\n"
                    f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
                    "Sec-WebSocket-Version: 13\r\n\r\n"
                ).encode())
                headers = bytearray()
                while not headers.endswith(b"\r\n\r\n"):
                    headers.extend(exact(connection, 1))
                    assert len(headers) < 16384
                expected = base64.b64encode(hashlib.sha1(
                    (key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()
                ).digest())
                assert b" 101 " in headers and expected in headers
                for opcode, payload in ((2, os.urandom(200000)), (9, b"ping"), (8, b"")):
                    mask = os.urandom(4)
                    size = len(payload)
                    header = bytes([128 | opcode])
                    if size < 126:
                        header += bytes([128 | size])
                    elif size < 65536:
                        header += b"\xfe" + struct.pack("!H", size)
                    else:
                        header += b"\xff" + struct.pack("!Q", size)
                    masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
                    connection.sendall(header + mask + masked)
                    first, second = exact(connection, 2)
                    assert not second & 128
                    size = second & 127
                    if size == 126:
                        size = struct.unpack("!H", exact(connection, 2))[0]
                    elif size == 127:
                        size = struct.unpack("!Q", exact(connection, 8))[0]
                    assert first & 15 == (10 if opcode == 9 else opcode)
                    assert exact(connection, size) == payload

    def half_close(self):
        payload = os.urandom(512 * 1024)
        with self.forward(self.ports["half"]) as (host, port, _):
            with socket.create_connection((host, port), timeout=10) as connection:
                connection.sendall(payload)
                connection.shutdown(socket.SHUT_WR)
                assert exact(connection, len(payload)) == payload
                assert connection.recv(1) == b""

    def cancellation(self):
        with self.forward(self.ports["half"]) as (host, port, process):
            with socket.create_connection((host, port), timeout=5) as connection:
                connection.sendall(b"unfinished")
                process.send_signal(signal.SIGINT)
                process.wait(timeout=10)
                try:
                    assert connection.recv(1) == b""
                except ConnectionResetError:
                    pass
        with socket.socket() as probe:
            assert probe.connect_ex((host, port)) != 0

    def sandbox_stop(self):
        with self.forward(self.ports["half"]) as (host, port, _):
            with socket.create_connection((host, port), timeout=10) as connection:
                connection.sendall(b"waiting")
                self.call("stop", self.name, "--json", timeout=30)
                try:
                    assert connection.recv(1) == b""
                except ConnectionResetError:
                    pass

    def run(self):
        started = time.monotonic()
        failure = None
        try:
            for nics in self.args.nics:
                self.check(f"create and matching agent ({nics} NIC)", lambda: self.create(nics))
                self.check(f"start guest loopback fixture ({nics} NIC)", self.start_fixture)
                for name, action in (
                    ("HTTP binary echo, keepalive, eight concurrent clients", self.http_and_concurrency),
                    ("WebSocket upgrade, binary frames, ping and close", self.websocket),
                    ("response after TCP request half-close", self.half_close),
                    ("SIGINT closes active connection and listener", self.cancellation),
                    ("sandbox stop unblocks active forwarding", self.sandbox_stop),
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
                "image": self.args.image, "agent_commit": self.args.commit,
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
    parser.add_argument("--commit", required=True, help="expected guest agent build identifier")
    parser.add_argument("--nics", nargs="+", type=int, choices=(0, 1), default=[0, 1])
    parser.add_argument("--root-dir")
    parser.add_argument("--run-dir")
    parser.add_argument("--log-dir")
    parser.add_argument("--report", default="/tmp/kumabox-forward-e2e.json")
    ForwardTests(parser.parse_args()).run()


if __name__ == "__main__":
    main()
