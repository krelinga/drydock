// The small pure rules, each over its whole table (vscode-extension §12.2):
// the window rule, drydock.url, the token's shape, the stored credential,
// the api range, the update URL, link → URI, redaction and the route match.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { checkPaste, parseStored } from './credential'
import { INSTALL_MATCHING, parseMeta, skew, vsixURL } from './handshake'
import { remoteURI } from './link'
import { redact } from './log'
import { pathFor, routeOf } from './routes'
import { originOf } from './settings'
import { mayMutate } from './windowRule'
import { TOKEN } from '../test/fakes'

describe('mayMutate (§8 rule 1)', () => {
  it('true for a local window and ssh-remote only; every other kind, an invented one included, fails closed', () => {
    expect(mayMutate(undefined)).toBe(true)
    expect(mayMutate('ssh-remote')).toBe(true)
    for (const n of ['attached-container', 'dev-container', 'tunnel', 'codespaces', 'wsl', 'k8s-container', 'ssh-remote-2', '', 'SSH-REMOTE']) {
      expect(mayMutate(n), n).toBe(false)
    }
  })
})

describe('drydock.url', () => {
  it('normalises to the origin URL.origin would write', () => {
    expect(originOf('https://drydock.example.com')).toEqual({ ok: true, origin: 'https://drydock.example.com' })
    expect(originOf(' https://drydock.example.com/ ')).toEqual({ ok: true, origin: 'https://drydock.example.com' })
    expect(originOf('https://drydock.test:8443')).toEqual({ ok: true, origin: 'https://drydock.test:8443' })
    expect(originOf('https://drydock.test:443')).toEqual({ ok: true, origin: 'https://drydock.test' })
  })

  it('refuses http, a capital, a path, a query, a user, a bad port and nothing', () => {
    for (const bad of ['http://drydock.test', 'https://Drydock.test', 'https://drydock.test/ws', 'https://drydock.test?x',
      'https://u@drydock.test', 'https://drydock.test:0', 'https://drydock.test:99999', 'https://drydock.test:0443',
      'drydock.test', 'https://', '', 'https://-x.test', 'HTTPS://drydock.test']) {
      expect(originOf(bad).ok, bad).toBe(false)
    }
    const cap = originOf('https://Drydock.test')
    expect(cap.ok === false && cap.sentence).toContain('lowercase')
  })
})

describe('the token', () => {
  it("a minted token's shape passes, trimmed of surrounding whitespace and nothing else", () => {
    expect(checkPaste(TOKEN)).toEqual({ ok: true, token: TOKEN })
    expect(checkPaste(`  ${TOKEN}\n`)).toEqual({ ok: true, token: TOKEN })
  })

  it('anything else is refused, and the sentence never repeats what was pasted', () => {
    for (const bad of ['', TOKEN.slice(0, -1), TOKEN + 'x', TOKEN.replace('drydock_', 'drydocx_'), `${TOKEN.slice(0, 20)} ${TOKEN.slice(20)}`,
      TOKEN.replace('A', '+'), 'ghs_' + 'a'.repeat(36)]) {
      const r = checkPaste(bad)
      expect(r.ok, bad).toBe(false)
      if (!r.ok && bad.length > 4) expect(r.sentence).not.toContain(bad.trim().slice(4, 20))
    }
  })

  it('the stored entry is read only when it is exactly what the extension writes', () => {
    expect(parseStored(JSON.stringify({ origin: 'https://d.test', token: TOKEN }))).toEqual({ origin: 'https://d.test', token: TOKEN })
    for (const bad of [undefined, '', 'x', 'null', '{}', JSON.stringify({ origin: 'https://d.test', token: 'nope' }),
      JSON.stringify({ origin: 1, token: TOKEN })]) {
      expect(parseStored(bad)).toBeNull()
    }
  })
})

