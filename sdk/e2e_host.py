#!/usr/bin/env python3
"""Build, import, and test KumaBox API plus native and E2B SDKs on a Linux/KVM host."""

import json
import os
import platform
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen

REQUIRED_COMMIT = "1bc169b"
GITEE = "https://gitee.com/kgpp34/KumaBox.git"
BRANCH = "rewrite/p12-v1"


def run(*args, env=None, cwd=None):
    print("+", " ".join(map(str, args)), flush=True)
    subprocess.run(args, check=True, env=env, cwd=cwd)


def output(*args):
    return subprocess.check_output(args, text=True).strip()


def stage(name):
    print(f"\n==> {name}", flush=True)


def http(method, path, body=None, *, sandbox_id=None, envd_token=None):
    headers = {}
    if envd_token is None:
        headers["X-API-Key"] = os.environ["KUMABOX_API_TOKEN"]
    else:
        headers["E2b-Sandbox-Id"] = sandbox_id
        headers["X-Access-Token"] = envd_token
    if isinstance(body, (dict, list)):
        body = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    elif body is not None:
        headers["Content-Type"] = "application/octet-stream"
    request = Request(
        os.environ["KUMABOX_API_URL"] + path,
        data=body,
        headers=headers,
        method=method,
    )
    try:
        with urlopen(request, timeout=180) as response:
            return response.status, response.read()
    except HTTPError as error:
        return error.code, error.read()


def expect(wanted, reply):
    status, data = reply
    if status != wanted:
        raise RuntimeError(
            f"HTTP {status}, expected {wanted}: {data.decode(errors='replace')}"
        )
    return json.loads(data) if data else None


def e2b_state(sandbox_id):
    return expect(200, http("GET", f"/sandboxes/{sandbox_id}"))["state"]


def wait_for(sandbox_id, wanted_status, wanted_state=None):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        status, body = http("GET", f"/sandboxes/{sandbox_id}")
        if status == wanted_status:
            if wanted_state is None or json.loads(body)["state"] == wanted_state:
                return
        elif status != 200:
            raise RuntimeError(f"inspect {sandbox_id}: HTTP {status}: {body!r}")
        time.sleep(1)
    raise TimeoutError(f"{sandbox_id} did not reach HTTP {wanted_status}/{wanted_state}")


def test_native_python():
    from kumabox import Client

    stage("Native Python SDK: create, exec, DNS, files, hibernate, restore")
    client = Client()
    box = client.create(
        os.environ["KUMABOX_E2E_IMAGE"],
        name=f"sdk-python-e2e-{os.getpid()}",
    )
    print("sandbox:", box.sandbox_id, flush=True)
    assert box.commands.run("printf native-ok").stdout == "native-ok"
    assert box.commands.run("getent ahostsv4 example.com", check=False).exit_code == 0
    data = bytes([0, 1, 2, 255])
    box.files.write("/tmp/kumabox-sdk-e2e.bin", data)
    assert box.files.read("/tmp/kumabox-sdk-e2e.bin", format="bytes") == data
    snapshot = box.hibernate()
    assert client.connect(box.sandbox_id).state == "stopped"
    box.restore(snapshot.snapshot_id)
    assert box.commands.run("printf restored").stdout == "restored"
    box.stop()
    box.kill()
    snapshot.remove()
    print("Native Python SDK: PASS", flush=True)


