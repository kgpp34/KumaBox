/** KumaBox HTTP client and E2B-style sandbox handles. */

export interface SandboxData {
  id: string
  name: string
  image_digest: string
  state: string
  cpus: number
  memory: number
  storage: number
  nics: number
  generation: number
  created_at: string
  updated_at: string
  vmm: string
  network?: string
  shared_memory: boolean
  data_disks?: DataDisk[]
}

export interface SnapshotData {
  id: string
  name?: string
  description?: string
  sandbox_id: string
  image_digest: string
  size: number
  created_at: string
  vmm: string
}

/** The native API projection of a sandbox-owned data volume; sizes are bytes. */
export interface DataDisk {
  name: string
  size: number
  fstype: string
  direct_io?: boolean
}

/** SDK input for one data volume. Filesystem validation belongs to the server. */
export interface DataDiskOptions {
  name: string
  size: number
  fstype?: string
  directIO?: boolean
}

/** HTTP deadline covers headers and the response body. Zero disables it. */
export interface RequestOptions {
  requestTimeoutMs?: number
  signal?: AbortSignal
}

export interface CreateSandboxOptions extends RequestOptions {
  name?: string
  cpus?: number
  memory?: number
  storage?: number
  nics?: number
  network?: string
  start?: boolean
  sharedMemory?: boolean
  dataDisks?: DataDiskOptions[]
}

export interface CommandResult {
  stdout: string
  stderr: string
  exitCode: number
}

/** Foreground execution options. Callbacks receive decoded output as it arrives. */
export interface CommandOptions extends RequestOptions {
  /** Server-side execution deadline, independent of the HTTP deadline. */
  timeoutMs?: number
  envs?: Record<string, string>
  stdin?: Uint8Array
  check?: boolean
  onStdout?: (chunk: string) => void
  onStderr?: (chunk: string) => void
}

export class KumaBoxError extends Error {
  constructor(
    readonly code: string,
    message: string,
    readonly committed = false,
    readonly resourceId = '',
  ) {
    super(message)
    this.name = 'KumaBoxError'
  }
}

export class CommandExitError extends KumaBoxError {
  constructor(readonly result: CommandResult) {
    super('COMMAND_EXIT', `guest command exited with status ${result.exitCode}`)
    this.name = 'CommandExitError'
  }

  get exitCode(): number { return this.result.exitCode }
  get stdout(): string { return this.result.stdout }
  get stderr(): string { return this.result.stderr }
}

/** Connection to one KumaBox API server. */
export class Client {
  readonly baseUrl: string
  readonly token: string
  readonly requestTimeoutMs: number

  constructor(options: { baseUrl?: string; token: string; requestTimeoutMs?: number }) {
    if (!options.token) throw new Error('KumaBox API token is required')
    this.baseUrl = (options.baseUrl ?? 'http://127.0.0.1:8765').replace(/\/$/, '')
    this.token = options.token
    this.requestTimeoutMs = options.requestTimeoutMs ?? 30_000
    validateTimeout(this.requestTimeoutMs, true)
  }

  async request<T>(method: string, path: string, body?: object, options: RequestOptions = {}): Promise<T> {
    return this.withResponse(method, path, body, options, response => response.json() as Promise<T>)
  }

