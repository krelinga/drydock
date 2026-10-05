// The workspaces store's own guards, without the button in front of them:
// a second create or start while one is in flight sends nothing (§4.2,
// frontend §4.5 #3 — the server's 409 is the guard across devices).

import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { freshBackend, settle, useMockApi } from '../test/setup'
import { WS_FAILED } from '../mocks/backend'
import { useStreamStore } from './stream'
import { cloneKey, startKey, useWorkspacesStore } from './workspaces'

useMockApi()

beforeEach(() => {
  setActivePinia(createPinia())
})

const posts = (b: ReturnType<typeof freshBackend>, path: string) =>
  b.log.filter((r) => r.method === 'POST' && new URL(r.url).pathname === path).length

describe('the workspaces store', () => {
  it('sends one create per repository while one is in flight', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const ws = useWorkspacesStore()
    await Promise.all([ws.create(3), ws.create(3)])
    expect(posts(b, '/api/workspaces')).toBe(1)
    expect(cloneKey(3) in useStreamStore().inFlight).toBe(true)
    // Control: another repository is its own request.
    await ws.create(4)
    expect(posts(b, '/api/workspaces')).toBe(2)
  })

  it('sends one start per workspace while one is in flight', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const ws = useWorkspacesStore()
    await Promise.all([ws.start(WS_FAILED), ws.start(WS_FAILED)])
    expect(posts(b, `/api/workspaces/${WS_FAILED}/start`)).toBe(1)
    // Control: once the refusal path ends it, a start is sent again.
    useStreamStore().end(startKey(WS_FAILED))
    await ws.start(WS_FAILED)
    expect(posts(b, `/api/workspaces/${WS_FAILED}/start`)).toBe(2)
  })

  it('a refused create clears its mark and rethrows the code', async () => {
    freshBackend({ signedIn: true, capacity: 0 })
    const ws = useWorkspacesStore()
    await expect(ws.create(3)).rejects.toMatchObject({ status: 409, code: 'at_capacity' })
    expect(cloneKey(3) in useStreamStore().inFlight).toBe(false)
    // Control: an accepted one keeps it.
    freshBackend({ signedIn: true, scriptMode: 'manual' })
    await ws.create(3)
    expect(cloneKey(3) in useStreamStore().inFlight).toBe(true)
  })

  it('the mock refuses a second create for a repository whose workspace failed, as the server does', async () => {
    // homelab (repo 2) holds WS_FAILED, failed at `up`: one repository, one
    // workspace, in any state, until the delete (design §5).
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const ws = useWorkspacesStore()
    await expect(ws.create(2)).rejects.toMatchObject({ status: 409, code: 'in_progress' })
    b.workspaces[WS_FAILED] = { ...b.workspaces[WS_FAILED]!, state: 'stopped' }
    await expect(ws.create(2)).rejects.toMatchObject({ status: 409, code: 'in_progress' })
    // Control: with the row gone, the same create is accepted.
    delete b.workspaces[WS_FAILED]
    await ws.create(2)
    expect(cloneKey(2) in useStreamStore().inFlight).toBe(true)
  })

  it('a start is refused at the cap, as a create is', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', capacity: 1 })
    const ws = useWorkspacesStore()
    await expect(ws.start(WS_FAILED)).rejects.toMatchObject({ status: 409, code: 'at_capacity' })
    // Control: with room, it is accepted.
    b.capacity = 2
    await ws.start(WS_FAILED)
    expect(startKey(WS_FAILED) in useStreamStore().inFlight).toBe(true)
  })

  it('loads the list and a detail into the reducer, and remembers a 404', async () => {
    freshBackend({ signedIn: true })
    const ws = useWorkspacesStore()
    await ws.loadList()
    expect(Object.keys(useStreamStore().entities.workspaces).length).toBe(3)
    await ws.loadOne('01JA00000000000000000NOPE0')
    await settle()
    expect(ws.detail['01JA00000000000000000NOPE0']?.status).toBe('not_found')
    await ws.loadOne(WS_FAILED)
    expect(ws.detail[WS_FAILED]?.status).toBe('ready')
    expect(useStreamStore().entities.feeds[WS_FAILED]?.length).toBeGreaterThan(0)
  })
})
