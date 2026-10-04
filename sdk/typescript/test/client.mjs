import assert from 'node:assert/strict'
import { test } from 'node:test'
import { Client, CommandExitError, Sandbox } from '../dist/index.js'

test('create and collect streamed command output', async () => {
  const originalFetch = globalThis.fetch
  const requests = []
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options })
    if (requests.length === 1) {
      return new Response(JSON.stringify({ id: 'vm-1', state: 'running' }), { status: 201 })
    }
    const frames = [
      { stream: 'stdout', data: 'aGVsbG8=' },
      { stream: 'stderr', data: 'd2FybmluZw==' },
      { exit_code: 0 },
    ]
    return new Response(frames.map(frame => JSON.stringify(frame)).join('\n') + '\n')
  }
  try {
    const sandbox = await new Client({ token: 'secret' }).create('ubuntu')
    const result = await sandbox.commands.run('echo hello')
    assert.deepEqual(result, { stdout: 'hello', stderr: 'warning', exitCode: 0 })
    assert.equal(JSON.parse(requests[0].options.body).image, 'ubuntu')
    assert.deepEqual(JSON.parse(requests[1].options.body).args, ['sh', '-lc', 'echo hello'])
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('nonzero command exits raise with collected output', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = async () => new Response('{"stream":"stderr","data":"YmFk"}\n{"exit_code":7}\n')
  try {
    const sandbox = new Sandbox(new Client({ token: 'secret' }), { id: 'vm-1', state: 'running' })
    await assert.rejects(sandbox.commands.run('false'), error => {
      assert.ok(error instanceof CommandExitError)
      assert.equal(error.exitCode, 7)
      assert.equal(error.stderr, 'bad')
      return true
    })
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('files transfer exact bytes through native API', async () => {
  const originalFetch = globalThis.fetch
  const requests = []
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options })
    return requests.length === 1
      ? new Response(JSON.stringify({ name: 'sample.bin', path: '/tmp/sample.bin' }))
      : new Response(new Uint8Array([0, 1, 255]))
  }
  try {
    const sandbox = new Sandbox(new Client({ token: 'secret' }), { id: 'vm-1', state: 'running' })
    assert.equal((await sandbox.files.write('/tmp/sample.bin', new Uint8Array([0, 1, 255]))).path, '/tmp/sample.bin')
    assert.deepEqual(await sandbox.files.read('/tmp/sample.bin', { format: 'bytes' }), new Uint8Array([0, 1, 255]))
    assert.match(requests[0].url, /\/files\?path=%2Ftmp%2Fsample.bin$/)
    assert.equal(requests[0].options.headers['Content-Type'], 'application/octet-stream')
  } finally {
    globalThis.fetch = originalFetch
  }
})
