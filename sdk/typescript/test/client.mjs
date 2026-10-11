import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createServer } from 'node:http'
import { getEventListeners } from 'node:events'
import { Client, CommandExitError, KumaBoxError, Sandbox } from '../dist/index.js'

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

test('callbacks deliver Unicode output before exit and release the stream', async () => {
  const originalFetch = globalThis.fetch
  const stdout = [], stderr = []
  let canceled = false
  const bytes = new TextEncoder().encode('你🙂')
  const frames = [
    { stream: 'stdout', data: Buffer.from(bytes.slice(0, 1)).toString('base64') },
    { stream: 'stderr', data: Buffer.from('warning').toString('base64') },
    { stream: 'stdout', data: Buffer.from(bytes.slice(1, 4)).toString('base64') },
    { stream: 'stdout', data: Buffer.from(bytes.slice(4)).toString('base64') },
    { exit_code: 0 },
  ]
  const response = new Response(new ReadableStream({
    pull(controller) {
      const frame = frames.shift()
      if ('exit_code' in frame) {
        assert.equal(stdout.join(''), '你🙂')
        assert.equal(stderr.join(''), 'warning')
      }
      controller.enqueue(new TextEncoder().encode(JSON.stringify(frame) + '\n'))
    },
    cancel() { canceled = true },
  }, { highWaterMark: 0 }))
  globalThis.fetch = async () => response
  try {
    const sandbox = new Sandbox(new Client({ token: 'secret' }), { id: 'vm-1' })
    const result = await sandbox.commands.run('echo', {
      onStdout: chunk => stdout.push(chunk), onStderr: chunk => stderr.push(chunk),
    })
    assert.deepEqual(result, { stdout: '你🙂', stderr: 'warning', exitCode: 0 })
    assert.equal(canceled, true)
    assert.equal(response.body.locked, false)
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('callback and protocol errors release command streams', async () => {
  const originalFetch = globalThis.fetch
  try {
    for (const kind of ['callback', 'server', 'truncated']) {
      let canceled = false
      const response = new Response(new ReadableStream({
        start(controller) {
          const frame = kind === 'server'
            ? { error: { code: 'ARTIFACT_UNAVAILABLE', message: 'stopped' } }
            : { stream: 'stdout', data: 'aGk=' }
          controller.enqueue(new TextEncoder().encode(JSON.stringify(frame) + '\n'))
          if (kind === 'truncated') controller.close()
        },
        cancel() { canceled = true },
      }))
      globalThis.fetch = async () => response
      const sandbox = new Sandbox(new Client({ token: 'secret' }), { id: 'vm-1' })
      await assert.rejects(sandbox.commands.run('echo', {
        onStdout() { if (kind === 'callback') throw new Error('callback failed') },
      }), error => {
        if (kind === 'callback') assert.equal(error.message, 'callback failed')
        else {
          assert.ok(error instanceof KumaBoxError)
          assert.equal(error.code, kind === 'server' ? 'ARTIFACT_UNAVAILABLE' : 'TRUNCATED_STREAM')
        }
        return true
      })
      assert.equal(response.body.locked, false)
      if (kind !== 'truncated') assert.equal(canceled, true)
    }
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('callbacks flush incomplete Unicode before a nonzero exit', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = async () => new Response('{"stream":"stderr","data":"4g=="}\n{"exit_code":7}\n')
  const output = []
  try {
    const sandbox = new Sandbox(new Client({ token: 'secret' }), { id: 'vm-1' })
    await assert.rejects(sandbox.commands.run('false', { onStderr: chunk => output.push(chunk) }), error => {
      assert.ok(error instanceof CommandExitError)
      assert.equal(error.stderr, '\ufffd')
      assert.equal(output.join(''), error.stderr)
      return true
    })
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('create maps data disks and shared memory without serializing transport options', async () => {
  const originalFetch = globalThis.fetch
  const signal = new AbortController().signal
  let received
  globalThis.fetch = async (_url, options) => {
    received = JSON.parse(options.body)
    return new Response(JSON.stringify({ id: 'vm-1', shared_memory: true, data_disks: received.data_disks }))
  }
  try {
    const sandbox = await new Client({ token: 'secret' }).create('ubuntu', {
      sharedMemory: true, dataDisks: [{ name: 'data', size: 256 * 1024 ** 2, directIO: false }],
      nics: 0, cpus: 2, memory: 1024 ** 3, start: false, requestTimeoutMs: 1000, signal,
    })
    assert.deepEqual(received, {
      image: 'ubuntu', cpus: 2, memory: 1024 ** 3, nics: 0, start: false,
      shared_memory: true, data_disks: [{ name: 'data', size: 256 * 1024 ** 2, fstype: 'ext4', direct_io: false }],
    })
    assert.equal(sandbox.data.shared_memory, true)
    assert.equal(getEventListeners(signal, 'abort').length, 0)
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('command deadlines stay distinct from HTTP timeouts and callbacks', async () => {
  const originalFetch = globalThis.fetch
  let received
  globalThis.fetch = async (_url, options) => {
    received = JSON.parse(options.body)
    return new Response('{"error":{"code":"COMMAND_TIMEOUT","message":"execution deadline"}}\n')
  }
  try {
    const sandbox = new Sandbox(new Client({ token: 'secret' }), { id: 'vm-1' })
    await assert.rejects(sandbox.commands.run('sleep 10', { timeoutMs: 5000, requestTimeoutMs: 6000 }), error => {
      assert.equal(error.code, 'COMMAND_TIMEOUT')
      return true
    })
    assert.equal(received.timeout_ms, 5000)
    assert.equal('requestTimeoutMs' in received, false)
  } finally {
    globalThis.fetch = originalFetch
  }
})

// A local HTTP server catches the common bug where a timer is cleared as soon
// as headers arrive, leaving a stalled JSON/file/exec body unbounded.
test('HTTP deadlines cover headers and every response body', { timeout: 10_000 }, async t => {
  let requests = 0
  const server = createServer((request, response) => {
    requests++
    if (request.url === '/v1/sandboxes/no-headers') return
    response.writeHead(200)
    if (request.url.endsWith('/exec')) response.write('{"stream":"stdout","data":"aGk="}\n')
    else response.write('[')
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(async () => {
    server.closeAllConnections()
    await new Promise(resolve => server.close(resolve))
  })
  const client = new Client({ token: 'secret', baseUrl: `http://127.0.0.1:${server.address().port}`, requestTimeoutMs: 0 })
  const sandbox = new Sandbox(client, { id: 'vm-1' })
  for (const [name, run] of [
    ['headers', () => client.connect('no-headers', { requestTimeoutMs: 200 })],
    ['JSON', () => client.list({ requestTimeoutMs: 200 })],
    ['file', () => sandbox.files.read('/tmp/file', { requestTimeoutMs: 200 })],
    ['exec', () => sandbox.commands.run('sleep 10', { requestTimeoutMs: 200 })],
  ]) {
    await t.test(name, async () => {
      await assert.rejects(run(), error => {
        assert.ok(error instanceof KumaBoxError)
        assert.equal(error.code, 'REQUEST_TIMEOUT')
        return true
      })
    })
  }
  assert.equal(requests, 4, 'timed-out requests must not be retried')
})

test('AbortSignal cancels an active exec stream and detaches its listener', { timeout: 10_000 }, async t => {
  let disconnected
  const closed = new Promise(resolve => { disconnected = resolve })
  const server = createServer((_request, response) => {
    response.on('close', disconnected)
    response.writeHead(200)
    response.write('{"stream":"stdout","data":"aGk="}\n')
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(async () => {
    server.closeAllConnections()
    await new Promise(resolve => server.close(resolve))
  })
  const controller = new AbortController()
  const client = new Client({ token: 'secret', baseUrl: `http://127.0.0.1:${server.address().port}` })
  const sandbox = new Sandbox(client, { id: 'vm-1' })
  await assert.rejects(sandbox.commands.run('sleep 10', {
    signal: controller.signal,
    onStdout: () => controller.abort(),
  }), error => {
    assert.equal(error.code, 'REQUEST_CANCELED')
    return true
  })
  await closed
  assert.equal(getEventListeners(controller.signal, 'abort').length, 0)
})

test('invalid timeouts and pre-aborted signals send no requests', async () => {
  const originalFetch = globalThis.fetch
  let calls = 0
  globalThis.fetch = async () => { calls++; throw new Error('must not send') }
  try {
    const client = new Client({ token: 'secret' })
    const sandbox = new Sandbox(client, { id: 'vm-1' })
    for (const value of [-1, NaN, Infinity, 1.5, 86_400_001]) {
      assert.throws(() => new Client({ token: 'secret', requestTimeoutMs: value }), RangeError)
      await assert.rejects(client.list({ requestTimeoutMs: value }), RangeError)
    }
    await assert.rejects(sandbox.commands.run('true', { timeoutMs: 0 }), RangeError)
    const controller = new AbortController()
    controller.abort()
    await assert.rejects(client.create('ubuntu', { signal: controller.signal }), error => {
      assert.equal(error.code, 'REQUEST_CANCELED')
      return true
    })
    assert.equal(calls, 0)
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('network failures are typed and create is not retried', async () => {
  const originalFetch = globalThis.fetch
  let calls = 0
  globalThis.fetch = async () => { calls++; throw new TypeError('fetch failed') }
  try {
    await assert.rejects(new Client({ token: 'secret' }).create('ubuntu'), error => {
      assert.ok(error instanceof KumaBoxError)
      assert.equal(error.code, 'CONNECTION_ERROR')
      return true
    })
    assert.equal(calls, 1)
  } finally {
    globalThis.fetch = originalFetch
  }
})
