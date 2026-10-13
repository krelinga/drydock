// The control channel between the extension-host tier's specs, which run
// inside VS Code's extension host, and the stack, which runs in the test
// process outside it: a loopback HTTP server the specs ask what the tap saw,
// to mint a token, or for a workspace's link as the server wrote it. Its
// answers carry tokens only where a spec needs one (a mint), and the tap's
// Authorization values only as labels.

import http from 'node:http'
import type { AddressInfo } from 'node:net'
import type { ExtensionStack } from '../../../test/extension/stack'
import { UI_HOST } from '../../../test/extension/stack'

export interface Ctl {
  url: string
  close(): Promise<void>
}

export async function startCtl(stack: ExtensionStack): Promise<Ctl> {
  const server = http.createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      void answer(stack, req.method ?? '', new URL(req.url ?? '/', 'http://ctl').pathname, body)
        .then((v) => {
          res.writeHead(200, { 'Content-Type': 'application/json' })
          res.end(JSON.stringify(v ?? null))
        }, (e: unknown) => {
          res.writeHead(500, { 'Content-Type': 'text/plain' })
          res.end(String(e))
        })
    })
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  return {
    url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    close: () => new Promise((r) => {
      server.closeAllConnections()
      server.close(() => r())
    }),
  }
}

const others: Array<{ stop(): Promise<void> }> = []

async function answer(stack: ExtensionStack, method: string, path: string, body: string): Promise<unknown> {
  switch (`${method} ${path}`) {
    case 'GET /origin':
      return stack.origin
    case 'GET /tap':
      return stack.tap.seen.map((s) => ({ method: s.method, path: s.path, authorization: stack.bearerLabel(s.authorization), origin: s.origin, status: s.status }))
    case 'POST /tap/clear':
      stack.tap.clear()
      return true
    case 'POST /mint': {
      const { scopes, label } = JSON.parse(body) as { scopes: string[]; label?: string }
      return stack.mintToken(scopes, label)
    }
    case 'POST /label':
      return stack.labelOf((JSON.parse(body) as { token: string }).token)
    case 'GET /links': {
      const r = await stack.direct('GET', '/api/workspaces')
      const list = JSON.parse(r.body) as { workspaces: Array<{ id: string; full_name: string }> }
      const out: Record<string, string | null> = {}
      for (const w of list.workspaces) {
        const v = JSON.parse((await stack.direct('GET', `/api/workspaces/${w.id}`)).body) as { vscode?: { url?: string | null } }
        out[w.full_name] = v.vscode?.url ?? null
      }
      return out
    }
    case 'GET /approved': {
      // The hash config.approved recorded for the docker-in-docker workspace, if any.
      const r = await stack.direct('GET', '/api/workspaces')
      const list = JSON.parse(r.body) as { workspaces: Array<{ id: string; full_name: string }> }
      const w = list.workspaces.find((x) => x.full_name === 'drydock-fixture/dind')
      if (w === undefined) return null
      const v = JSON.parse((await stack.direct('GET', `/api/workspaces/${w.id}`)).body) as { events?: Array<{ kind: string; data?: { hash?: string } }> }
      return v.events?.find((e) => e.kind === 'config.approved')?.data?.hash ?? null
    }
    case 'POST /wrongcert': {
      const c = await stack.otherCaddy(`wrong-${others.length}`, { uiCert: stack.otherCert() })
      others.push(c)
      return `https://${UI_HOST}:${c.httpsPort}`
    }
  }
  throw new Error(`ctl: no ${method} ${path}`)
}

export async function stopOthers(): Promise<void> {
  for (const c of others.splice(0)) await c.stop().catch(() => {})
}
