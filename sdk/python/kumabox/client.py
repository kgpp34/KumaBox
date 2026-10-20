"""A small synchronous SDK with E2B-style sandbox and command handles."""

from __future__ import annotations

import base64
import builtins
import codecs
import json
import math
import os
from collections.abc import Callable, Iterator, Sequence
from dataclasses import dataclass
from http.client import HTTPResponse
from typing import Any, cast
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen


class KumaBoxError(Exception):
    """An API failure with a stable code and partial-commit indicator."""

    def __init__(self, code: str, message: str, *, committed: bool = False, resource_id: str = ""):
        super().__init__(message)
        self.code = code
        self.committed = committed
        self.resource_id = resource_id


@dataclass(frozen=True)
class CommandResult:
    """Collected output and exit status of a completed guest command."""

    stdout: str
    stderr: str
    exit_code: int


@dataclass(frozen=True)
class DataDisk:
    """One sandbox-owned data volume; size is in bytes."""

    name: str
    size: int
    fstype: str = "ext4"
    direct_io: bool | None = None

    def _json(self) -> dict[str, Any]:
        result: dict[str, Any] = {"name": self.name, "size": self.size, "fstype": self.fstype}
        if self.direct_io is not None:
            result["direct_io"] = self.direct_io
        return result


class CommandExitError(KumaBoxError):
    """A guest command exited with a nonzero status."""

    def __init__(self, result: CommandResult):
        super().__init__("COMMAND_EXIT", f"guest command exited with status {result.exit_code}")
        self.result = result
        self.exit_code = result.exit_code
        self.stdout = result.stdout
        self.stderr = result.stderr


class Client:
    """Connection to one KumaBox API server.

    `base_url` may be an SSH-tunneled loopback URL. The server token can also be
    supplied through `KUMABOX_API_TOKEN`.
    """

    def __init__(
        self,
        base_url: str = "http://127.0.0.1:8765",
        token: str | None = None,
        *,
        request_timeout: float | None = 30.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.token = token if token is not None else os.environ.get("KUMABOX_API_TOKEN", "")
        if not self.token:
            raise ValueError("KumaBox API token is required")
        _validate_timeout(request_timeout)
        self.request_timeout = request_timeout

    def _open(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | bytes | None = None,
        *,
        request_timeout: float | None = None,
    ) -> HTTPResponse:
        """Build and open all SDK requests through one authenticated boundary."""
        timeout = self.request_timeout if request_timeout is None else request_timeout
        _validate_timeout(timeout)
        binary = isinstance(body, bytes)
        payload = (
            body if isinstance(body, bytes) or body is None else json.dumps(body).encode("utf-8")
        )
        request = Request(
            self.base_url + path,
            payload,
            {
                "Authorization": f"Bearer {self.token}",
                "Content-Type": "application/octet-stream" if binary else "application/json",
            },
            method=method,
        )
        return _open_stream(request, timeout)

    def _request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        *,
        request_timeout: float | None = None,
    ) -> Any:
        with self._open(method, path, body, request_timeout=request_timeout) as response:
            return json.loads(_read_response(response))

    def create(
        self,
        image: str,
        *,
        name: str = "",
        cpus: int | None = None,
        memory: int | None = None,
        storage: int | None = None,
        nics: int | None = None,
        network: str = "",
        start: bool = True,
        shared_memory: bool = False,
        data_disks: Sequence[DataDisk] | None = None,
        request_timeout: float | None = None,
    ) -> Sandbox:
        """Create and normally start a sandbox from an imported image."""
        body: dict[str, Any] = {"image": image, "name": name, "network": network, "start": start}
        for key, value in (
            ("cpus", cpus),
            ("memory", memory),
            ("storage", storage),
            ("nics", nics),
        ):
            if value is not None:
                body[key] = value
        body["shared_memory"] = shared_memory
        if data_disks is not None:
            body["data_disks"] = [disk._json() for disk in data_disks]
        return Sandbox(
            self, self._request("POST", "/v1/sandboxes", body, request_timeout=request_timeout)
        )

    def connect(self, reference: str, *, request_timeout: float | None = None) -> Sandbox:
        """Inspect an existing sandbox by ID or name and return its handle."""
        return Sandbox(
            self,
            self._request(
                "GET", f"/v1/sandboxes/{quote(reference, safe='')}", request_timeout=request_timeout
            ),
        )

    def list(self, *, request_timeout: float | None = None) -> builtins.list[Sandbox]:
        """List current and stopped sandboxes."""
        return [
            Sandbox(self, data)
            for data in self._request("GET", "/v1/sandboxes", request_timeout=request_timeout)
        ]

    def snapshots(self, *, request_timeout: float | None = None) -> builtins.list[Snapshot]:
        """List saved snapshots."""
        return [
            Snapshot(self, data)
            for data in self._request("GET", "/v1/snapshots", request_timeout=request_timeout)
        ]

    def snapshot(self, reference: str, *, request_timeout: float | None = None) -> Snapshot:
        """Inspect a snapshot by ID or name."""
        return Snapshot(
            self,
            self._request(
                "GET", f"/v1/snapshots/{quote(reference, safe='')}", request_timeout=request_timeout
            ),
        )


