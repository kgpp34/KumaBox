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
}

export interface SnapshotData {
  id: string
  name?: string
  description?: string
  sandbox_id: string
  image_digest: string
  size: number
  created_at: string
}

export interface CreateSandboxOptions {
  name?: string
  cpus?: number
  memory?: number
  storage?: number
  nics?: number
  network?: string
  start?: boolean
}

export interface CommandResult {
  stdout: string
  stderr: string
  exitCode: number
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

  constructor(options: { baseUrl?: string; token: string }) {
    if (!options.token) throw new Error('KumaBox API token is required')
    this.baseUrl = (options.baseUrl ?? 'http://127.0.0.1:8765').replace(/\/$/, '')
    this.token = options.token
  }

  async request<T>(method: string, path: string, body?: object): Promise<T> {
    const response = await this.fetch(method, path, body)
    return response.json() as Promise<T>
  }

  async fetch(method: string, path: string, body?: object): Promise<Response> {
    const response = await fetch(this.baseUrl + path, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        'Content-Type': 'application/json',
      },
      body: body === undefined ? undefined : JSON.stringify(body),
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

  /** Create a sandbox from an imported image and start it by default. */
  async create(image: string, options: CreateSandboxOptions = {}): Promise<Sandbox> {
    const data = await this.request<SandboxData>('POST', '/v1/sandboxes', { image, ...options })
    return new Sandbox(this, data)
  }

  /** Attach a handle to an existing sandbox by ID or name. */
  async connect(reference: string): Promise<Sandbox> {
    const data = await this.request<SandboxData>('GET', `/v1/sandboxes/${encodeURIComponent(reference)}`)
    return new Sandbox(this, data)
  }

  async list(): Promise<Sandbox[]> {
    return (await this.request<SandboxData[]>('GET', '/v1/sandboxes')).map(data => new Sandbox(this, data))
  }

  async snapshots(): Promise<Snapshot[]> {
    return (await this.request<SnapshotData[]>('GET', '/v1/snapshots')).map(data => new Snapshot(this, data))
  }

  async snapshot(reference: string): Promise<Snapshot> {
    return new Snapshot(this, await this.request<SnapshotData>('GET', `/v1/snapshots/${encodeURIComponent(reference)}`))
  }
}

/** A durable sandbox with lifecycle and command operations. */
export class Sandbox {
  readonly commands: Commands

  constructor(readonly client: Client, public data: SandboxData) {
    this.commands = new Commands(this)
  }

  get sandboxId(): string { return this.data.id }
  get state(): string { return this.data.state }

  async refresh(): Promise<this> {
    this.data = (await this.client.connect(this.sandboxId)).data
    return this
  }

  async start(): Promise<this> {
    this.data = await this.client.request<SandboxData>('POST', `/v1/sandboxes/${this.sandboxId}/start`)
    return this
  }

  async stop(): Promise<this> {
    this.data = await this.client.request<SandboxData>('POST', `/v1/sandboxes/${this.sandboxId}/stop`)
    return this
  }

  async kill(): Promise<void> {
    await this.client.request('DELETE', `/v1/sandboxes/${this.sandboxId}`)
  }

  async saveSnapshot(name = '', description = ''): Promise<Snapshot> {
    const data = await this.client.request<SnapshotData>('POST', '/v1/snapshots', {
      sandbox: this.sandboxId, name, description,
    })
    return new Snapshot(this.client, data)
  }

  async hibernate(name = '', description = ''): Promise<Snapshot> {
    const data = await this.client.request<SnapshotData>('POST', `/v1/sandboxes/${this.sandboxId}/hibernate`, { name, description })
    return new Snapshot(this.client, data)
  }

  async restore(snapshot: string): Promise<this> {
    this.data = await this.client.request<SandboxData>('POST', `/v1/sandboxes/${this.sandboxId}/restore`, { snapshot })
    return this
  }
}

/** Guest command execution using a shell string or explicit argv. */
export class Commands {
  constructor(readonly sandbox: Sandbox) {}

  async run(command: string | string[], options: { envs?: Record<string, string>; stdin?: Uint8Array; check?: boolean } = {}): Promise<CommandResult> {
    const args = typeof command === 'string' ? ['sh', '-lc', command] : command
    const response = await this.sandbox.client.fetch('POST', `/v1/sandboxes/${this.sandbox.sandboxId}/exec`, {
      args,
      env: options.envs ?? {},
      stdin: encodeBase64(options.stdin ?? new Uint8Array()),
    })
    if (!response.body) throw new KumaBoxError('TRUNCATED_STREAM', 'command response has no body')
    const output: Record<'stdout' | 'stderr', Uint8Array[]> = { stdout: [], stderr: [] }
    const decoder = new TextDecoder()
    let pending = ''
    const reader = response.body.getReader()
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
          const result = {
            stdout: decodeChunks(output.stdout),
            stderr: decodeChunks(output.stderr),
            exitCode: event.exit_code,
          }
          if (options.check !== false && result.exitCode !== 0) throw new CommandExitError(result)
          return result
        }
        if (event.stream !== 'stdout' && event.stream !== 'stderr') throw new KumaBoxError('INVALID_STREAM', 'unexpected command stream')
        output[event.stream].push(decodeBase64(event.data ?? ''))
      }
    }
    throw new KumaBoxError('TRUNCATED_STREAM', 'command response ended without exit status')
  }
}

/** One captured sandbox state. */
export class Snapshot {
  constructor(readonly client: Client, readonly data: SnapshotData) {}
  get snapshotId(): string { return this.data.id }

  async clone(name: string): Promise<Sandbox> {
    const data = await this.client.request<SandboxData>('POST', `/v1/snapshots/${this.snapshotId}/clone`, { name })
    return new Sandbox(this.client, data)
  }

  async remove(): Promise<void> {
    await this.client.request('DELETE', `/v1/snapshots/${this.snapshotId}`)
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
