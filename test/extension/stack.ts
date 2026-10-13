// The client tier's stack (vscode-extension §12.4): the real Drydock (the Go
// test TestServeStack in ./stack, built once and run as its own process),
// a tap in front of its API socket, and real Caddy on the shipped Caddyfile
// with a throwaway CA. The extension's client reaches it as it reaches a
// real Drydock: HTTPS to https://drydock.test:<port>, validated against the
// CA through NODE_EXTRA_CA_CERTS and nothing else — never
// rejectUnauthorized: false, never NODE_TLS_REJECT_UNAUTHORIZED.
//
// Node has no host-resolver flag, so drydock.test must resolve to 127.0.0.1
// (/etc/hosts; run.sh checks it), and the UI origin carries Caddy's port,
// which the stack's --ui-origin matches.
//
// Everything mutable is under one temp root, removed at teardown unless
// DRYDOCK_EXTENSION_KEEP is set.

import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { existsSync, mkdirSync, mkdtempSync, openSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { CaddyProc, type CaddyOptions, type CaddySite } from '../harness/caddy'
import { makeCerts } from '../harness/certs'
import { freePort, GaveUp, sleep, waitFor } from '../harness/proc'
import { COOKIE, Tap } from '../harness/tap'

export const UI_HOST = 'drydock.test'
export const PREVIEW_DOMAIN = 'drydock-preview.test'
/** The seeded repositories (stack_test.go). */
export const PLAIN = { id: 101, fullName: 'drydock-fixture/plain' }
export const DIND = { id: 102, fullName: 'drydock-fixture/dind' }

/**
 * The tier's CA and leaves: the UI host, the preview wildcard, and a valid
 * chain for another name (the wrong-host control). Returns the CA's path,
 * for NODE_EXTRA_CA_CERTS.
 */
export function makeTierCerts(dir: string): string {
  makeCerts(dir, [
    { name: 'ui', san: `DNS:${UI_HOST}` },
    { name: 'preview', san: `DNS:*.${PREVIEW_DOMAIN},DNS:${PREVIEW_DOMAIN}` },
    { name: 'other', san: 'DNS:other.test' },
  ], 'Drydock extension-tier throwaway CA')
  return path.join(dir, 'ca.crt')
}

export interface Answer {
  status: number
  body: string
  headers: http.IncomingHttpHeaders
}

export class ExtensionStack implements CaddySite {
  readonly root: string
  readonly repo: string
  readonly uiHost = UI_HOST
  readonly previewDomain = PREVIEW_DOMAIN
  readonly certs: string
  readonly sites: string
  readonly runDir: string
  /** Drydock's own API socket; Caddy reaches it through the tap. */
  readonly drydockSocket: string
  readonly previewSocket: string
  readonly apiSocket: string
  readonly password = `pw-${randomBytes(12).toString('hex')}`
  origin = ''
  tap!: Tap
  caddy!: CaddyProc
  proc: ChildProcess | null = null
  private cookie: string | null = null

  private constructor(repo: string, certs: string) {
    this.repo = repo
    this.root = mkdtempSync(path.join(tmpdir(), 'drydock-extension-'))
    this.certs = certs
    this.sites = path.join(this.root, 'sites')
    // Short: Unix socket paths must fit 107 bytes.
    this.runDir = mkdtempSync(path.join(tmpdir(), 'ddx-'))
    this.drydockSocket = path.join(this.runDir, 'http.sock')
    this.previewSocket = path.join(this.runDir, 'preview.sock')
    this.apiSocket = path.join(this.runDir, 'api-tap.sock')
  }

  /**
   * Starts a stack whose Caddy serves the leaves in `certs` (makeTierCerts):
   * made before the test process starts, since NODE_EXTRA_CA_CERTS is read
   * once, at a process's start.
   */
  static async start(repo: string, certs: string): Promise<ExtensionStack> {
    const s = new ExtensionStack(repo, certs)
    try {
      await s.boot()
    } catch (e) {
      await s.teardown()
      throw e
    }
    return s
  }

  private async boot(): Promise<void> {
    mkdirSync(this.sites, { recursive: true })

    const port = await freePort()
    this.origin = `https://${UI_HOST}:${port}`

    const bin = path.join(this.root, 'bin', 'stack.test')
    execFileSync('go', ['test', '-c', '-o', bin, './test/extension/stack'], { cwd: this.repo, stdio: 'inherit' })
    const ready = path.join(this.root, 'ready')
    const cfg = path.join(this.root, 'stack.json')
    writeFileSync(cfg, JSON.stringify({
      root: this.root, ui_origin: this.origin, ui_host: UI_HOST, preview_domain: PREVIEW_DOMAIN,
      api_socket: this.drydockSocket, preview_socket: this.previewSocket, password: this.password, ready,
    }))
    const log = path.join(this.root, 'stack.log')
    const out = openSync(log, 'a')
    this.proc = spawn(bin, ['-test.run', '^TestServeStack$', '-test.timeout', '0', '-test.v'], {
      cwd: path.join(this.repo, 'test', 'extension', 'stack'),
      env: { ...process.env, DRYDOCK_EXTENSION_STACK: cfg, DRYDOCK_STACK_REPO: this.repo },
      stdio: ['ignore', out, out],
    })
    const proc = this.proc
    await waitFor('the extension stack', () => {
      if (proc.exitCode !== null) throw new GaveUp(`the stack exited ${proc.exitCode}`)
      return existsSync(ready)
    }, log, 600)

    this.tap = new Tap('api', this.apiSocket, this.drydockSocket)
    await this.tap.listening
    this.caddy = await CaddyProc.start(this, 'main', { httpsPort: port })
  }

  /** Another Caddy in front of the same Drydock, on its own port (the wrong-host and second-site controls). */
  async otherCaddy(tag: string, opts: CaddyOptions = {}): Promise<CaddyProc> {
    return CaddyProc.start(this, tag, opts)
  }

  /** The leaf for other.test, valid under the same CA. */
  otherCert(): { cert: string; key: string } {
    return { cert: path.join(this.certs, 'other.crt'), key: path.join(this.certs, 'other.key') }
  }

  /** Switches the docker-in-docker fixture's configuration (stack_test.go's fake devcontainer). */
  dindVariant(v: 'privileged' | 'initialize'): void {
    writeFileSync(path.join(this.root, 'fakestate', 'dind-variant'), v === 'initialize' ? 'initialize\n' : '\n')
  }

  /**
   * The harness's own hand on Drydock's API socket, past the tap and Caddy:
   * the operator's cookie session, signed in once, with the exact Origin.
   * For what the extension does not do itself (minting tokens, starting,
   * declining) and for setting state up.
   */
  async direct(method: string, p: string, body?: unknown, auth: { bearer?: string } = {}): Promise<Answer> {
    if (auth.bearer === undefined && this.cookie === null) await this.signIn()
    const data = body === undefined ? undefined : JSON.stringify(body)
    const headers: Record<string, string | number> = { Host: UI_HOST, Origin: this.origin }
    if (auth.bearer !== undefined) headers.Authorization = `Bearer ${auth.bearer}`
    else headers.Cookie = `${COOKIE}=${this.cookie}`
    if (data !== undefined) {
      headers['Content-Type'] = 'application/json'
      headers['Content-Length'] = Buffer.byteLength(data)
    }
    return request(this.drydockSocket, method, p, headers, data)
  }

  /** The cookie session's value, for a test that sends it itself. */
  async sessionCookie(): Promise<string> {
    if (this.cookie === null) await this.signIn()
    return this.cookie!
  }

  private async signIn(): Promise<void> {
    const data = JSON.stringify({ password: this.password })
    const r = await request(this.drydockSocket, 'POST', '/api/auth/session',
      { Host: UI_HOST, Origin: this.origin, 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) }, data)
    const set = ([] as string[]).concat(r.headers['set-cookie'] ?? [])
    const c = set.map((x) => new RegExp(`^${COOKIE}=([^;]+)`).exec(x)?.[1]).find((x) => x !== undefined)
    if (r.status !== 204 || c === undefined) throw new Error(`stack sign-in: ${r.status} ${r.body}`)
    this.cookie = c
  }

  /** Mints an API token through Settings' own route (the cookie session), as an operator does. */
  async mintToken(scopes: string[], label = 'VS Code (client tier)'): Promise<{ id: string; token: string }> {
    const r = await this.direct('POST', '/api/tokens', { label, scopes, expires_in_days: 1 })
    if (r.status !== 201) throw new Error(`mint: ${r.status} ${r.body}`)
    const b = JSON.parse(r.body) as { id: string; token: string }
    return { id: b.id, token: b.token }
  }

  /** Waits for `ok` on the stack's own view of things. */
  async until<T>(what: string, get: () => Promise<T>, ok: (v: T) => boolean, ms = 30_000): Promise<T> {
    const deadline = Date.now() + ms
    let v = await get()
    while (!ok(v)) {
      if (Date.now() > deadline) throw new Error(`${what}: never (last: ${JSON.stringify(v).slice(0, 400)})`)
      await sleep(50)
      v = await get()
    }
    return v
  }

  async teardown(): Promise<void> {
    await this.caddy?.stop().catch(() => {})
    await this.tap?.close().catch(() => {})
    const p = this.proc
    this.proc = null
    if (p && p.exitCode === null) {
      const gone = new Promise((r) => p.once('exit', r))
      p.kill('SIGTERM')
      await Promise.race([gone, sleep(15_000).then(() => p.kill('SIGKILL'))])
    }
    if (process.env.DRYDOCK_EXTENSION_KEEP) {
      console.error(`kept ${this.root}`)
      return
    }
    rmSync(this.root, { recursive: true, force: true })
    rmSync(this.runDir, { recursive: true, force: true })
  }
}

function request(sock: string, method: string, p: string, headers: Record<string, string | number>, data?: string): Promise<Answer> {
  return new Promise((resolve, reject) => {
    const req = http.request({ socketPath: sock, method, path: p, headers }, (res) => {
      let b = ''
      res.on('data', (c) => (b += c))
      res.on('end', () => resolve({ status: res.statusCode ?? 0, body: b, headers: res.headers }))
    })
    req.on('error', reject)
    req.end(data)
  })
}
