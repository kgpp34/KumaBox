# Experimental E2B adapter

The adapter runs inside `kumabox serve`. It translates a subset of the official
E2B SDK's control API and envd Connect protocol into existing application
services. It does not install envd inside the guest or change the native v1 API.

```text
Official E2B Python / TypeScript SDK
                  |
            API key / sandbox token
                  |
           api/e2b adapter
          /               \
  API-owned lease      application services
  metadata, defaults   sandbox / snapshot / exec
                               |
                           guest agent
```

## Supported calls

| Capability | Support |
| --- | --- |
| Create | Local image reference or saved snapshot ID as the template |
| Connect, inspect, kill | Existing E2B-created sandboxes |
| Metadata | Creation, inspect, list and exact-match filtering |
| List | Running and paused resources; metadata, state, template and `startedAfter` filters; ascending/descending order; cursor pagination |
| Environment | Creation defaults plus per-command overrides |
| Commands | Foreground execution, stdout/stderr streaming and exit status |
| Files | Read/write regular files, including binary and multipart uploads |
| TTL | Creation timeout, `set_timeout` / `setTimeout`, expiration and optional auto-pause |
| Pause/resume | Memory snapshot and restore, including reconnect to a paused sandbox |
| Snapshots | Create, list, remove and create a sandbox from a snapshot |

Metadata and command defaults survive adapter restarts and pause/resume. Defaults
apply to processes launched through E2B `commands.run`, rather than guest system
services or processes launched through the native API. Command-specific values
override defaults without modifying subsequent commands. The adapter supplies
`E2B_SANDBOX`, `E2B_SANDBOX_ID` and `E2B_TEMPLATE_ID` as process defaults too.

The API-owned state directory contains deadlines, snapshot references, metadata
and environment defaults. Its directory is private (`0700`) and lease files are
owner-readable/writable (`0600`). Environment defaults are not returned by inspect
or list; list responses also omit sandbox access tokens.

List returns only sandboxes with E2B leases. A native CLI-created sandbox is not
automatically exposed as an E2B resource. Pagination defaults to 100 items, with
a maximum of 100. The opaque `x-next-token` cursor uses creation time and sandbox
ID so deleting an earlier entry does not skip the following page. Keep filters
and sort order unchanged when following a cursor. Listing is live, not a frozen
snapshot of all sandboxes.

## Use the official SDK

First import an appropriate guest image on the host, then start the API:

```sh
sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/kumabox-api.token'
sudo kumabox serve --token-file /etc/kumabox-api.token
```

Use a loopback connection, SSH tunnel or TLS reverse proxy. Configure the client
with the token from that file and the same API URL for both protocol surfaces:

```sh
export E2B_API_URL=http://127.0.0.1:8765
export E2B_SANDBOX_URL=http://127.0.0.1:8765
export E2B_API_KEY=YOUR_SERVER_TOKEN
```

These examples assume an imported image alias `ubuntu`. Replace it with your
local image reference. E2B's default template name `base` needs that alias to
exist locally as well.

```python
from e2b import Sandbox, SandboxQuery

sandbox = Sandbox.create(
    "ubuntu", timeout=300,
    metadata={"project": "demo"}, envs={"APP_MODE": "test"},
)
try:
    result = sandbox.commands.run("printf '%s\\n' \"$APP_MODE\"")
    print(result.stdout)
    # This override affects this command only.
    sandbox.commands.run("echo $APP_MODE", envs={"APP_MODE": "once"})
    print(sandbox.get_info().metadata)
    paginator = Sandbox.list(query=SandboxQuery(metadata={"project": "demo"}))
    while paginator.has_next:
        print(paginator.next_items())
finally:
    sandbox.kill()
```

```ts
import { Sandbox } from 'e2b'

const sandbox = await Sandbox.create('ubuntu', {
  timeoutMs: 300_000,
  metadata: { project: 'demo' }, envs: { APP_MODE: 'test' },
})
try {
  await sandbox.commands.run('echo $APP_MODE', { onStdout: chunk => process.stdout.write(chunk) })
  const paginator = Sandbox.list({ query: { metadata: { project: 'demo' } } })
  while (paginator.hasNext) console.log(await paginator.nextItems())
} finally {
  await sandbox.kill()
}
```

## Limits and validation

This remains experimental compatibility, not a complete E2B implementation.
PTY, process attach/list/kill, background command management, stdin streaming,
custom working directories, filesystem watches/directory APIs, filesystem-only
snapshots, template builds, network policies, MCP, IAM and volume mounts are
not supported. Do not use `background=True` / `background: true`: the current
process endpoint is a foreground stream, and its reported PID is not a
reconnectable guest process handle. A native sandbox being stopped is not enough
to qualify as an E2B paused sandbox; resume requires a recorded memory snapshot.

The default TTL is 300 seconds. Explicit values must be between 1 and 86400
seconds. Expiration cleanup can be delayed by an in-flight operation or cleanup
failure; deadlines are persisted and reloaded on API restart.

Protocol contract tests exercise published **Python `e2b==2.52.0`** and
**TypeScript `e2b@2.55.0`** packages against a real HTTP adapter with application
test doubles. They cover metadata encoding, list filters, environment defaults,
foreground commands, connect and snapshot cloning. They do not boot a VM.

```sh
python3 -m pip install --target /tmp/kumabox-e2b-python 'e2b==2.52.0'
npm install --prefix /tmp/kumabox-e2b-node --no-audit --no-fund e2b@2.55.0
KUMABOX_E2B_PYTHON_PATH=/tmp/kumabox-e2b-python \
KUMABOX_E2B_NODE_MODULE=/tmp/kumabox-e2b-node/node_modules/e2b \
go test -race ./api/e2b
```

Protocol references: [environment variables](https://docs.e2b.dev/sandbox/environment-variables),
[metadata](https://docs.e2b.dev/sandbox/metadata),
[sandbox listing](https://docs.e2b.dev/sandbox/list).
