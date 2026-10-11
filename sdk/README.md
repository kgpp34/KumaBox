# KumaBox SDKs

The native Python and TypeScript SDKs use KumaBox's versioned `/v1` API.
They are separate from the experimental adapter for the official `e2b` packages;
see the [E2B compatibility guide](../api/e2b/README.md) for that protocol.

Both SDKs expose sandbox creation and connection, lifecycle operations,
foreground commands, binary file transfers, snapshots, hibernation, restore and
clone. Images must already exist on the host. The SDKs are currently installed
from this repository, rather than from a published PyPI/npm package.

Start `kumabox serve --token-file ...` on the host. Use its loopback endpoint
directly or through an SSH tunnel / TLS reverse proxy. Set `KUMABOX_API_TOKEN` to
the server token in the examples below.

## Python

```sh
python3 -m pip install ./sdk/python
```

```python
import os
import sys
from kumabox import Client, DataDisk

client = Client(token=os.environ["KUMABOX_API_TOKEN"])
sandbox = client.create(
    "ubuntu", cpus=2, memory=1024**3, nics=1, shared_memory=True,
    data_disks=[DataDisk("data", 256 * 1024**2, direct_io=False)],
)
try:
    result = sandbox.commands.run(
        "echo hello; echo warning >&2",
        on_stdout=lambda chunk: print(chunk, end="", flush=True),
        on_stderr=lambda chunk: print(chunk, end="", flush=True, file=sys.stderr),
    )
    print(result.exit_code)
    sandbox.files.write("/tmp/example.bin", bytes([0, 1, 255]))
    assert sandbox.files.read("/tmp/example.bin", format="bytes") == bytes([0, 1, 255])
finally:
    sandbox.stop()
    sandbox.kill()
```

## TypeScript

Build the local package, then install it into your application using its absolute
directory path:

```sh
npm ci --prefix sdk/typescript
npm run build --prefix sdk/typescript
# In your application: npm install /absolute/path/to/KumaBox/sdk/typescript
```

```ts
import { Client } from '@kumabox/sdk'

const client = new Client({ token: process.env.KUMABOX_API_TOKEN! })
const sandbox = await client.create('ubuntu', {
  cpus: 2, memory: 1024 ** 3, nics: 1, sharedMemory: true,
  dataDisks: [{ name: 'data', size: 256 * 1024 ** 2, directIO: false }],
})
try {
  const result = await sandbox.commands.run('echo hello; echo warning >&2', {
    onStdout: chunk => { process.stdout.write(chunk) },
    onStderr: chunk => { process.stderr.write(chunk) },
  })
  console.log(result.exitCode)
  await sandbox.files.write('/tmp/example.bin', new Uint8Array([0, 1, 255]))
  console.log(await sandbox.files.read('/tmp/example.bin', { format: 'bytes' }))
} finally {
  await sandbox.stop()
  await sandbox.kill()
}
```

Output callbacks run synchronously as chunks arrive. Chunks contain complete
UTF-8 characters but need not be full lines. Commands still return all collected
stdout/stderr. A callback exception propagates and closes the response; it does
not guarantee that the guest command has been terminated. TypeScript callbacks
are not awaited, so use synchronous callbacks.

Nonzero guest exits raise `CommandExitError` with the collected output. Pass
`check=False` (Python) or `{ check: false }` (TypeScript) to receive the result
instead. A connection ending without an exit event is `TRUNCATED_STREAM`, never
a successful command result.

## Resource parameters, deadlines and cancellation

Both SDKs expose CPU count, memory/storage sizes in bytes, NIC count, network,
shared memory, data disks and whether to start after creation. Data disks default
to `ext4`; server-side validation determines supported configurations. Python
uses `shared_memory`, `data_disks` and `DataDisk.direct_io`. TypeScript uses
`sharedMemory`, `dataDisks` and `directIO` in creation options; returned `data`
objects retain the native API's snake_case fields.

Command execution and HTTP waits have separate settings:

| Setting | Python | TypeScript | Meaning |
| --- | --- | --- | --- |
| Command deadline | `commands.run(..., timeout=5)` | `commands.run(..., { timeoutMs: 5000 })` | Server bounds the entire Exec operation, including connecting to the agent |
| HTTP wait | `request_timeout=10` | `requestTimeoutMs: 10000` | How long the SDK waits for the HTTP transport |
| Disable HTTP limit | `Client(..., request_timeout=None)` | `Client({ ..., requestTimeoutMs: 0 })` | Opt out for long-running operations |
| Explicit cancellation | Interrupt the synchronous call, e.g. Ctrl-C | Pass `signal: controller.signal`, then `controller.abort()` | Close the active client request |

Python time units are seconds; TypeScript uses milliseconds. Positive deadlines
are capped at 24 hours; command deadlines must be at least 1ms. Command deadlines
are omitted by default and return `COMMAND_TIMEOUT` when exceeded. They require
an API server containing the `timeout_ms` contract.

Both clients default their HTTP wait to 30 seconds. A per-call value overrides
the client default for that operation. In Python, per-call `None` inherits the
client default. Its synchronous standard-library transport limits connect/read
waits, not the aggregate duration of a response that keeps producing data. In
TypeScript, the HTTP deadline covers fetching headers and consuming the entire
response, including files and command streams. `Client.fetch` is a low-level
escape hatch: its caller owns body consumption and cancellation; high-level SDK
methods manage their own deadlines and cleanup.

For example, allow the server five seconds for execution and the client ten
seconds to receive the result:

```python
result = sandbox.commands.run("sleep 10", timeout=5, request_timeout=10)
```

```ts
const controller = new AbortController()
const result = await sandbox.commands.run('sleep 10', {
  timeoutMs: 5000, requestTimeoutMs: 10000, signal: controller.signal,
})
// Another event can call controller.abort() while the promise is pending.
```

HTTP timeout raises `REQUEST_TIMEOUT`; TypeScript explicit cancellation raises
`REQUEST_CANCELED`. Existing request-context cancellation closes the agent
channel, and the guest agent cancels its foreground process group. The SDK does
not wait for a separate guest termination acknowledgement after disconnecting.
It also cannot roll back a create, clone or snapshot that has already committed.
Requests are never automatically retried; inspect retained resources when a
mutating operation's result is uncertain.

## SDK tests

```sh
PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v
npm test --prefix sdk/typescript
# Optional static checks, with mypy and ruff installed:
python3 -m mypy --config-file sdk/python/pyproject.toml sdk/python/kumabox
ruff check sdk/python
ruff format --check sdk/python
```

These are transport and client tests. Real guest execution still requires Linux,
KVM, the host dependencies and an imported guest image.
