// The browser tier's stack (testing §10.1): the real `drydock serve`, real
// Caddy on the shipped Caddyfile, and Playwright's Chromium trusting a
// throwaway CA through NSS — Spike 04's method, with nothing that disables
// certificate validation anywhere. Firefox and WebKit, which cannot be made
// to trust that CA here, reach the same `drydock serve` through `front()`, a
// plain-HTTP loopback front used by engines.spec.ts alone.
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
//   root/fakebin/docker     a stand-in docker on `drydock serve`'s PATH (below)
//   root/fakeclaude/        fakeclaude and its script, for the login handshake
//
// A second piece is not production either: **`docker` is a stand-in**. This
// tier's subject is the browser, not Docker, and the one Docker-backed thing
// it drives — the Claude login handshake (login.spec.ts) — needs a terminal
// process behind `docker run -it`. The stand-in answers the shared volume's
// and the Claude image's questions, runs fakeclaude (testing §6.4) on
// Drydock's own PTY in place of the login container, and serves the recorded
// credential and `auth status` fixtures once a login has succeeded. It also
// means the tier can never reach the host's Docker, its real credential
// volume above all; `--claude-volume` is the tier's own name besides.
//
// One piece is not production: a **tap** between Caddy and each Drydock socket.
// It forwards bytes unchanged and records what arrived — above all whether the
// session cookie came with a request. That is the only place the cross-site
// assertions can be made: "the browser did not send the cookie" and "the
// request was refused for another reason" look identical from inside the page
// (Spike 04's D1/D2). The tap streams; it does not buffer, so the SSE
// assertion still measures Caddy.

import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, openSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import net from 'node:net'
import { networkInterfaces, tmpdir } from 'node:os'
import path from 'node:path'
import tls from 'node:tls'
import { chromium, type Browser } from '@playwright/test'
import { CaddyProc, type CaddyOptions, type CaddySite } from '../harness/caddy'
import { makeCerts as makeCertsIn } from '../harness/certs'
import { filesUnder, freePort, groupName, sleep, unixAnswers, waitFor } from '../harness/proc'
import { COOKIE, Tap, type Seen } from '../harness/tap'

// The shared pieces (test/harness): Caddy, the certificates, the tap and the
// process helpers, which the VS Code extension's client tier runs too.
export { CaddyProc, COOKIE, filesUnder, Tap, type CaddyOptions, type Seen }

export const UI_HOST = 'drydock.test'
export const PREVIEW_DOMAIN = 'drydock-preview.test'
export const PREVIEW_HOST = `abc123.${PREVIEW_DOMAIN}`
export const OTHER_PREVIEW_HOST = `xyz789.${PREVIEW_DOMAIN}`
/** A slug preview.spec.ts makes previewable (an enabled port on a running workspace). */
export const SLUG = 'myapp-5173-p2mq'
export const SLUG_HOST = `${SLUG}.${PREVIEW_DOMAIN}`
/** The preview session cookie (internal/preview.CookieName). */
export const PREVIEW_COOKIE = '__Host-drydock-preview'
// No port: the origin under test must be the default-port HTTPS origin, or the
// `__Host-` rules and the site comparisons are not the real ones. The resolver
// rules carry the port instead (Spike 04, result 4).
export const UI = `https://${UI_HOST}`
export const PREVIEW = `https://${PREVIEW_HOST}`
/**
 * A document on a preview origin with no handshake in front of it: the denied
 * page, which every preview host serves as itself. A spec that only needs a
 * page on a preview origin loads this; the origin's root now starts the
 * handshake and ends on the UI's sign-in.
 */
export const PREVIEW_PAGE = `${PREVIEW}/.drydock/denied`
/** The session cookie's name between a browser and the loopback front. */
const LOOPBACK_COOKIE = 'drydock-loopback'

export const REPO = path.resolve(__dirname, '..', '..')

/** A throwaway CA and two leaves (UI host, preview wildcard), each with a subjectAltName. */
function makeCerts(dir: string): void {
  makeCertsIn(dir, [
    { name: 'ui', san: `DNS:${UI_HOST}` },
    // One wildcard covers every preview slug; it matches exactly one label.
    { name: 'preview', san: `DNS:*.${PREVIEW_DOMAIN},DNS:${PREVIEW_DOMAIN}` },
  ], 'Drydock browser-tier throwaway CA')
}

