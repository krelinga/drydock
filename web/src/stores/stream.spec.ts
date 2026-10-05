// The stream store's connection handling (frontend §2.3, §4.3) and the
// in-flight set (§4.2), against a hand-driven EventSource.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { toRaw } from 'vue'
import { onUnauthorized } from '../api/client'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, settle, useMockApi } from '../test/setup'
import { CLONE_OK, REFRESHED, WS, stateEvent } from './reducer.fixtures'
import { MARKER_AFTER_MS, SLOW_AFTER_MS, useStreamStore } from './stream'

useMockApi()

beforeEach(() => {
  setActivePinia(createPinia())
})

afterEach(() => {
  useStreamStore().disconnect()
  vi.useRealTimers()
})

describe('connecting', () => {
  it('opens one stream on /api/events and feeds every frame to the reducer', () => {
    const stream = useStreamStore()
    stream.connect()
    stream.connect() // idempotent
    expect(FakeEventSource.instances.length).toBe(1)
    const es = FakeEventSource.latest()
    expect(es.url).toBe('/api/events')
    es.open()
    expect(stream.phase).toBe('live')
    for (const ev of CLONE_OK) es.send(ev)
    expect(stream.entities.workspaces[WS]?.state).toBe('running')
    expect(stream.lastEventId).toBe(13)
  })

  it('ignores a frame that is not an event, and keeps going', () => {
    const stream = useStreamStore()
    stream.connect()
    const es = FakeEventSource.latest().open()
    es.sendRaw('not json', '5').sendRaw('{"kind":1}', '6')
    expect(stream.lastEventId).toBe(0)
    // Control: the next real frame is applied.
    es.send(CLONE_OK[0]!)
    expect(stream.entities.workspaces[WS]?.state).toBe('pending')
  })
})

describe('a drop the browser retries (readyState CONNECTING)', () => {
  it('shows nothing for five seconds, then the quiet marker, and keeps the data', () => {
    vi.useFakeTimers()
    const stream = useStreamStore()
    stream.connect()
    const es = FakeEventSource.latest().open()
    for (const ev of CLONE_OK) es.send(ev)

    es.drop()
    expect(stream.phase).toBe('retrying')
    vi.advanceTimersByTime(MARKER_AFTER_MS - 1)
    expect(stream.reconnecting).toBe(false)
    vi.advanceTimersByTime(1)
    expect(stream.reconnecting).toBe(true)
    // Stale-but-labelled, never blank.
    expect(stream.entities.workspaces[WS]?.state).toBe('running')
    // The browser retries on the same object; nothing new was created.
    expect(FakeEventSource.instances.length).toBe(1)

    es.open()
    expect(stream.reconnecting).toBe(false)
    expect(stream.phase).toBe('live')
  })

  it('a blip shorter than five seconds never shows the marker', () => {
    vi.useFakeTimers()
    const stream = useStreamStore()
    stream.connect()
    const es = FakeEventSource.latest().open()
    es.drop()
    vi.advanceTimersByTime(2000)
    es.open()
    vi.advanceTimersByTime(10_000)
    expect(stream.reconnecting).toBe(false)
    // Control: the same drop left alone does show it.
    es.drop()
    vi.advanceTimersByTime(MARKER_AFTER_MS)
    expect(stream.reconnecting).toBe(true)
  })

  it('refetches on every open after the first, and not on the first', () => {
    const stream = useStreamStore()
    const refetch = vi.fn()
    stream.addRefetcher({ refetch })
    stream.connect()
    const es = FakeEventSource.latest().open()
    expect(refetch).not.toHaveBeenCalled()
    es.drop().open()
    expect(refetch).toHaveBeenCalledTimes(1)
    es.drop().open()
    expect(refetch).toHaveBeenCalledTimes(2)
  })
})

describe('a stream that fails for good (readyState CLOSED)', () => {
  it('probes the session; a 401 signs out and nothing reconnects', async () => {
    freshBackend({ signedIn: false })
    const stream = useStreamStore()
    const on401 = vi.fn(() => stream.disconnect())
    onUnauthorized(on401)
    stream.connect()
    FakeEventSource.latest().open().refuse()
    expect(stream.phase).toBe('probing')
    await settle()
    expect(on401).toHaveBeenCalledTimes(1)
    expect(stream.phase).toBe('idle')
    await new Promise((r) => setTimeout(r, 1100))
    expect(FakeEventSource.instances.length).toBe(1)
  })

  it('a probe that succeeds is a hard retry with backoff, and the new stream refetches', async () => {
    freshBackend({ signedIn: true })
    const stream = useStreamStore()
    const on401 = vi.fn()
    onUnauthorized(on401)
    const refetch = vi.fn()
    stream.addRefetcher({ refetch })
    stream.connect()
    const first = FakeEventSource.latest().open()
    for (const ev of CLONE_OK) first.send(ev)
    first.refuse()
    expect(first.closed).toBe(true)
    await settle()
    expect(on401).not.toHaveBeenCalled()
    expect(stream.phase).toBe('backoff')
    expect(FakeEventSource.instances.length).toBe(1)
    await new Promise((r) => setTimeout(r, 1100))
    expect(FakeEventSource.instances.length).toBe(2)
    // A new EventSource cannot send Last-Event-ID, so it asks for the
    // replay in its URL instead — from the last event the first one saw.
    expect(FakeEventSource.latest().url).toBe('/api/events?last_event_id=13')
    FakeEventSource.latest().open()
    expect(refetch).toHaveBeenCalledTimes(1)
    expect(stream.phase).toBe('live')
  })

  it('disconnect during the backoff cancels the retry', async () => {
    freshBackend({ signedIn: true })
    const stream = useStreamStore()
    stream.connect()
    FakeEventSource.latest().open().refuse()
    await settle()
    expect(stream.phase).toBe('backoff')
    stream.disconnect()
    await new Promise((r) => setTimeout(r, 1100))
    expect(FakeEventSource.instances.length).toBe(1)
  })
})

