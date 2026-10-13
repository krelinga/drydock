// The window's live state (live.ts; vscode-extension §5.5, §2.6): nothing is
// asked of Drydock until something holds the stream; the snapshots are read
// after it opens; a resync and a move to running refetch; and the stream
// closes five minutes after nothing holds it — not before.

import { describe, expect, it } from 'vitest'
import { stateEvent } from '../../web/src/core/reducer.fixtures'
import { IDLE_MS } from './live'
import { statusView, MARKER_AFTER_MS, EXPIRY_WARN_MS } from './status'
import { NEW_WS, WS, flowWorld } from '../test/fakeDrydock'
import { flush } from '../test/fakes'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

describe('Live', () => {
  it('asks nothing until held; a hold opens the stream and then reads the snapshots and the handshake', async () => {
    const w = flowWorld()
    await flush()
    expect(w.server.transport.requests).toEqual([])
    expect(w.server.streams).toBe(0)
    const release = await w.live.hold()
    expect(w.server.streams).toBe(1)
    const paths = w.server.transport.requests.map((r) => new URL(r.url).pathname).sort()
    expect(paths).toEqual(['/api/meta', '/api/repos', '/api/workspaces'])
    expect(w.live.meta?.version).toBe('v0.12.2')
    release()
  })

  it('closes the stream five minutes after the last hold is released, not before', async () => {
    const w = flowWorld()
    const release = await w.live.hold()
    release()
    for (let t = 0; t < IDLE_MS - 20_000; t += 20_000) {
      w.clock.advance(20_000)
      w.server.stream!.push(': ping\n\n')
      await flush()
    }
    expect(w.live.streaming).toBe(true)
    w.clock.advance(20_000)
    await flush()
    expect(w.live.streaming).toBe(false)
    // Held again: a new stream, and the snapshots again.
    const again = await w.live.hold()
    expect(w.server.streams).toBe(2)
    again()
  })

  it('a resync refetches the snapshots; a move to running refetches that workspace, for its link', async () => {
    const w = flowWorld()
    const release = await w.live.hold()
    const count = (p: string) => w.server.transport.requests.filter((r) => new URL(r.url).pathname === p).length
    const lists = count('/api/workspaces')
    w.server.stream!.push('id: 500\nevent: resync\ndata: {}\n\n')
    await flush()
    expect(count('/api/workspaces')).toBe(lists + 1)
    w.server.emit(stateEvent(501, WS, 'running', { from: 'building' }))
    await flush()
    expect(count(`/api/workspaces/${WS}`)).toBe(1)
    w.server.emit(stateEvent(502, NEW_WS, 'building', { from: 'cloning' }))
    await flush()
    expect(count(`/api/workspaces/${NEW_WS}`)).toBe(0)
    release()
  })
})

describe('the status bar', () => {
  it('counts running workspaces, says idle once the stream has closed, and reconnecting only after five seconds', async () => {
    const w = flowWorld()
    const release = await w.live.hold()
    const now = w.clock.now()
    expect(statusView(w.live, now, true)).toMatchObject({ text: '$(server) Drydock: 1 running', command: 'drydock.go', warning: false })
    w.server.stream!.end()
    await flush()
    expect(w.live.phase).toBe('backoff')
    expect(statusView(w.live, w.clock.now() + MARKER_AFTER_MS - 1, true).text).toBe('$(server) Drydock: 1 running')
    expect(statusView(w.live, w.clock.now() + MARKER_AFTER_MS, true).text).toBe('$(server) Drydock: 1 running · reconnecting')
    release()
    w.live.close()
    expect(statusView(w.live, w.clock.now(), true).text).toBe('$(server) Drydock: 1 running · idle')
    expect(statusView(w.live, w.clock.now(), false)).toMatchObject({ command: 'drydock.signIn' })
  })

  it('a token within seven days of expiry says so ahead of the count, and a skewed server says that', async () => {
    const w = flowWorld()
    const release = await w.live.hold()
    const expires = Date.parse(w.live.meta!.credential.expires_at)
    expect(statusView(w.live, expires - EXPIRY_WARN_MS - 1, true).warning).toBe(false)
    expect(statusView(w.live, expires - EXPIRY_WARN_MS + 1, true)).toMatchObject({ text: '$(warning) Drydock token expires in 7 days', command: 'drydock.signIn', warning: true })
    expect(statusView(w.live, expires + 1, true).text).toBe('$(warning) Drydock token has expired')
    w.live.meta = { ...w.live.meta!, api: 9 }
    expect(statusView(w.live, 0, true)).toMatchObject({ text: '$(warning) Drydock: version mismatch', warning: true })
    release()
  })

  it("the quiet marker's delay is the web UI's", () => {
    const web = readFileSync(resolve(process.cwd(), '../web/src/stores/stream.ts'), 'utf8')
    expect(web).toContain(`export const MARKER_AFTER_MS = ${MARKER_AFTER_MS.toLocaleString('en').replace(',', '_')}`)
  })
})