/** A HOME whose NSS store trusts `ca` (or nothing, for the control). */
function makeHome(dir: string, ca: string | null): string {
  const nss = path.join(dir, '.pki', 'nssdb')
  mkdirSync(nss, { recursive: true })
  execFileSync('certutil', ['-d', `sql:${nss}`, '-N', '--empty-password'])
  if (ca) execFileSync('certutil', ['-d', `sql:${nss}`, '-A', '-t', 'C,,', '-n', 'drydock-browser-tier-ca', '-i', ca])
  return dir
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

export class Stack implements CaddySite {
  readonly root: string
  readonly certs: string
  readonly sites: string
  readonly run: string
  readonly apiTapSock: string
  readonly previewTapSock: string
  // CaddySite: Caddy forwards to the taps.
  readonly repo = REPO
  readonly uiHost = UI_HOST
  readonly previewDomain = PREVIEW_DOMAIN
  get apiSocket(): string {
    return this.apiTapSock
  }
  get previewSocket(): string {
    return this.previewTapSock
  }
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
      // -tags browsertier: the one difference from a release build. This
      // tier's preview container is this host's own address (see
      // previewContainer), which a release refuses to dial; the tagged build
      // lists no local address and changes nothing else
      // (internal/server/localaddrs_browsertier.go).
      execFileSync('go', ['build', '-tags', 'browsertier', '-o', bin, './cmd/drydock'], { cwd: REPO, stdio: 'inherit' })
    }
    this.bin = bin

    // A signed-in operator needs a password, set the only way there is.
    execFileSync(bin, ['passwd', '--db', this.db], { input: `${this.password}\n`, stdio: ['pipe', 'ignore', 'inherit'] })
    const key = path.join(this.root, 'secrets.key')
    writeFileSync(key, randomBytes(32), { mode: 0o400 })
    chmodSync(key, 0o400)
    this.secretsKey = key

    this.fakeDocker()
    await this.startDrydock()
    this.apiTap = new Tap('api', this.apiTapSock, path.join(this.run, 'http.sock'))
    this.previewTap = new Tap('preview', this.previewTapSock, path.join(this.run, 'preview.sock'))
    this.caddy = await CaddyProc.start(this, 'main')
    this.browser = await this.launch(this.caddy.httpsPort, this.trustedHome)
  }

  bin = ''
  secretsKey = ''
  /**
   * This Drydock's own label namespace, so reconciliation can never adopt or
   * delete a container another Drydock — or a developer — owns (testing §5.4).
   */
  readonly labelPrefix = `test.browser.${randomBytes(4).toString('hex')}`

  /**
   * Makes the stand-in docker report workspace `ws`'s container running at
   * `ip` — or, with null, report none: the container gone behind Drydock's
   * back. What the preview proxy resolves before every dial (PF §8.1).
   */
  previewContainer(ws: string, ip: string | null): void {
    const state = path.join(this.root, 'fakedocker')
    if (ip === null) {
      rmSync(path.join(state, 'preview-ip'), { force: true })
      return
    }
    writeFileSync(path.join(state, 'preview-ws'), ws)
    writeFileSync(path.join(state, 'preview-ip'), ip)
  }
  /** The one login code fakeclaude accepts: a high-entropy canary, `<code>#<state>`. */
  readonly loginCode = `cnryCODE${randomBytes(10).toString('hex')}#cnrySTATE${randomBytes(10).toString('hex')}`
  /** The tier's own shared-volume name; the stand-in docker never creates a real one. */
  readonly claudeVolume = `drydock-browser-claude-${randomBytes(4).toString('hex')}`
  fakeBin = ''

  /**
   * Builds fakeclaude, scripts it to accept `loginCode` alone, and writes
   * the stand-in docker (see the header). The script is the whole of what
   * `drydock serve` can ask Docker here; anything else is logged and
   * answered with nothing.
   */
  private fakeDocker(): void {
    const fc = path.join(this.root, 'fakeclaude')
    mkdirSync(fc, { recursive: true })
    const claude = path.join(fc, 'claude')
    execFileSync('go', ['build', '-o', claude, './internal/claudetest/fakeclaude'], { cwd: REPO, stdio: 'inherit' })
    const sha = createHash('sha256').update(this.loginCode).digest('hex')
    writeFileSync(`${claude}.json`, JSON.stringify({
      corpus: path.join(REPO, 'test', 'fixtures'),
      state_dir: path.join(this.root, 'fakeclaude-state'),
      login: { mode: 'answer', accept_sha256: sha },
    }))
    const state = path.join(this.root, 'fakedocker')
    mkdirSync(state, { recursive: true })
    this.fakeBin = path.join(this.root, 'fakebin')
    mkdirSync(this.fakeBin, { recursive: true })
    const fx = path.join(REPO, 'test', 'fixtures')
    const docker = path.join(this.fakeBin, 'docker')
    writeFileSync(docker, `#!/bin/sh
# The browser tier's stand-in docker (harness.ts). Never a real container.
S='${state}'
printf '%s\\n' "$*" >>"$S/argv.log"
case "$1 $2" in
"volume ls") [ -e "$S/volume" ] && cat "$S/volume"; exit 0 ;;
"volume create") for a; do last=$a; done; echo "$last" >"$S/volume"; echo "$6" | sed 's/=.*//' >"$S/label"; echo "$last"; exit 0 ;;
"volume inspect")
	case " $* " in
	*" --format "*) printf '{"%s":"true"}\\n' "$(cat "$S/label")" ;;
	*) printf '[{"Name":"%s","Driver":"local","Labels":{"%s":"true"}}]\\n' "$(cat "$S/volume")" "$(cat "$S/label")" ;;
	esac
	exit 0 ;;
"image inspect") echo sha256:${'b'.repeat(64)}; exit 0 ;;
esac
# A preview's container (preview.spec.ts, previewContainer()): running, at
# the address the spec wrote, for the one workspace it seeded — the two calls
# the preview proxy makes before and after every dial. Only while the spec
# says it runs; otherwise, as every other container here, there is none.
if [ -e "$S/preview-ip" ]; then
	ws=$(cat "$S/preview-ws")
	ip=$(cat "$S/preview-ip")
	id=${'c'.repeat(64)}
	case " $* " in
	*" ps --quiet --no-trunc --filter status=running --filter label=${this.labelPrefix}.workspace=$ws ") echo "$id"; exit 0 ;;
	" inspect --type container -- $id ")
		printf '[{"Id":"%s","State":{"Running":true},"Config":{"Labels":{"%s":"%s"}},"NetworkSettings":{"Networks":{"bridge":{"NetworkID":"%s","IPAddress":"%s"}}}}]\n' \
			"$id" '${this.labelPrefix}.workspace' "$ws" '${'d'.repeat(64)}' "$ip"
		exit 0 ;;
	" network inspect -- ${'d'.repeat(64)} ")
		printf '[{"Id":"%s","Driver":"bridge"}]\n' '${'d'.repeat(64)}'
		exit 0 ;;
	esac
fi
case "$1" in
ps|rm|stop|inspect) exit 0 ;;
run) ;;
*) exit 0 ;;
esac
case " $* " in
*" --tty "*)
	# The login: fakeclaude on Drydock's own PTY, where the container's
	# claude would be. A success leaves a login for the watch to find.
	'${claude}' auth login --claudeai
	rc=$?
	[ "$rc" = 0 ] && : >"$S/loggedin"
	exit "$rc" ;;
*" DRYDOCK_UID="*) exit 0 ;;
*".credentials.json"*) [ -e "$S/loggedin" ] && exec cat '${fx}/credentials/ok.json'; exit 3 ;;
*" auth status "*) [ -e "$S/loggedin" ] && exec cat '${fx}/authstatus/valid.json'; cat '${fx}/authstatus/absent.json'; exit 1 ;;
esac
exit 0
`, { mode: 0o755 })
  }

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
      '--label-prefix', this.labelPrefix,
      '--secrets-key', this.secretsKey,
      '--claude-volume', this.claudeVolume,
    ], { stdio: ['ignore', out, out], env: { ...process.env, PATH: `${this.fakeBin}:${process.env.PATH ?? ''}` } })
    const sock = path.join(this.run, 'http.sock')
    await waitFor('drydock serve', () => unixAnswers(sock, UI_HOST), log)
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

  private loopback: { url: string; tap: Tap } | null = null

  /**
   * The loopback front: the real Drydock (its embedded UI and its API) over
   * plain HTTP on `http://127.0.0.1:<port>`, for the engines the TLS stack
   * cannot serve — Playwright's Firefox and WebKit have no per-run trust store
   * this tier can put its CA in, and certificate errors stay unignored
   * (testing §10.1). 127.0.0.1 is a potentially trustworthy origin, so the
   * `__Host-` cookie's `Secure` holds there too.
   *
   * It is a tap that records what the browser sent and then edits what it
   * forwards: `Host` becomes the UI host, and an `Origin` that is *exactly*
   * this front's origin becomes the UI origin. Anything else — `null`, a
   * wrong port, none at all — is forwarded as it arrived, so the server's
   * exact match still decides. What the browser sent is in `seen`, and that
   * is what the engine specs assert on.
   *
   * The session cookie is renamed on the way out and back, and loses
   * `Secure`: WebKit keeps no `Secure` cookie from an `http:` page, even on
   * loopback, and a `__Host-` cookie cannot exist without it. The cookie's
   * own rules are the TLS stack's subject (cookies.spec.ts), not this one's.
   */
  async front(): Promise<{ url: string; tap: Tap }> {
    if (this.loopback) return this.loopback
    let origin = ''
    const tap = new Tap('api', { host: '127.0.0.1', port: 0 }, path.join(this.run, 'http.sock'), {
      request: (h) => {
        h.host = UI_HOST
        if (h.origin === origin) h.origin = UI
        if (h.cookie) h.cookie = h.cookie.replaceAll(`${LOOPBACK_COOKIE}=`, `${COOKIE}=`)
      },
      response: (h) => {
        const sc = h['set-cookie']
        if (sc) h['set-cookie'] = sc.map((c) => c.replace(`${COOKIE}=`, `${LOOPBACK_COOKIE}=`).replace(/;\s*Secure/i, ''))
      },
    })
    await tap.listening
    origin = `http://127.0.0.1:${tap.port()}`
    this.loopback = { url: origin, tap }
    return this.loopback
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
    await this.loopback?.tap.close().catch(() => {})
    await this.stopDrydock().catch(() => {})
    if (process.env.DRYDOCK_BROWSER_KEEP) {
      console.error(`kept ${this.root}`)
      return
    }
    rmSync(this.root, { recursive: true, force: true })
  }
}