class Sandbox:
    """A durable sandbox handle with E2B-style `commands.run`."""

    def __init__(self, client: Client, data: dict[str, Any]):
        self.client = client
        self.data = data
        self.commands = Commands(self)
        # Files use the native binary transfer endpoint.
        self.files = Files(self)

    @property
    def sandbox_id(self) -> str:
        return cast(str, self.data["id"])

    @property
    def state(self) -> str:
        return cast(str, self.data["state"])

    def _action(
        self,
        action: str,
        body: dict[str, Any] | None = None,
        *,
        request_timeout: float | None = None,
    ) -> dict[str, Any]:
        return cast(
            dict[str, Any],
            self.client._request(
                "POST",
                f"/v1/sandboxes/{self.sandbox_id}/{action}",
                body,
                request_timeout=request_timeout,
            ),
        )

    def refresh(self, *, request_timeout: float | None = None) -> Sandbox:
        self.data = self.client.connect(self.sandbox_id, request_timeout=request_timeout).data
        return self

    def start(self, *, request_timeout: float | None = None) -> Sandbox:
        self.data = self._action("start", request_timeout=request_timeout)
        return self

    def stop(self, *, request_timeout: float | None = None) -> Sandbox:
        self.data = self._action("stop", request_timeout=request_timeout)
        return self

    def kill(self, *, request_timeout: float | None = None) -> None:
        self.client._request(
            "DELETE", f"/v1/sandboxes/{self.sandbox_id}", request_timeout=request_timeout
        )

    def save_snapshot(
        self, name: str = "", description: str = "", *, request_timeout: float | None = None
    ) -> Snapshot:
        result = self.client._request(
            "POST",
            "/v1/snapshots",
            {"sandbox": self.sandbox_id, "name": name, "description": description},
            request_timeout=request_timeout,
        )
        return Snapshot(self.client, result)

    def hibernate(
        self, name: str = "", description: str = "", *, request_timeout: float | None = None
    ) -> Snapshot:
        return Snapshot(
            self.client,
            self._action(
                "hibernate",
                {"name": name, "description": description},
                request_timeout=request_timeout,
            ),
        )

    def restore(self, snapshot: str, *, request_timeout: float | None = None) -> Sandbox:
        self.data = self._action("restore", {"snapshot": snapshot}, request_timeout=request_timeout)
        return self


class Commands:
    """Guest command runner attached to a sandbox."""

    def __init__(self, sandbox: Sandbox):
        self.sandbox = sandbox

    def run(
        self,
        command: str | list[str],
        *,
        envs: dict[str, str] | None = None,
        stdin: bytes = b"",
        check: bool = True,
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
        timeout: float | None = None,
        request_timeout: float | None = None,
    ) -> CommandResult:
        """Run and collect output, optionally delivering UTF-8 chunks as they arrive.

        Callbacks run synchronously while consuming the response. They receive
        complete Unicode characters even when a character spans output frames.
        A callback exception closes the response and propagates to the caller.
        """
        args = ["sh", "-lc", command] if isinstance(command, str) else command
        body: dict[str, Any] = {
            "args": args,
            "env": envs or {},
            "stdin": base64.b64encode(stdin).decode("ascii"),
        }
        if timeout is not None:
            _validate_timeout(timeout)
            milliseconds = int(timeout * 1000)
            if milliseconds < 1:
                raise ValueError("command timeout must be at least one millisecond")
            body["timeout_ms"] = milliseconds
        path = f"/v1/sandboxes/{self.sandbox.sandbox_id}/exec"
        stdout, stderr = bytearray(), bytearray()
        callbacks = {"stdout": on_stdout, "stderr": on_stderr}
        decoders = {
            name: codecs.getincrementaldecoder("utf-8")(errors="replace") for name in callbacks
        }
        with self.sandbox.client._open(
            "POST", path, body, request_timeout=request_timeout
        ) as response:
            for line in _response_lines(response):
                event = json.loads(line)
                if "error" in event:
                    failure = event["error"]
                    raise KumaBoxError(
                        failure["code"],
                        failure["message"],
                        committed=failure.get("committed", False),
                        resource_id=failure.get("resource_id", ""),
                    )
                if "exit_code" in event:
                    for name, callback in callbacks.items():
                        tail = decoders[name].decode(b"", final=True)
                        if callback is not None and tail:
                            callback(tail)
                    result = CommandResult(
                        stdout.decode("utf-8", errors="replace"),
                        stderr.decode("utf-8", errors="replace"),
                        event["exit_code"],
                    )
                    if check and result.exit_code != 0:
                        raise CommandExitError(result)
                    return result
                data = base64.b64decode(event["data"])
                if event["stream"] == "stdout":
                    stdout.extend(data)
                elif event["stream"] == "stderr":
                    stderr.extend(data)
                else:
                    raise KumaBoxError("INVALID_STREAM", "unexpected command stream")
                callback = callbacks[event["stream"]]
                text = decoders[event["stream"]].decode(data)
                if callback is not None and text:
                    callback(text)
        raise KumaBoxError("TRUNCATED_STREAM", "command response ended without exit status")