describe('the handshake', () => {
  it('the api range decides over every row of §9.1', () => {
    expect(skew(1, [1, 1])).toEqual({ kind: 'ok' })
    expect(skew(2, [1, 3]).kind).toBe('ok')
    expect(skew(0, [1, 1]).kind).toBe('server_older')
    expect(skew(1.5, [1, 2]).kind).toBe('server_older')
    expect(skew(2, [1, 1])).toMatchObject({ kind: 'server_newer', action: INSTALL_MATCHING })
  })

  it('a version builds a download URL only when it is vX.Y.Z', () => {
    expect(vsixURL('v0.12.0')).toBe('https://github.com/krelinga/drydock/releases/download/v0.12.0/drydock-vscode.vsix')
    for (const v of ['dev', 'v0.12', 'v0.12.0-rc1', '0.12.0', 'v0.12.0/../../x', 'v1.2.3\n', 12, null]) {
      expect(vsixURL(v), String(v)).toBeNull()
    }
  })

  it("parses the server's meta answer and nothing else", () => {
    const good = { version: 'v0.12.1', api: 1, credential: { kind: 'token', label: 'VS Code', scopes: ['read'], expires_at: '2027-01-01T00:00:00Z' } }
    expect(parseMeta(good)).toEqual(good)
    expect(parseMeta({ ...good, api: '1' })).toBeNull()
    expect(parseMeta({ ...good, credential: { ...good.credential, scopes: [1] } })).toBeNull()
    expect(parseMeta(null)).toBeNull()
  })
})

describe('link → URI', () => {
  // The golden links internal/vscode pins against Dev Containers, read from
  // the Go test's own table, so the two cannot drift.
  const go = readFileSync(resolve(process.cwd(), '../internal/vscode/vscode_test.go'), 'utf8')
  const golden = [...go.matchAll(/"(vscode:\/\/vscode-remote\/[^"]+)"/g)].map((m) => m[1]!)

  it('rewrites only the scheme and authority prefix of every golden link', () => {
    expect(golden.length).toBeGreaterThanOrEqual(3)
    for (const u of golden) {
      const r = remoteURI(u)
      expect(r, u).toBe('vscode-remote://' + u.slice('vscode://vscode-remote/'.length))
    }
  })

  it('refuses every shape the web UI refuses', () => {
    const good = golden[0]!
    for (const bad of [good.replace('vscode://', 'http://'), good.replace('attached-container+', 'dev-container+'),
      good + '?x=1', good + '#f', good.replace('@ssh-remote+devbox', '@ssh-remote+evil.test@x'),
      'vscode://vscode-remote/attached-container+ZZ@ssh-remote+devbox/w', 'vscode-remote://x', '', 7, null]) {
      expect(remoteURI(bad), String(bad)).toBeNull()
    }
  })
})

describe('redact', () => {
  it('masks a token, a bearer value and a GitHub token; leaves ordinary text (control)', () => {
    expect(redact(`token ${TOKEN} here`)).toBe('token [redacted] here')
    expect(redact('Authorization: Bearer abc.def')).toBe('Authorization: Bearer [redacted]')
    expect(redact('ghs_' + 'a'.repeat(36))).toBe('[redacted]')
    expect(redact('signed in to https://drydock.test as "VS Code"')).toBe('signed in to https://drydock.test as "VS Code"')
  })
})

describe('routes', () => {
  it('matches contracted routes by method and fills parameters as one segment each', () => {
    expect(routeOf('POST', '/api/workspaces')?.name).toBe('workspaces.create')
    expect(routeOf('GET', '/api/workspaces')?.name).toBe('workspaces.list')
    expect(routeOf('DELETE', '/api/workspaces/abc/config-approval')?.name).toBe('workspaces.decline')
    expect(routeOf('PUT', '/api/workspaces')).toBeNull()
    expect(pathFor('workspaces.read', { id: 'a/b?c' })).toBe('/api/workspaces/a%2Fb%3Fc')
    expect(routeOf('GET', pathFor('workspaces.read', { id: 'a/b?c' }))?.name).toBe('workspaces.read')
    expect(() => pathFor('secrets.list')).toThrow()
  })
})