def test_e2b_protocol():
    stage("E2B protocol: files, snapshots, pause, resume, autoPause, TTL")
    image = os.environ["KUMABOX_E2E_IMAGE"]
    created = expect(201, http("POST", "/v2/sandboxes", {
        "templateID": image, "timeout": 180, "autoPause": True,
    }))
    sandbox_id = created["sandboxID"]
    envd_token = created["envdAccessToken"]
    print("sandbox:", sandbox_id, flush=True)
    path = "/tmp/kumabox-e2b-e2e.bin"
    route = "/files?path=" + quote(path, safe="")
    data = bytes([0, 1, 2, 255])
    expect(200, http("POST", route, data, sandbox_id=sandbox_id, envd_token=envd_token))
    status, downloaded = http("GET", route, sandbox_id=sandbox_id, envd_token=envd_token)
    assert status == 200 and downloaded == data

    snapshot = expect(201, http(
        "POST", f"/sandboxes/{sandbox_id}/snapshots", {"name": "sdk-e2e-warm"},
    ))
    snapshot_id = snapshot["snapshotID"]
    clone = expect(201, http("POST", "/v2/sandboxes", {"templateID": snapshot_id}))
    clone_id = clone["sandboxID"]
    expect(204, http("DELETE", f"/sandboxes/{clone_id}"))
    expect(204, http("DELETE", f"/templates/{snapshot_id}"))

    expect(204, http("POST", f"/sandboxes/{sandbox_id}/pause", {"memory": True}))
    assert e2b_state(sandbox_id) == "paused"
    expect(200, http(
        "POST", f"/v2/sandboxes/{sandbox_id}/connect", {"timeout": 180},
    ))
    assert e2b_state(sandbox_id) == "running"
    status, downloaded = http("GET", route, sandbox_id=sandbox_id, envd_token=envd_token)
    assert status == 200 and downloaded == data
    expect(204, http(
        "POST", f"/sandboxes/{sandbox_id}/timeout", {"timeout": 3},
    ))
    wait_for(sandbox_id, 200, "paused")
    expect(200, http(
        "POST", f"/v2/sandboxes/{sandbox_id}/connect", {"timeout": 180},
    ))
    expect(204, http("DELETE", f"/sandboxes/{sandbox_id}"))
    wait_for(sandbox_id, 404)

    created = expect(201, http("POST", "/v2/sandboxes", {
        "templateID": image, "timeout": 3,
    }))
    wait_for(created["sandboxID"], 404)
    print("E2B protocol: PASS", flush=True)


def test_native_typescript(repo, work):
    stage("Native TypeScript SDK: create, exec, files, stop, start")
    script = work / "native-ts.mjs"
    script.write_text("""
import assert from 'node:assert/strict'
const { Client } = await import(process.env.KUMABOX_TS_MODULE)
const client = new Client({
  baseUrl: process.env.KUMABOX_API_URL,
  token: process.env.KUMABOX_API_TOKEN,
})
const box = await client.create(process.env.KUMABOX_E2E_IMAGE, {
  name: 'sdk-ts-e2e-' + process.pid,
})
console.log('sandbox:', box.sandboxId)
try {
  assert.equal((await box.commands.run('printf ts-ok')).stdout, 'ts-ok')
  const data = new Uint8Array([0, 1, 2, 255])
  await box.files.write('/tmp/kumabox-ts-e2e.bin', data)
  assert.deepEqual(
    await box.files.read('/tmp/kumabox-ts-e2e.bin', { format: 'bytes' }),
    data,
  )
  await box.stop()
  assert.equal((await box.refresh()).state, 'stopped')
  await box.start()
  assert.equal((await box.commands.run('printf restarted')).stdout, 'restarted')
} finally {
  await box.kill()
}
console.log('Native TypeScript SDK: PASS')
""")
    env = os.environ.copy()
    env["KUMABOX_TS_MODULE"] = (repo / "sdk/typescript/dist/index.js").as_uri()
    run("node", script, env=env)


def test_official_e2b_python():
    from e2b import Sandbox

    stage("Official E2B Python SDK: create, exec, files, pause, connect")
    box = Sandbox.create(os.environ["KUMABOX_E2E_IMAGE"])
    print("sandbox:", box.sandbox_id, flush=True)
    try:
        assert box.commands.run("printf e2b-python").stdout == "e2b-python"
        box.files.write("/tmp/e2b-python.txt", "python-file")
        assert box.files.read("/tmp/e2b-python.txt") == "python-file"
        box.beta_pause()
        box = Sandbox.connect(box.sandbox_id)
        assert box.commands.run("printf resumed").stdout == "resumed"
    finally:
        box.kill()
    print("Official E2B Python SDK: PASS", flush=True)