class Files:
    """Read and write regular files through the sandbox's guest command channel."""

    def __init__(self, sandbox: Sandbox):
        self.sandbox = sandbox

    def _path(self, path: str) -> str:
        if not path.startswith("/") or "\x00" in path:
            raise ValueError("file path must be absolute and contain no NUL bytes")
        return (
            f"/v1/sandboxes/{quote(self.sandbox.sandbox_id, safe='')}/files"
            + f"?path={quote(path, safe='')}"
        )

    def read(
        self, path: str, *, format: str = "text", request_timeout: float | None = None
    ) -> str | bytes:
        """Return a file as UTF-8 text or exact bytes."""
        if format not in ("text", "bytes"):
            raise ValueError("format must be 'text' or 'bytes'")
        with self.sandbox.client._open(
            "GET", self._path(path), request_timeout=request_timeout
        ) as response:
            data = _read_response(response)
        return data if format == "bytes" else data.decode("utf-8")

    def write(
        self, path: str, data: str | bytes, *, request_timeout: float | None = None
    ) -> dict[str, str]:
        """Create or replace a guest file, creating parent directories."""
        payload = data.encode("utf-8") if isinstance(data, str) else data
        with self.sandbox.client._open(
            "POST", self._path(path), payload, request_timeout=request_timeout
        ) as response:
            return cast(dict[str, str], json.loads(_read_response(response)))


class Snapshot:
    """A captured sandbox state that can be cloned or removed."""

    def __init__(self, client: Client, data: dict[str, Any]):
        self.client = client
        self.data = data

    @property
    def snapshot_id(self) -> str:
        return cast(str, self.data["id"])

    def clone(self, name: str, *, request_timeout: float | None = None) -> Sandbox:
        result = self.client._request(
            "POST",
            f"/v1/snapshots/{self.snapshot_id}/clone",
            {"name": name},
            request_timeout=request_timeout,
        )
        return Sandbox(self.client, result)

    def remove(self, *, request_timeout: float | None = None) -> None:
        self.client._request(
            "DELETE", f"/v1/snapshots/{self.snapshot_id}", request_timeout=request_timeout
        )


def _open_stream(request: Request, timeout: float | None) -> HTTPResponse:
    try:
        # urllib's annotation is Any because it also supports non-HTTP schemes.
        return cast(HTTPResponse, urlopen(request, timeout=timeout))
    except HTTPError as exc:
        with exc:
            try:
                failure = json.loads(_read_response(exc))["error"]
                if not isinstance(failure, dict):
                    raise ValueError("invalid error envelope")
            except (ValueError, KeyError, TypeError):
                raise KumaBoxError("HTTP_ERROR", f"HTTP {exc.code}") from exc
        raise KumaBoxError(
            failure.get("code", "HTTP_ERROR"),
            failure.get("message", f"HTTP {exc.code}"),
            committed=failure.get("committed", False),
            resource_id=failure.get("resource_id", ""),
        ) from exc
    except OSError as exc:
        raise _transport_error(exc) from exc


def _validate_timeout(value: float | None) -> None:
    if value is not None and (
        isinstance(value, bool) or not math.isfinite(value) or not 0 < value <= 86400
    ):
        raise ValueError("timeout must be finite, positive and at most 86400 seconds")


def _transport_error(error: OSError) -> KumaBoxError:
    reason = error.reason if isinstance(error, URLError) else error
    if isinstance(reason, TimeoutError):
        return KumaBoxError("REQUEST_TIMEOUT", "HTTP connect/read timeout exceeded")
    return KumaBoxError("CONNECTION_ERROR", "HTTP connection failed")


def _read_response(response: HTTPResponse | HTTPError) -> bytes:
    try:
        return response.read()
    except OSError as exc:
        raise _transport_error(exc) from exc


def _response_lines(response: HTTPResponse) -> Iterator[bytes]:
    iterator = iter(response)
    while True:
        try:
            line = next(iterator)
        except StopIteration:
            return
        except OSError as exc:
            raise _transport_error(exc) from exc
        yield line
