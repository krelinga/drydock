// Small process and socket helpers shared by the browser tier and the
// extension's client tier (vscode-extension §12.4). Node built-ins only, so
// either toolchain can import this by path.

import { execFileSync } from 'node:child_process'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import http from 'node:http'
import net from 'node:net'
import { userInfo } from 'node:os'
import path from 'node:path'
import tls from 'node:tls'

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

export async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = net.createServer()
    s.listen(0, '127.0.0.1', () => {
      const a = s.address()
      s.close(() => (typeof a === 'object' && a ? resolve(a.port) : reject(new Error('no port'))))
    })
  })
}

/** Thrown by a waitFor check to stop waiting at once: what it waits for can no longer come. */
export class GaveUp extends Error {}

/**
 * Polls `ok` every 100 ms for `tries` tries; throws naming `what`, with
 * `log`'s contents when given. A check that throws is "not yet", except a
 * GaveUp, which ends the wait.
 */
export async function waitFor(what: string, ok: () => boolean | Promise<boolean>, log?: string, tries = 150): Promise<void> {
  for (let i = 0; i < tries; i++) {
    try {
      if (await ok()) return
    } catch (e) {
      if (e instanceof GaveUp) throw new Error(`${what}: ${e.message}${log ? `\n${safeRead(log)}` : ''}`)
      /* not yet */
    }
    await sleep(100)
  }
  const tail = log ? `\n${safeRead(log)}` : ''
  throw new Error(`${what} never came up${tail}`)
}

export function safeRead(p: string): string {
  try {
    return readFileSync(p, 'utf8')
  } catch {
    return ''
  }
}

/**
 * Every regular file under dir, recursively: what a sweep of a stack's root
 * reads — Caddy's log, Drydock's log, the database and its WAL, the fakes'
 * argv logs.
 */
export function filesUnder(dir: string): string[] {
  const out: string[] = []
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name)
    const st = statSync(p, { throwIfNoEntry: false })
    if (st?.isDirectory()) out.push(...filesUnder(p))
    else if (st?.isFile()) out.push(p)
  }
  return out
}

/** Whether something answers HTTP on a Unix socket. */
export function unixAnswers(sock: string, host: string): Promise<boolean> {
  return new Promise((resolve) => {
    const req = http.request({ socketPath: sock, path: '/', headers: { Host: host } }, (res) => {
      res.resume()
      resolve(true)
    })
    req.on('error', () => resolve(false))
    req.end()
  })
}

/** Whether something speaks TLS on the port yet. */
export function tlsAnswers(port: number, servername: string): Promise<boolean> {
  return new Promise((resolve) => {
    // rejectUnauthorized is off for this liveness probe ONLY: it asks "is
    // something speaking TLS on the port yet", not "is it the right cert".
    // What the assertions are about — the browser, the extension's client —
    // validates fully.
    const s = tls.connect({ host: '127.0.0.1', port, servername, rejectUnauthorized: false }, () => {
      s.end()
      resolve(true)
    })
    s.on('error', () => resolve(false))
  })
}

export function groupName(): string {
  // The process's primary group: Caddy runs as the same user, so it can
  // reach a 0660 socket owned by it — the test's stand-in for the drydock group.
  return execFileSync('id', ['-gn'], { encoding: 'utf8' }).trim() || String(userInfo().gid)
}
