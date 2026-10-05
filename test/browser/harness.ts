// The browser tier's stack (testing §10.1): the real `drydock serve`, real
// Caddy on the shipped Caddyfile, and Playwright's Chromium trusting a
// throwaway CA through NSS — Spike 04's method, with nothing that disables
// certificate validation anywhere.
//
// Everything mutable lives under one temp root, removed at teardown:
//
//   root/bin/drydock        built from this checkout
//   root/certs/             the CA and two leaves (UI host, preview wildcard)
//   root/home/.pki/nssdb    the NSS store the CA is trusted in — the browser
//                           runs with HOME=root/home, so the operator's own
//                           ~/.pki/nssdb is never touched and there is nothing
//                           to remove from it afterwards (Spike 04 rule E)
//   root/run/               Drydock's two sockets and the taps in front of them
//   root/caddy-*/           each Caddy's data, config, admin socket and log
//   root/drydock.db         the store; root/secrets.key the master key
//
// One piece is not production: a **tap** between Caddy and each Drydock socket.
// It forwards bytes unchanged and records what arrived — above all whether the
// session cookie came with a request. That is the only place the cross-site
// assertions can be made: "the browser did not send the cookie" and "the
// request was refused for another reason" look identical from inside the page
// (Spike 04's D1/D2). The tap streams; it does not buffer, so the SSE
// assertion still measures Caddy.

import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import net from 'node:net'
import { tmpdir, userInfo } from 'node:os'
import path from 'node:path'
import tls from 'node:tls'
import { chromium, type Browser } from '@playwright/test'

export const UI_HOST = 'drydock.test'
export const PREVIEW_DOMAIN = 'drydock-preview.test'
export const PREVIEW_HOST = `abc123.${PREVIEW_DOMAIN}`
export const OTHER_PREVIEW_HOST = `xyz789.${PREVIEW_DOMAIN}`
// No port: the origin under test must be the default-port HTTPS origin, or the
// `__Host-` rules and the site comparisons are not the real ones. The resolver
// rules carry the port instead (Spike 04, result 4).
export const UI = `https://${UI_HOST}`
export const PREVIEW = `https://${PREVIEW_HOST}`
export const COOKIE = '__Host-drydock'

export const REPO = path.resolve(__dirname, '..', '..')

/** What the tap saw for one request. */
export interface Seen {
  seq: number
  socket: 'api' | 'preview'
  method: string
  path: string
  host: string
  origin: string | null
  /** The value of the session cookie, if one arrived. */
  session: string | null
  cookieNames: string[]
  secFetchSite: string | null
  secFetchDest: string | null
  secFetchMode: string | null
  lastEventId: string | null
  acceptEncoding: string | null
  status: number | null
  responseHeaders: http.IncomingHttpHeaders | null
}

class Tap {
  readonly seen: Seen[] = []
  private seq = 0
  private server: http.Server

  constructor(readonly socket: 'api' | 'preview', listen: string, upstream: string) {
    this.server = http.createServer((req, res) => {
      const cookies = parseCookies(req.headers.cookie)
      const rec: Seen = {
        seq: ++this.seq,
        socket,
        method: req.method ?? '',
        path: req.url ?? '',
        host: req.headers.host ?? '',
        origin: header(req, 'origin'),
        session: cookies.get(COOKIE) ?? null,
        cookieNames: [...cookies.keys()],
        secFetchSite: header(req, 'sec-fetch-site'),
        secFetchDest: header(req, 'sec-fetch-dest'),
        secFetchMode: header(req, 'sec-fetch-mode'),
        lastEventId: header(req, 'last-event-id'),
        acceptEncoding: header(req, 'accept-encoding'),
        status: null,
        responseHeaders: null,
      }
      this.seen.push(rec)
      const up = http.request(
        { socketPath: upstream, method: req.method, path: req.url, headers: req.headers },
        (ur) => {
          rec.status = ur.statusCode ?? null
          rec.responseHeaders = ur.headers
          res.writeHead(ur.statusCode ?? 502, ur.headers)
          // Streams each chunk as it arrives: SSE frames are not held here.
          ur.on('data', (c: Buffer) => res.write(c))
          ur.on('end', () => res.end())
          ur.on('error', () => res.destroy())
        },
      )
      up.on('error', () => {
        if (!res.headersSent) res.writeHead(502)
        res.end()
      })
      res.on('close', () => up.destroy())
      req.pipe(up)
    })
    this.server.listen(listen)
  }