def test_official_e2b_javascript(work):
    stage("Official E2B JavaScript SDK: create, exec, files, connect")
    script = work / "e2b-node/e2e.mjs"
    script.write_text("""
import assert from 'node:assert/strict'
import { Sandbox } from 'e2b'
const box = await Sandbox.create(process.env.KUMABOX_E2E_IMAGE)
console.log('sandbox:', box.sandboxId)
try {
  assert.equal((await box.commands.run('printf e2b-js')).stdout, 'e2b-js')
  await box.files.write('/tmp/e2b-js.txt', 'js-file')
  assert.equal(await box.files.read('/tmp/e2b-js.txt'), 'js-file')
  const same = await Sandbox.connect(box.sandboxId)
  assert.equal(same.sandboxId, box.sandboxId)
} finally {
  await box.kill()
}
console.log('Official E2B JavaScript SDK: PASS')
""")
    run("node", script)


def run_tests(repo, work):
    test_native_python()
    test_e2b_protocol()
    test_native_typescript(repo, work)
    test_official_e2b_python()
    test_official_e2b_javascript(work)


def main():
    repo = Path.cwd()
    if not (repo / "go.mod").is_file():
        repo = Path.home() / "kumabox/KumaBox"
    if not (repo / "oci-images/ubuntu/Dockerfile").is_file():
        raise RuntimeError("Run from KumaBox repository or put it at ~/kumabox/KumaBox")
    os.chdir(repo)

    for tool in ("git", "go", "make", "docker", "python3", "node", "npm",
                 "curl", "systemd-run"):
        if shutil.which(tool) is None:
            raise RuntimeError(f"Missing prerequisite: {tool}")
    run("sudo", "-v")

    stage("Update source from Gitee if needed")
    has_commit = subprocess.run(
        ["git", "merge-base", "--is-ancestor", REQUIRED_COMMIT, "HEAD"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    ).returncode == 0
    if not has_commit:
        if output("git", "status", "--porcelain", "--untracked-files=no"):
            raise RuntimeError("Repository has local changes; cannot fast-forward safely")
        run("git", "pull", "--ff-only", GITEE, BRANCH)
    run("git", "merge-base", "--is-ancestor", REQUIRED_COMMIT, "HEAD")
    print("Source:", output("git", "rev-parse", "--short", "HEAD"))

    work = Path(tempfile.mkdtemp(prefix="kumabox-sdk-e2e-"))
    stamp = time.strftime("%Y%m%d%H%M%S") + f"-{os.getpid()}"
    tag = f"kumabox/ubuntu:e2e-{stamp}"
    image = f"ubuntu-e2-220261003-e2e-{stamp}"
    unit = f"kumabox-e2e-{stamp}"
    service_started = False
    success = False

    docker = ["docker"]
    if subprocess.run(
        ["docker", "info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    ).returncode != 0:
        docker = ["sudo", "docker"]
        run(*docker, "info")

    try:
        stage("Build and install KumaBox host binary")
        os.environ["GOPROXY"] = os.environ.get("GOPROXY", "https://goproxy.cn,direct")
        run("make", "build")
        run("sudo", "install", "-m", "0755", "bin/kumabox", "/usr/local/bin/kumabox")
        run("sudo", "install", "-m", "0755", "bin/kumabox-check", "/usr/local/bin/kumabox-check")
        run("sudo", "kumabox", "doctor")

        arch = {"x86_64": "linux/amd64", "aarch64": "linux/arm64"}.get(platform.machine())
        if arch is None:
            raise RuntimeError(f"Unsupported architecture: {platform.machine()}")
        stage("Build matching Ubuntu guest image and import Docker archive")
        run(*docker, "buildx", "build", "--load", "--platform", arch,
            "--build-arg", f"GOPROXY={os.environ['GOPROXY']}",
            "-f", "oci-images/ubuntu/Dockerfile", "-t", tag, ".")
        archive = work / "ubuntu.tar"
        run(*docker, "save", "-o", archive, tag)
        run("sudo", "kumabox", "image", "import", image, archive,
            "--format", "docker", "--source-tag", tag)
        run("sudo", "kumabox", "image", "verify", image)
        run("sudo", "kumabox", "image", "inspect", image)

        stage("Install local and official SDKs")
        venv = work / "venv"
        run("python3", "-m", "venv", venv)
        run(venv / "bin/python", "-m", "pip", "install",
            repo / "sdk/python", "e2b==2.5.0")
        run("npm", "ci", "--prefix", repo / "sdk/typescript")
        run("npm", "run", "build", "--prefix", repo / "sdk/typescript")
        node_dir = work / "e2b-node"
        node_dir.mkdir()
        run("npm", "install", "--prefix", node_dir, "--no-save", "e2b@2.6.2")

        stage("Create temporary API key and launch dedicated local API")
        token = secrets.token_hex(32)
        token_file = work / "api.token"
        token_file.write_text(token + "\n")
        token_file.chmod(0o600)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        url = f"http://127.0.0.1:{port}"
        run("sudo", "systemd-run", f"--unit={unit}", "--collect",
            "/usr/local/bin/kumabox", "serve", "--listen",
            f"127.0.0.1:{port}", "--token-file", token_file)
        service_started = True
        os.environ["KUMABOX_API_URL"] = url
        os.environ["KUMABOX_API_TOKEN"] = token
        os.environ["KUMABOX_E2E_IMAGE"] = image
        os.environ["KUMABOX_E2E_WORKDIR"] = str(work)
        os.environ["E2B_API_URL"] = url
        os.environ["E2B_SANDBOX_URL"] = url
        os.environ["E2B_API_KEY"] = token
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                expect(200, http("GET", "/v1/sandboxes"))
                break
            except Exception:
                time.sleep(0.5)
        else:
            raise RuntimeError("API did not become ready")
        unauth = Request(url + "/v1/sandboxes")
        try:
            urlopen(unauth, timeout=5)
            raise AssertionError("Unauthenticated API request succeeded")
        except HTTPError as error:
            assert error.code == 401, error.code
        print("API ready; authentication verified", flush=True)

        stage("Run end-to-end SDK and E2B checks")
        run(venv / "bin/python", Path(__file__), "--tests",
            env=os.environ.copy(), cwd=repo)

        stage("Final resource check and cleanup")
        run("sudo", "kumabox", "ps", "--json")
        run("sudo", "kumabox", "snapshot", "ls", "--json")
        run("sudo", "kumabox", "image", "remove", image)
        success = True
    finally:
        if service_started:
            subprocess.run(["sudo", "systemctl", "stop", unit],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if success:
            subprocess.run([*docker, "image", "rm", tag],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            shutil.rmtree(work)
            print("\nALL END-TO-END CHECKS PASSED", flush=True)
        else:
            print(f"\nFAILED. Test files retained: {work}", file=sys.stderr)
            if service_started:
                subprocess.run(["sudo", "journalctl", "-u", unit, "-n", "80",
                                "--no-pager"], check=False)
            subprocess.run(["sudo", "kumabox", "ps", "--json"], check=False)


if __name__ == "__main__":
    if sys.argv[1:] == ["--tests"]:
        run_tests(Path.cwd(), Path(os.environ["KUMABOX_E2E_WORKDIR"]))
    elif len(sys.argv) == 1:
        main()
    else:
        raise SystemExit("usage: python3 kumabox-full-e2e.py")
