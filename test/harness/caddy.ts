// Real Caddy on the shipped Caddyfile (deploy/Caddyfile, byte for byte: a
// tested copy of a config is not a tested config), shared by the browser
// tier and the extension's client tier. Its admin API is on a 0600 Unix
// socket in its own directory, never localhost:2019.

import { spawn, type ChildProcess } from 'node:child_process'
import { mkdirSync, openSync } from 'node:fs'
import path from 'node:path'
import { freePort, sleep, tlsAnswers, waitFor } from './proc'

/** What a Caddy needs to know about the stack it fronts. */
export interface CaddySite {
  /** The run's temp root: the Caddy's directory is made under it. */
  readonly root: string
  /** The repository checkout (for deploy/Caddyfile). */
  readonly repo: string
  readonly uiHost: string
  readonly previewDomain: string
  /** A directory holding ui.crt, ui.key, preview.crt and preview.key. */
  readonly certs: string
  /** The sockets Caddy forwards to (a tap's, or Drydock's own). */
  readonly apiSocket: string
  readonly previewSocket: string
  /** The sites directory the Caddyfile imports. */
  readonly sites: string
}

export interface CaddyOptions {
  /** Serve the UI host this certificate instead of its own (the wrong-host tests). */
  uiCert?: { cert: string; key: string }
  /** The HTTPS port, when the caller must know it first (an origin with a port); otherwise a free one. */
  httpsPort?: number
}

export class CaddyProc {
  proc: ChildProcess | null = null
  readonly httpsPort: number
  readonly httpPort: number
  readonly dir: string
  private env: NodeJS.ProcessEnv
  private readonly repo: string
  private readonly uiHost: string

  private constructor(site: CaddySite, tag: string, httpsPort: number, httpPort: number, opts: CaddyOptions) {
    this.httpsPort = httpsPort
    this.httpPort = httpPort
    this.repo = site.repo
    this.uiHost = site.uiHost
    this.dir = path.join(site.root, `caddy-${tag}`)
    mkdirSync(this.dir, { recursive: true, mode: 0o700 })
    const c = site.certs
    this.env = {
      ...process.env,
      HOME: this.dir,
      XDG_DATA_HOME: path.join(this.dir, 'data'),
      XDG_CONFIG_HOME: path.join(this.dir, 'config'),
      DRYDOCK_UI_HOST: site.uiHost,
      DRYDOCK_UI_CERT: opts.uiCert?.cert ?? path.join(c, 'ui.crt'),
      DRYDOCK_UI_KEY: opts.uiCert?.key ?? path.join(c, 'ui.key'),
      DRYDOCK_PREVIEW_DOMAIN: site.previewDomain,
      DRYDOCK_PREVIEW_CERT: path.join(c, 'preview.crt'),
      DRYDOCK_PREVIEW_KEY: path.join(c, 'preview.key'),
      DRYDOCK_API_SOCKET: site.apiSocket,
      DRYDOCK_PREVIEW_SOCKET: site.previewSocket,
      DRYDOCK_HTTPS_PORT: String(httpsPort),
      DRYDOCK_HTTP_PORT: String(httpPort),
      DRYDOCK_CADDY_ADMIN: `unix/${path.join(this.dir, 'admin.sock')}|0600`,
      DRYDOCK_CADDY_SITES: site.sites,
    }
  }

  static async start(site: CaddySite, tag: string, opts: CaddyOptions = {}): Promise<CaddyProc> {
    const c = new CaddyProc(site, tag, opts.httpsPort ?? (await freePort()), await freePort(), opts)
    await c.run()
    return c
  }

  /** (Re)starts on the same ports. */
  async run(): Promise<void> {
    const log = path.join(this.dir, 'caddy.log')
    this.proc = spawn('caddy', ['run', '--config', path.join(this.repo, 'deploy', 'Caddyfile'), '--adapter', 'caddyfile'], {
      env: this.env,
      // Caddy logs to stderr; kept in the root for a failed run's post-mortem.
      stdio: ['ignore', 'ignore', openSync(log, 'a')],
    })
    const port = this.httpsPort
    await waitFor(`caddy (${path.basename(this.dir)})`, () => tlsAnswers(port, this.uiHost), log)
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
