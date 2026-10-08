"""A small synchronous SDK with E2B-style sandbox and command handles."""

from __future__ import annotations

import base64
import json
import os
from dataclasses import dataclass
from typing import Any, BinaryIO
from urllib.error import HTTPError
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

    def __init__(self, base_url: str = "http://127.0.0.1:8765", token: str | None = None):
        self.base_url = base_url.rstrip("/")
        self.token = token if token is not None else os.environ.get("KUMABOX_API_TOKEN", "")
        if not self.token:
            raise ValueError("KumaBox API token is required")

    def _request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        payload = None if body is None else json.dumps(body).encode("utf-8")
        request = Request(
            self.base_url + path,
            payload,
            {"Authorization": f"Bearer {self.token}", "Content-Type": "application/json"},
            method=method,
        )
        try:
            with urlopen(request) as response:
                return json.load(response)
        except HTTPError as exc:
            try:
                failure = json.load(exc)["error"]
            except (ValueError, KeyError, TypeError):
                raise KumaBoxError("HTTP_ERROR", f"HTTP {exc.code}") from exc
            raise KumaBoxError(
                failure.get("code", "HTTP_ERROR"),
                failure.get("message", f"HTTP {exc.code}"),
                committed=failure.get("committed", False),
                resource_id=failure.get("resource_id", ""),
            ) from exc

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
    ) -> Sandbox:
        """Create and normally start a sandbox from an imported image."""
        body: dict[str, Any] = {"image": image, "name": name, "network": network, "start": start}
        for key, value in (("cpus", cpus), ("memory", memory), ("storage", storage), ("nics", nics)):
            if value is not None:
                body[key] = value
        return Sandbox(self, self._request("POST", "/v1/sandboxes", body))

    def connect(self, reference: str) -> Sandbox:
        """Inspect an existing sandbox by ID or name and return its handle."""
        return Sandbox(self, self._request("GET", f"/v1/sandboxes/{quote(reference, safe='')}"))

    def list(self) -> list[Sandbox]:
        """List current and stopped sandboxes."""
        return [Sandbox(self, data) for data in self._request("GET", "/v1/sandboxes")]

    def snapshots(self) -> list[Snapshot]:
        """List saved snapshots."""
        return [Snapshot(self, data) for data in self._request("GET", "/v1/snapshots")]

    def snapshot(self, reference: str) -> Snapshot:
        """Inspect a snapshot by ID or name."""
        return Snapshot(self, self._request("GET", f"/v1/snapshots/{quote(reference, safe='')}"))


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
        return self.data["id"]

    @property
    def state(self) -> str:
        return self.data["state"]

    def _action(self, action: str, body: dict[str, Any] | None = None) -> dict[str, Any]:
        return self.client._request("POST", f"/v1/sandboxes/{self.sandbox_id}/{action}", body)

    def refresh(self) -> Sandbox:
        self.data = self.client.connect(self.sandbox_id).data
        return self

    def start(self) -> Sandbox:
        self.data = self._action("start")
        return self

    def stop(self) -> Sandbox:
        self.data = self._action("stop")
        return self

    def kill(self) -> None:
        self.client._request("DELETE", f"/v1/sandboxes/{self.sandbox_id}")

    def save_snapshot(self, name: str = "", description: str = "") -> Snapshot:
        result = self.client._request(
            "POST", "/v1/snapshots",
            {"sandbox": self.sandbox_id, "name": name, "description": description},
        )
        return Snapshot(self.client, result)

    def hibernate(self, name: str = "", description: str = "") -> Snapshot:
        return Snapshot(self.client, self._action("hibernate", {"name": name, "description": description}))

    def restore(self, snapshot: str) -> Sandbox:
        self.data = self._action("restore", {"snapshot": snapshot})
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
    ) -> CommandResult:
        """Run a shell string or explicit argv and collect stdout/stderr."""
        args = ["sh", "-lc", command] if isinstance(command, str) else command
        body = {"args": args, "env": envs or {}, "stdin": base64.b64encode(stdin).decode("ascii")}
        path = f"/v1/sandboxes/{self.sandbox.sandbox_id}/exec"
        payload = json.dumps(body).encode("utf-8")
        request = Request(
            self.sandbox.client.base_url + path, payload,
            {"Authorization": f"Bearer {self.sandbox.client.token}", "Content-Type": "application/json"},
            method="POST",
        )
        stdout, stderr = bytearray(), bytearray()
        with _open_stream(request) as response:
            for line in response:
                event = json.loads(line)
                if "error" in event:
                    failure = event["error"]
                    raise KumaBoxError(
                        failure["code"], failure["message"],
                        committed=failure.get("committed", False),
                        resource_id=failure.get("resource_id", ""),
                    )
                if "exit_code" in event:
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
        raise KumaBoxError("TRUNCATED_STREAM", "command response ended without exit status")


class Files:
    """Read and write regular files through the sandbox's guest command channel."""

    def __init__(self, sandbox: Sandbox):
        self.sandbox = sandbox

    def _url(self, path: str) -> str:
        if not path.startswith("/") or "\x00" in path:
            raise ValueError("file path must be absolute and contain no NUL bytes")
        return (
            self.sandbox.client.base_url
            + f"/v1/sandboxes/{quote(self.sandbox.sandbox_id, safe='')}/files"
            + f"?path={quote(path, safe='')}"
        )

    def read(self, path: str, *, format: str = "text") -> str | bytes:
        """Return a file as UTF-8 text or exact bytes."""
        if format not in ("text", "bytes"):
            raise ValueError("format must be 'text' or 'bytes'")
        request = Request(self._url(path), headers={"Authorization": f"Bearer {self.sandbox.client.token}"})
        with _open_stream(request) as response:
            data = response.read()
        return data if format == "bytes" else data.decode("utf-8")

    def write(self, path: str, data: str | bytes) -> dict[str, str]:
        """Create or replace a guest file, creating parent directories."""
        payload = data.encode("utf-8") if isinstance(data, str) else data
        request = Request(
            self._url(path), payload,
            {"Authorization": f"Bearer {self.sandbox.client.token}", "Content-Type": "application/octet-stream"},
            method="POST",
        )
        with _open_stream(request) as response:
            return json.load(response)


class Snapshot:
    """A captured sandbox state that can be cloned or removed."""

    def __init__(self, client: Client, data: dict[str, Any]):
        self.client = client
        self.data = data

    @property
    def snapshot_id(self) -> str:
        return self.data["id"]

    def clone(self, name: str) -> Sandbox:
        result = self.client._request(
            "POST", f"/v1/snapshots/{self.snapshot_id}/clone", {"name": name}
        )
        return Sandbox(self.client, result)

    def remove(self) -> None:
        self.client._request("DELETE", f"/v1/snapshots/{self.snapshot_id}")


def _open_stream(request: Request) -> BinaryIO:
    try:
        return urlopen(request)
    except HTTPError as exc:
        try:
            failure = json.load(exc)["error"]
        except (ValueError, KeyError, TypeError):
            raise KumaBoxError("HTTP_ERROR", f"HTTP {exc.code}") from exc
        raise KumaBoxError(
            failure.get("code", "HTTP_ERROR"),
            failure.get("message", f"HTTP {exc.code}"),
            committed=failure.get("committed", False),
            resource_id=failure.get("resource_id", ""),
        ) from exc