/**
 * This host's first non-loopback IPv4 address: what the stand-in docker
 * reports as a preview container's address, on a bridge network. A release
 * refuses every address the host holds — dialling one would reach the host —
 * which is why the tier builds with -tags browsertier (see boot()); loopback,
 * gateways and non-bridge networks are still refused, so the dev server
 * listens on every interface and is reached at this one.
 */
export function hostAddress(): string {
  for (const list of Object.values(networkInterfaces())) {
    for (const a of list ?? []) {
      if (a.family === 'IPv4' && !a.internal) return a.address
    }
  }
  throw new Error('no non-loopback IPv4 address on this host: the preview tests need one')
}

/** A real Vite dev server, HMR and all, on every interface. */
export class DevServer {
  proc: ChildProcess | null = null
  constructor(
    readonly root: string,
    readonly port: number,
  ) {}

  /** Starts Vite from web/'s own install on `root`, and waits for it. */
  static async start(root: string, files: Record<string, string>): Promise<DevServer> {
    mkdirSync(root, { recursive: true })
    for (const [name, text] of Object.entries(files)) writeFileSync(path.join(root, name), text)
    const d = new DevServer(root, await freePort())
    await d.restart()
    return d
  }

  /** (Re)starts it on the same root and port. */
  async restart(): Promise<void> {
    const log = path.join(this.root, 'vite.log')
    this.proc = spawn(process.execPath, [
      path.join(REPO, 'web', 'node_modules', 'vite', 'bin', 'vite.js'),
      '--host', '0.0.0.0', '--port', String(this.port), '--strictPort',
    ], { cwd: this.root, stdio: ['ignore', openSync(log, 'a'), openSync(log, 'a')], env: { ...process.env, NODE_PATH: '' } })
    const port = this.port
    await waitFor('vite', () => tcpAnswers(port), log)
  }

  async stop(): Promise<void> {
    const p = this.proc
    this.proc = null
    if (!p || p.exitCode !== null) return
    const gone = new Promise((r) => p.once('exit', r))
    p.kill('SIGTERM')
    await Promise.race([gone, sleep(5000).then(() => p.kill('SIGKILL'))])
  }
}

function tcpAnswers(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const s = net.connect(port, '127.0.0.1', () => {
      s.end()
      resolve(true)
    })
    s.on('error', () => resolve(false))
  })
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