  clear(): void {
    this.seen.length = 0
  }

  close(): Promise<void> {
    this.server.closeAllConnections()
    return new Promise((r) => this.server.close(() => r()))
  }
}

function header(req: http.IncomingMessage, name: string): string | null {
  const v = req.headers[name]
  return typeof v === 'string' ? v : Array.isArray(v) ? v.join(', ') : null
}

function parseCookies(raw: string | undefined): Map<string, string> {
  const out = new Map<string, string>()
  for (const part of (raw ?? '').split(';')) {
    const i = part.indexOf('=')
    if (i > 0) out.set(part.slice(0, i).trim(), part.slice(i + 1).trim())
  }
  return out
}

async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = net.createServer()
    s.listen(0, '127.0.0.1', () => {
      const a = s.address()
      s.close(() => (typeof a === 'object' && a ? resolve(a.port) : reject(new Error('no port'))))
    })
  })
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

async function waitFor(what: string, ok: () => boolean | Promise<boolean>, log?: string): Promise<void> {
  for (let i = 0; i < 150; i++) {
    try {
      if (await ok()) return
    } catch {
      /* not yet */
    }
    await sleep(100)
  }
  const tail = log ? `\n${safeRead(log)}` : ''
  throw new Error(`${what} never came up${tail}`)
}

function safeRead(p: string): string {
  try {
    return readFileSync(p, 'utf8')
  } catch {
    return ''
  }
}

/** A throwaway CA and two leaves, each with a subjectAltName (Chromium ignores CN). */
function makeCerts(dir: string): void {
  mkdirSync(dir, { recursive: true })
  const ossl = (...args: string[]) => execFileSync('openssl', args, { cwd: dir, stdio: ['ignore', 'ignore', 'pipe'] })
  ossl('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-sha256', '-days', '2',
    '-keyout', 'ca.key', '-out', 'ca.crt', '-subj', '/CN=Drydock browser-tier throwaway CA',
    '-addext', 'basicConstraints=critical,CA:TRUE,pathlen:0',
    '-addext', 'keyUsage=critical,keyCertSign,cRLSign')
  const leaf = (name: string, san: string) => {
    writeFileSync(path.join(dir, `${name}.ext`), [
      `subjectAltName=${san}`,
      'basicConstraints=critical,CA:FALSE',
      'keyUsage=critical,digitalSignature,keyEncipherment',
      'extendedKeyUsage=serverAuth',
    ].join('\n'))
    ossl('req', '-newkey', 'rsa:2048', '-nodes', '-sha256', '-keyout', `${name}.key`, '-out', `${name}.csr`, '-subj', `/CN=${name}`)
    ossl('x509', '-req', '-in', `${name}.csr`, '-CA', 'ca.crt', '-CAkey', 'ca.key', '-CAcreateserial',
      '-out', `${name}.crt`, '-days', '2', '-sha256', '-extfile', `${name}.ext`)
  }
  leaf('ui', `DNS:${UI_HOST}`)
  // One wildcard covers every preview slug; it matches exactly one label.
  leaf('preview', `DNS:*.${PREVIEW_DOMAIN},DNS:${PREVIEW_DOMAIN}`)
}

/** A HOME whose NSS store trusts `ca` (or nothing, for the control). */
function makeHome(dir: string, ca: string | null): string {
  const nss = path.join(dir, '.pki', 'nssdb')
  mkdirSync(nss, { recursive: true })
  execFileSync('certutil', ['-d', `sql:${nss}`, '-N', '--empty-password'])
  if (ca) execFileSync('certutil', ['-d', `sql:${nss}`, '-A', '-t', 'C,,', '-n', 'drydock-browser-tier-ca', '-i', ca])
  return dir
}