  /** Low-level fetch. The caller owns response consumption and cancellation. */
  async fetch(method: string, path: string, body?: object | Uint8Array, signal?: AbortSignal): Promise<Response> {
    const binary = body instanceof Uint8Array
    const response = await fetch(this.baseUrl + path, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        'Content-Type': binary ? 'application/octet-stream' : 'application/json',
      },
      body: body === undefined ? undefined : binary ? new Blob([new Uint8Array(body as Uint8Array)]) : JSON.stringify(body),
      signal,
    }).catch(error => {
      if (signal?.aborted) throw error
      throw new KumaBoxError('CONNECTION_ERROR', 'HTTP connection failed')
    })
    if (!response.ok) {
      const failure = await response.json().catch(() => null) as
        | { error?: { code?: string; message?: string; committed?: boolean; resource_id?: string } }
        | null
      const error = failure?.error
      throw new KumaBoxError(error?.code ?? 'HTTP_ERROR', error?.message ?? `HTTP ${response.status}`, error?.committed, error?.resource_id)
    }
    return response
  }

  /** Keep the deadline active through JSON parsing, file reads and exec streams. */
  async withResponse<T>(
    method: string, path: string, body: object | Uint8Array | undefined,
    options: RequestOptions, consume: (response: Response) => Promise<T>,
  ): Promise<T> {
    const timeout = options.requestTimeoutMs ?? this.requestTimeoutMs
    validateTimeout(timeout, true)
    const controller = new AbortController()
    let timedOut = false
    const abort = () => controller.abort(options.signal?.reason)
    if (options.signal?.aborted) abort()
    else options.signal?.addEventListener('abort', abort, { once: true })
    const timer = timeout === 0 ? undefined : setTimeout(() => {
      timedOut = true
      controller.abort()
    }, timeout)
    let response: Response | undefined
    try {
      controller.signal.throwIfAborted()
      response = await this.fetch(method, path, body, controller.signal)
      const result = await consume(response)
      controller.signal.throwIfAborted()
      return result
    } catch (error) {
      if (controller.signal.aborted) {
        throw new KumaBoxError(timedOut ? 'REQUEST_TIMEOUT' : 'REQUEST_CANCELED',
          timedOut ? 'HTTP request deadline exceeded' : 'HTTP request canceled')
      }
      throw error
    } finally {
      clearTimeout(timer)
      options.signal?.removeEventListener('abort', abort)
      if (response?.body && !response.body.locked) {
        try { await response.body.cancel() } catch { /* Already consumed or closed. */ }
      }
    }
  }

  /** Create a sandbox from an imported image and start it by default. */
  async create(image: string, options: CreateSandboxOptions = {}): Promise<Sandbox> {
    const data = await this.request<SandboxData>('POST', '/v1/sandboxes', {
      image, name: options.name, cpus: options.cpus, memory: options.memory,
      storage: options.storage, nics: options.nics, network: options.network, start: options.start,
      shared_memory: options.sharedMemory,
      data_disks: options.dataDisks?.map(disk => ({
        name: disk.name, size: disk.size, fstype: disk.fstype ?? 'ext4', direct_io: disk.directIO,
      })),
    }, options)
    return new Sandbox(this, data)
  }

  /** Attach a handle to an existing sandbox by ID or name. */
  async connect(reference: string, options: RequestOptions = {}): Promise<Sandbox> {
    const data = await this.request<SandboxData>('GET', `/v1/sandboxes/${encodeURIComponent(reference)}`, undefined, options)
    return new Sandbox(this, data)
  }

  async list(options: RequestOptions = {}): Promise<Sandbox[]> {
    return (await this.request<SandboxData[]>('GET', '/v1/sandboxes', undefined, options)).map(data => new Sandbox(this, data))
  }

  async snapshots(options: RequestOptions = {}): Promise<Snapshot[]> {
    return (await this.request<SnapshotData[]>('GET', '/v1/snapshots', undefined, options)).map(data => new Snapshot(this, data))
  }

  async snapshot(reference: string, options: RequestOptions = {}): Promise<Snapshot> {
    return new Snapshot(this, await this.request<SnapshotData>('GET', `/v1/snapshots/${encodeURIComponent(reference)}`, undefined, options))
  }
}

/** A durable sandbox with lifecycle and command operations. */
export class Sandbox {
  readonly commands: Commands
  readonly files: Files

  constructor(readonly client: Client, public data: SandboxData) {
    this.commands = new Commands(this)
    this.files = new Files(this)
  }

  get sandboxId(): string { return this.data.id }
  get state(): string { return this.data.state }

  async refresh(options: RequestOptions = {}): Promise<this> {
    this.data = (await this.client.connect(this.sandboxId, options)).data
    return this
  }

  async start(options: RequestOptions = {}): Promise<this> {
    this.data = await this.client.request<SandboxData>('POST', `/v1/sandboxes/${this.sandboxId}/start`, undefined, options)
    return this
  }

  async stop(options: RequestOptions = {}): Promise<this> {
    this.data = await this.client.request<SandboxData>('POST', `/v1/sandboxes/${this.sandboxId}/stop`, undefined, options)
    return this
  }

  async kill(options: RequestOptions = {}): Promise<void> {
    await this.client.request('DELETE', `/v1/sandboxes/${this.sandboxId}`, undefined, options)
  }

  async saveSnapshot(name = '', description = '', options: RequestOptions = {}): Promise<Snapshot> {
    const data = await this.client.request<SnapshotData>('POST', '/v1/snapshots', {
      sandbox: this.sandboxId, name, description,
    }, options)
    return new Snapshot(this.client, data)
  }

  async hibernate(name = '', description = '', options: RequestOptions = {}): Promise<Snapshot> {
    const data = await this.client.request<SnapshotData>('POST', `/v1/sandboxes/${this.sandboxId}/hibernate`, { name, description }, options)
    return new Snapshot(this.client, data)
  }

  async restore(snapshot: string, options: RequestOptions = {}): Promise<this> {
    this.data = await this.client.request<SandboxData>('POST', `/v1/sandboxes/${this.sandboxId}/restore`, { snapshot }, options)
    return this
  }
}

/** Binary-safe guest file transfers over the native KumaBox API. */
export class Files {
  constructor(readonly sandbox: Sandbox) {}

  private route(path: string): string {
    if (!path.startsWith('/') || path.includes('\0')) throw new Error('file path must be absolute and contain no NUL bytes')
    return `/v1/sandboxes/${encodeURIComponent(this.sandbox.sandboxId)}/files?path=${encodeURIComponent(path)}`
  }

  async read(path: string, options: RequestOptions & { format?: 'text' | 'bytes' } = {}): Promise<string | Uint8Array> {
    return this.sandbox.client.withResponse('GET', this.route(path), undefined, options, async response =>
      options.format === 'bytes' ? new Uint8Array(await response.arrayBuffer()) : response.text())
  }

