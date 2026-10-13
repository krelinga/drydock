// A Drydock in memory, over the fake transport, for the flows' unit specs:
// the routes E1 reads and writes, answering in the server's shapes (core's
// fixtures, which the web UI's specs hold to the same wire), and a stream
// the spec writes events to. What it records — every request — is what a
// "sent nothing" assertion reads. The client tier holds the real server to
// the same shapes.

import { CLONE_OK, WS, catalogBody, wsView } from '../../web/src/core/reducer.fixtures'
import type { CatalogView, StreamEvent, WorkspaceView } from '../../web/src/core/types'
import { DrydockClient } from '../src/client'
import { CredentialStore } from '../src/credential'
import { Live } from '../src/live'
import type { FlowDeps } from '../src/flows'
import type { HttpRequest, HttpResponse } from '../src/ports'
import { originOf } from '../src/settings'
import {
  FakeClock, FakeStream, FakeTransport, FakeWindow, MemoryLog, MemorySecrets, ORIGIN, RecordingOpener, ScriptedPrompter, TOKEN,
  errorResponse, jsonResponse, settingsOf,
} from './fakes'

/** The link internal/vscode builds for WS (its golden form). */
export const LINK = 'vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f6d6f646573745f686572736368656c227d@ssh-remote+devbox/workspaces/repo'
export const NEW_WS = '01JA0000000000000000000099'

const at = (n: number) => new Date(Date.UTC(2026, 9, 4, 12, 0, n)).toISOString()

export function event(id: number, kind: string, ws: string | undefined, data: Record<string, unknown>): StreamEvent {
  return { id, kind, message: kind, level: 'info', at: at(id % 60), ...(ws !== undefined ? { workspace_id: ws } : {}), data }
}

export class FakeDrydock {
  readonly transport = new FakeTransport()
  stream: FakeStream | null = null
  streams = 0
  catalog: CatalogView = catalogBody()
  workspaces: WorkspaceView[] = [wsView({ vscode: { configured: true, url: LINK } } as Partial<WorkspaceView>)]
  cap: number | null = 10
  api = 1
  version = 'v0.12.2'
  nextId = 100
  /** What a POST /api/workspaces writes to the stream: the create's own state event, by default. */
  onCreate: (repoId: number) => StreamEvent[] = (repoId) => [event(this.nextId++, 'workspace.state', NEW_WS, { state: 'pending', repository_id: repoId, branch: 'main' })]

  constructor() {
    this.transport.route = (req) => this.answer(req)
    this.transport.streamRoute = () => {
      this.stream = new FakeStream()
      this.streams++
      return this.stream
    }
  }

  /** Writes events to the open stream as SSE frames. */
  emit(...events: StreamEvent[]): void {
    for (const e of events) this.stream?.push(`id: ${e.id}\ndata: ${JSON.stringify(e)}\n\n`)
  }

  sent(method: string, path: string): HttpRequest[] {
    return this.transport.requests.filter((r) => r.method === method && new URL(r.url).pathname === path)
  }

  mutations(): HttpRequest[] {
    return this.transport.requests.filter((r) => r.method !== 'GET')
  }

  private answer(req: HttpRequest): HttpResponse {
    const path = new URL(req.url).pathname
    if (req.method === 'GET') {
      if (path === '/api/meta') {
        return jsonResponse(200, { version: this.version, api: this.api, credential: { kind: 'token', label: 'VS Code', scopes: ['read', 'lifecycle', 'self'], expires_at: '2099-01-01T00:00:00Z' } })
      }
      if (path === '/api/repos') return jsonResponse(200, this.catalog)
      if (path === '/api/workspaces') {
        const occupied = this.workspaces.filter((v) => ['pending', 'cloning', 'building', 'running'].includes(v.state)).length
        return jsonResponse(200, { workspaces: this.workspaces, capacity: { cap: this.cap, occupied } })
      }
      const m = /^\/api\/workspaces\/([^/]+)$/.exec(path)
      if (m) {
        const v = this.workspaces.find((w) => w.id === m[1])
        return v === undefined ? errorResponse(404, 'not_found') : jsonResponse(200, { ...v, events: [] })
      }
    }
    if (req.method === 'POST' && path === '/api/workspaces') {
      const { repository_id } = JSON.parse(req.body ?? '{}') as { repository_id: number }
      const events = this.onCreate(repository_id)
      queueMicrotask(() => this.emit(...events))
      // A decoy id: the extension must learn the workspace from the stream, never the 202 (§5.4).
      return jsonResponse(202, { id: '01JA00000000000000000DECOY' })
    }
    if (req.method === 'DELETE' && path === '/api/auth/token') return { status: 204, headers: { get: () => null }, text: async () => '' }
    if (req.method === 'POST' && path === '/api/repos/refresh') {
      queueMicrotask(() => this.emit(event(this.nextId++, 'repo.refreshed', undefined, { count: 2, added: 0, removed: 0 })))
      return jsonResponse(202, {})
    }
    return errorResponse(404, 'not_found')
  }
}

export interface FlowWorld extends FlowDeps {
  server: FakeDrydock
  prompter: ScriptedPrompter
  opener: RecordingOpener
  window: FakeWindow
  clock: FakeClock
  log: MemoryLog
  installed: Set<string>
  connectNow: { value: boolean }
  secrets: MemorySecrets
  credentials: CredentialStore
}

/** The flows over a FakeDrydock, signed in, in a window named `remote` (local by default). */
export function flowWorld(remote: string | undefined = undefined): FlowWorld {
  const server = new FakeDrydock()
  const secrets = new MemorySecrets()
  secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN })
  const settings = settingsOf(ORIGIN)
  const clock = new FakeClock()
  const log = new MemoryLog()
  const credentials = new CredentialStore(secrets)
  const client = new DrydockClient({ transport: server.transport, credentials, origin: () => originOf(settings.url()) })
  const live = new Live({ client, clock, log })
  const installed = new Set(['ms-vscode-remote.remote-ssh', 'ms-vscode-remote.remote-containers'])
  const connectNow = { value: false }
  return {
    server, live, client, prompter: new ScriptedPrompter(), opener: new RecordingOpener(), window: new FakeWindow(remote),
    settings, clock, log, ready: new Set(), installed, connectNow, secrets, credentials,
    extensions: { installed: (id) => installed.has(id), install: async (id) => { installed.add(id) } },
    connectWhenReady: () => connectNow.value,
  }
}

export { CLONE_OK, WS }