export interface CaddyOptions {
  /** Serve the UI host this certificate instead of its own (the wrong-host test). */
  uiCert?: { cert: string; key: string }
}

export class CaddyProc {
  proc: ChildProcess | null = null
  readonly httpsPort: number
  readonly dir: string
  private env: NodeJS.ProcessEnv

  private constructor(stack: Stack, tag: string, httpsPort: number, httpPort: number, opts: CaddyOptions) {
    this.httpsPort = httpsPort
    this.dir = path.join(stack.root, `caddy-${tag}`)
    mkdirSync(this.dir, { recursive: true, mode: 0o700 })
    const c = stack.certs
    this.env = {
      ...process.env,
      HOME: this.dir,
      XDG_DATA_HOME: path.join(this.dir, 'data'),
      XDG_CONFIG_HOME: path.join(this.dir, 'config'),
      DRYDOCK_UI_HOST: UI_HOST,
      DRYDOCK_UI_CERT: opts.uiCert?.cert ?? path.join(c, 'ui.crt'),
      DRYDOCK_UI_KEY: opts.uiCert?.key ?? path.join(c, 'ui.key'),
      DRYDOCK_PREVIEW_DOMAIN: PREVIEW_DOMAIN,
      DRYDOCK_PREVIEW_CERT: path.join(c, 'preview.crt'),
      DRYDOCK_PREVIEW_KEY: path.join(c, 'preview.key'),
      DRYDOCK_API_SOCKET: stack.apiTapSock,
      DRYDOCK_PREVIEW_SOCKET: stack.previewTapSock,
      DRYDOCK_HTTPS_PORT: String(httpsPort),
      DRYDOCK_HTTP_PORT: String(httpPort),
      DRYDOCK_CADDY_ADMIN: `unix/${path.join(this.dir, 'admin.sock')}|0600`,
      DRYDOCK_CADDY_SITES: stack.sites,
    }
  }

  static async start(stack: Stack, tag: string, opts: CaddyOptions = {}): Promise<CaddyProc> {
    const c = new CaddyProc(stack, tag, await freePort(), await freePort(), opts)
    await c.run()
    return c
  }

  /** (Re)starts on the same ports. */
  async run(): Promise<void> {
    const log = path.join(this.dir, 'caddy.log')
    // The shipped file, byte for byte: a tested copy of a config is not a tested config.
    this.proc = spawn('caddy', ['run', '--config', path.join(REPO, 'deploy', 'Caddyfile'), '--adapter', 'caddyfile'], {
      env: this.env,
      // Caddy logs to stderr; kept in the root for a failed run's post-mortem.
      stdio: ['ignore', 'ignore', openSync(log, 'a')],
    })
    const port = this.httpsPort
    await waitFor(`caddy (${path.basename(this.dir)})`, () => tlsAnswers(port), log)
  }

  /** SIGKILL: every connection through it drops at once, as a crashed proxy's would. */
  async kill(): Promise<void> {
    const p = this.proc
    this.proc = null
    if (!p || p.exitCode !== null) return
    const gone = new Promise((r) => p.once('exit', r))
    p.kill('SIGKILL')
    await gone
  }

  async stop(): Promise<void> {
    const p = this.proc
    this.proc = null
    if (!p || p.exitCode !== null) return
    const gone = new Promise((r) => p.once('exit', r))
    p.kill('SIGINT')
    await Promise.race([gone, sleep(5000).then(() => p.kill('SIGKILL'))])
  }
}

function tlsAnswers(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    // rejectUnauthorized is off for this liveness probe ONLY: it asks "is
    // something speaking TLS on the port yet", not "is it the right cert".
    // The browser, which is what the assertions are about, validates fully.
    const s = tls.connect({ host: '127.0.0.1', port, servername: UI_HOST, rejectUnauthorized: false }, () => {
      s.end()
      resolve(true)
    })
    s.on('error', () => resolve(false))
  })
}