  async write(path: string, data: string | Uint8Array, options: RequestOptions = {}): Promise<{ name: string; path: string }> {
    const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : data
    return this.sandbox.client.withResponse('POST', this.route(path), bytes, options,
      response => response.json() as Promise<{ name: string; path: string }>)
  }
}

/** Guest command execution using a shell string or explicit argv. */
export class Commands {
  constructor(readonly sandbox: Sandbox) {}

  async run(command: string | string[], options: CommandOptions = {}): Promise<CommandResult> {
    if (options.timeoutMs !== undefined) validateTimeout(options.timeoutMs, false)
    const args = typeof command === 'string' ? ['sh', '-lc', command] : command
    return this.sandbox.client.withResponse('POST', `/v1/sandboxes/${this.sandbox.sandboxId}/exec`, {
      args,
      env: options.envs ?? {},
      stdin: encodeBase64(options.stdin ?? new Uint8Array()),
      timeout_ms: options.timeoutMs,
    }, options, response => this.collect(response, options))
  }

  private async collect(response: Response, options: CommandOptions): Promise<CommandResult> {
    if (!response.body) throw new KumaBoxError('TRUNCATED_STREAM', 'command response has no body')
    const output: Record<'stdout' | 'stderr', Uint8Array[]> = { stdout: [], stderr: [] }
    const callbacks = { stdout: options.onStdout, stderr: options.onStderr }
    const outputDecoders = { stdout: new TextDecoder(), stderr: new TextDecoder() }
    const decoder = new TextDecoder()
    let pending = ''
    const reader = response.body.getReader()
    try {
      while (true) {
        const { value: chunk, done } = await reader.read()
        if (done) break
        pending += decoder.decode(chunk, { stream: true })
        let newline: number
        while ((newline = pending.indexOf('\n')) !== -1) {
          const line = pending.slice(0, newline)
          pending = pending.slice(newline + 1)
          if (!line) continue
          const event = JSON.parse(line) as { stream?: string; data?: string; exit_code?: number; error?: { code: string; message: string; committed?: boolean; resource_id?: string } }
          if (event.error) throw new KumaBoxError(event.error.code, event.error.message, event.error.committed, event.error.resource_id)
          if (event.exit_code !== undefined) {
            for (const name of ['stdout', 'stderr'] as const) {
              const tail = outputDecoders[name].decode()
              if (tail) callbacks[name]?.(tail)
            }
            const result = {
              stdout: decodeChunks(output.stdout),
              stderr: decodeChunks(output.stderr),
              exitCode: event.exit_code,
            }
            if (options.check !== false && result.exitCode !== 0) throw new CommandExitError(result)
            return result
          }
          if (event.stream !== 'stdout' && event.stream !== 'stderr') throw new KumaBoxError('INVALID_STREAM', 'unexpected command stream')
          const bytes = decodeBase64(event.data ?? '')
          output[event.stream].push(bytes)
          const text = outputDecoders[event.stream].decode(bytes, { stream: true })
          if (text) callbacks[event.stream]?.(text)
        }
      }
      throw new KumaBoxError('TRUNCATED_STREAM', 'command response ended without exit status')
    } finally {
      // Returning at the exit frame or throwing from a callback must release
      // the response stream too. A cleanup failure must not mask the result.
      try { await reader.cancel() } catch { /* The transport may already be closed. */ }
      reader.releaseLock()
    }
  }
}

/** One captured sandbox state. */
export class Snapshot {
  constructor(readonly client: Client, readonly data: SnapshotData) {}
  get snapshotId(): string { return this.data.id }

  async clone(name: string, options: RequestOptions = {}): Promise<Sandbox> {
    const data = await this.client.request<SandboxData>('POST', `/v1/snapshots/${this.snapshotId}/clone`, { name }, options)
    return new Sandbox(this.client, data)
  }

  async remove(options: RequestOptions = {}): Promise<void> {
    await this.client.request('DELETE', `/v1/snapshots/${this.snapshotId}`, undefined, options)
  }
}

function validateTimeout(value: number, allowZero: boolean): void {
  if (!Number.isInteger(value) || value < (allowZero ? 0 : 1) || value > 86_400_000) {
    throw new RangeError(`timeout must be an integer between ${allowZero ? 0 : 1} and 86400000 milliseconds`)
  }
}

function encodeBase64(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary)
}

function decodeBase64(value: string): Uint8Array {
  const binary = atob(value)
  return Uint8Array.from(binary, character => character.charCodeAt(0))
}

function decodeChunks(chunks: Uint8Array[]): string {
  const length = chunks.reduce((total, chunk) => total + chunk.length, 0)
  const combined = new Uint8Array(length)
  let offset = 0
  for (const chunk of chunks) {
    combined.set(chunk, offset)
    offset += chunk.length
  }
  return new TextDecoder().decode(combined)
}