describe('resync and unplaceable events', () => {
  it('a resync frame marks the data stale and refetches', () => {
    const stream = useStreamStore()
    const refetch = vi.fn()
    stream.addRefetcher({ refetch })
    stream.connect()
    const es = FakeEventSource.latest().open()
    es.send(CLONE_OK[0]!)
    expect(stream.stale).toBe(false)
    es.resync(900)
    expect(stream.stale).toBe(true)
    expect(stream.lastEventId).toBe(900)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('an event for a workspace it cannot place refetches; one it can does not', () => {
    const stream = useStreamStore()
    const refetch = vi.fn()
    stream.addRefetcher({ refetch, when: (ev) => ev.kind.startsWith('repo.') })
    stream.connect()
    const es = FakeEventSource.latest().open()
    es.send(CLONE_OK[0]!) // a create: carries its repository, so it is placed
    expect(refetch).not.toHaveBeenCalled()
    es.send(stateEvent(50, '01JA0000000000000000000099', 'building'))
    expect(refetch).toHaveBeenCalledTimes(1)
    es.send(REFRESHED(51))
    expect(refetch).toHaveBeenCalledTimes(2)
  })

  it('a refetcher unregistered no longer runs', () => {
    const stream = useStreamStore()
    const refetch = vi.fn()
    const off = stream.addRefetcher({ refetch })
    stream.connect()
    const es = FakeEventSource.latest().open()
    es.resync(1)
    off()
    es.resync(2)
    expect(refetch).toHaveBeenCalledTimes(1)
  })
})

describe('the in-flight set (§4.2)', () => {
  it('is cleared by the settling event, not by any other', () => {
    const stream = useStreamStore()
    stream.connect()
    const es = FakeEventSource.latest().open()
    stream.begin('catalog:refresh', (ev) => ev.kind === 'repo.refreshed')
    expect('catalog:refresh' in stream.inFlight).toBe(true)
    es.send(CLONE_OK[0]!)
    expect('catalog:refresh' in stream.inFlight).toBe(true)
    es.send(REFRESHED(20))
    expect('catalog:refresh' in stream.inFlight).toBe(false)
  })

  it('settles when an action runs with a wrapping proxy as `this`, as Pinia\'s devtools call it', () => {
    // pinia's devtools plugin (dev builds, so `npm run dev:mock`) calls every
    // action with `this` set to a fresh `new Proxy(store, …)`. Keyed by
    // `this`, `begin` and `receive` got different runtimes, and no button
    // ever settled in the browser. The runtime must be the store's own.
    const stream = useStreamStore()
    stream.connect()
    const es = FakeEventSource.latest().open()
    const tracked = () => new Proxy(stream, {})
    stream.begin.call(tracked(), 'catalog:refresh', (ev) => ev.kind === 'repo.refreshed')
    expect('catalog:refresh' in stream.inFlight).toBe(true)
    es.send(REFRESHED(20))
    expect('catalog:refresh' in stream.inFlight).toBe(false)
    // And the other way round: begun plainly, received through a proxy.
    stream.begin('catalog:refresh', (ev) => ev.kind === 'repo.refreshed')
    stream.receive.call(tracked(), REFRESHED(21))
    expect('catalog:refresh' in stream.inFlight).toBe(false)
    // Control: the raw store is the key, so it is the same runtime.
    expect(toRaw(tracked())).toBe(toRaw(stream))
  })

  it('says "no response yet" after ten seconds and never turns into a failure', () => {
    vi.useFakeTimers()
    const stream = useStreamStore()
    stream.begin('catalog:refresh', () => false)
    vi.advanceTimersByTime(SLOW_AFTER_MS - 1)
    expect(stream.inFlight['catalog:refresh']?.slow).toBe(false)
    vi.advanceTimersByTime(1)
    expect(stream.inFlight['catalog:refresh']?.slow).toBe(true)
    vi.advanceTimersByTime(10 * 60_000)
    expect(stream.inFlight['catalog:refresh']).toMatchObject({ slow: true })
    // Control: end() does clear it.
    stream.end('catalog:refresh')
    expect(stream.inFlight['catalog:refresh']).toBeUndefined()
  })
})

describe('disconnect', () => {
  it('closes the stream, and a closed stream feeds nothing', () => {
    const stream = useStreamStore()
    stream.connect()
    const es = FakeEventSource.latest().open()
    es.send(CLONE_OK[0]!)
    stream.disconnect()
    expect(es.closed).toBe(true)
    expect(stream.phase).toBe('idle')
    es.send(CLONE_OK[3]!)
    expect(stream.entities.workspaces[WS]?.state).toBe('pending')
    // Control: connecting again makes a new stream that does feed.
    stream.connect()
    FakeEventSource.latest().open().send(CLONE_OK[3]!)
    expect(stream.entities.workspaces[WS]?.state).toBe('cloning')
  })
})