/** Reads the certificate a listener presents for `servername`. */
export function presentedCert(port: number, servername: string): Promise<tls.PeerCertificate> {
  return new Promise((resolve, reject) => {
    const s = tls.connect({ host: '127.0.0.1', port, servername, rejectUnauthorized: false }, () => {
      const c = s.getPeerCertificate()
      s.end()
      resolve(c)
    })
    s.on('error', reject)
  })
}

export class Stack {
  readonly root: string
  readonly certs: string
  readonly sites: string
  readonly run: string
  readonly apiTapSock: string
  readonly previewTapSock: string
  readonly db: string
  readonly password = `pw-${randomBytes(12).toString('hex')}`
  apiTap!: Tap
  previewTap!: Tap
  drydock: ChildProcess | null = null
  caddy!: CaddyProc
  browser!: Browser
  /** A HOME that trusts the CA through NSS. */
  trustedHome!: string

  constructor() {
    this.root = mkdtempSync(path.join(tmpdir(), 'drydock-browser-'))
    this.certs = path.join(this.root, 'certs')
    this.sites = path.join(this.root, 'sites')
    this.run = path.join(this.root, 'run')
    this.apiTapSock = path.join(this.run, 'api-tap.sock')
    this.previewTapSock = path.join(this.run, 'preview-tap.sock')
    this.db = path.join(this.root, 'drydock.db')
  }

  static async start(): Promise<Stack> {
    const s = new Stack()
    try {
      await s.boot()
    } catch (e) {
      await s.teardown()
      throw e
    }
    return s
  }

  private async boot(): Promise<void> {
    mkdirSync(this.run, { recursive: true, mode: 0o750 })
    mkdirSync(this.sites, { recursive: true })
    // The optional preview site, installed the way an operator installs it.
    copyFileSync(path.join(REPO, 'deploy', 'preview.caddy'), path.join(this.sites, 'preview.caddy'))
    makeCerts(this.certs)
    this.trustedHome = makeHome(path.join(this.root, 'home'), path.join(this.certs, 'ca.crt'))

    const bin = process.env.DRYDOCK_BIN || path.join(this.root, 'bin', 'drydock')
    if (!process.env.DRYDOCK_BIN) {
      execFileSync('go', ['build', '-o', bin, './cmd/drydock'], { cwd: REPO, stdio: 'inherit' })
    }
    this.bin = bin

    // A signed-in operator needs a password, set the only way there is.
    execFileSync(bin, ['passwd', '--db', this.db], { input: `${this.password}\n`, stdio: ['pipe', 'ignore', 'inherit'] })
    const key = path.join(this.root, 'secrets.key')
    writeFileSync(key, randomBytes(32), { mode: 0o400 })
    chmodSync(key, 0o400)
    this.secretsKey = key

    await this.startDrydock()
    this.apiTap = new Tap('api', this.apiTapSock, path.join(this.run, 'http.sock'))
    this.previewTap = new Tap('preview', this.previewTapSock, path.join(this.run, 'preview.sock'))
    this.caddy = await CaddyProc.start(this, 'main')
    this.browser = await this.launch(this.caddy.httpsPort, this.trustedHome)
  }

  bin = ''
  secretsKey = ''

  async startDrydock(): Promise<void> {
    const log = path.join(this.root, 'drydock.log')
    const out = openSync(log, 'a')
    this.drydock = spawn(this.bin, [
      'serve',
      '--db', this.db,
      '--ui-origin', UI,
      '--ui-host', UI_HOST,
      '--preview-domain', PREVIEW_DOMAIN,
      '--api-socket', path.join(this.run, 'http.sock'),
      '--preview-socket', path.join(this.run, 'preview.sock'),
      '--socket-group', groupName(),
      '--workspace-root', path.join(this.root, 'ws'),
      '--broker-dir', path.join(this.run, 'broker'),
      // Its own label namespace, so reconciliation can never adopt or delete
      // a container another Drydock — or a developer — owns (testing §5.4).
      '--label-prefix', `test.browser.${randomBytes(4).toString('hex')}`,
      '--secrets-key', this.secretsKey,
    ], { stdio: ['ignore', out, out] })
    const sock = path.join(this.run, 'http.sock')
    await waitFor('drydock serve', () => unixAnswers(sock), log)
  }

  async stopDrydock(): Promise<void> {
    const p = this.drydock
    this.drydock = null
    if (!p || p.exitCode !== null) return
    const gone = new Promise((r) => p.once('exit', r))
    p.kill('SIGTERM')
    await Promise.race([gone, sleep(15000).then(() => p.kill('SIGKILL'))])
  }

  /** Chromium, resolving both test domains to `port` — with validation on. */
  async launch(port: number, home: string): Promise<Browser> {
    return chromium.launch({
      args: [
        // Wildcard resolution with a port, which /etc/hosts cannot express.
        `--host-resolver-rules=MAP ${UI_HOST} 127.0.0.1:${port},MAP *.${PREVIEW_DOMAIN} 127.0.0.1:${port}`,
      ],
      // HOME decides which NSS store Chromium reads: ~/.pki/nssdb.
      env: { ...process.env, HOME: home },
    })
  }

  /** A HOME with an empty NSS store: the control that proves trust is doing the work. */
  untrustedHome(): string {
    return makeHome(path.join(this.root, `home-untrusted-${randomBytes(3).toString('hex')}`), null)
  }

  /**
   * A request straight to Drydock's API socket, bypassing Caddy and the
   * browser — the test's own hand, for setting up state while the browser's
   * path is deliberately down.
   */
  direct(method: string, p: string, session: string, body?: unknown): Promise<{ status: number; body: string }> {
    return new Promise((resolve, reject) => {
      const data = body === undefined ? undefined : JSON.stringify(body)
      const req = http.request({
        socketPath: path.join(this.run, 'http.sock'),
        method,
        path: p,
        headers: {
          Host: UI_HOST,
          Origin: UI,
          Cookie: `${COOKIE}=${session}`,
          ...(data ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } : {}),
        },
      }, (res) => {
        let b = ''
        res.on('data', (c) => (b += c))
        res.on('end', () => resolve({ status: res.statusCode ?? 0, body: b }))
      })
      req.on('error', reject)
      req.end(data)
    })
  }

  async teardown(): Promise<void> {
    await this.browser?.close().catch(() => {})
    await this.caddy?.stop().catch(() => {})
    await this.apiTap?.close().catch(() => {})
    await this.previewTap?.close().catch(() => {})
    await this.stopDrydock().catch(() => {})
    if (process.env.DRYDOCK_BROWSER_KEEP) {
      console.error(`kept ${this.root}`)
      return
    }
    rmSync(this.root, { recursive: true, force: true })
  }
}

function unixAnswers(sock: string): Promise<boolean> {
  return new Promise((resolve) => {
    const req = http.request({ socketPath: sock, path: '/', headers: { Host: UI_HOST } }, (res) => {
      res.resume()
      resolve(true)
    })
    req.on('error', () => resolve(false))
    req.end()
  })
}

function groupName(): string {
  // The process's primary group: Caddy runs as the same user, so it can
  // reach a 0660 socket owned by it — the test's stand-in for the drydock group.
  return execFileSync('id', ['-gn'], { encoding: 'utf8' }).trim() || String(userInfo().gid)
}

/**
 * Frontend §8's policy, as internal/web.CSP spells it. Duplicated on purpose:
 * this tier asserts what reached the browser, and a test that imported the
 * value it checks would pass whatever the value was.
 */
export const CSP_EXPECTED =
  "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
  "connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; " +
  "form-action 'self'; frame-src 'none'; frame-ancestors 'none'"
