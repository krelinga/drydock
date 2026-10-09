// Port discovery in the UI (port forwarding §8.2, §13 step 5): the done-when —
// a server started on an undeclared port with the panel already open appears,
// disabled and correctly labelled, and nothing is pushed at you — plus the
// rescan's lifecycle and the card's ambient count. The events are the mock's,
// which src/mocks/discovery.spec.ts holds to the server's. Every negative
// assertion sits beside its positive control.

import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { scanPorts, SCAN_RETIRE_SCANS, seedPorts, WS_RUNNING } from '../mocks/backend'
import { useStreamStore } from '../stores/stream'
import { portRescanKey } from '../stores/ports'

useMockApi()

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
type Backend = ReturnType<typeof freshBackend>
const row = (w: Wrapper, port: number) => w.find(`[data-test="port"][data-port="${port}"]`)
const btn = (scope: { find: Wrapper['find'] }, name: string) => scope.find(`[data-test="${name}"] [data-test="action"]`)
const sent = (b: Backend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path)
const runningCard = (w: Wrapper, id: string) =>
  w.findAll('[data-test="running-row"]').find((r) => r.find('[data-test="ws-link"]').attributes('href') === `/ws/${id}`)!

/** Everything on the page that could interrupt: alerts, the live announcement, the title. */
function interruptions(): { alerts: number; announced: string; title: string } {
  return {
    alerts: document.querySelectorAll('[role="alert"]').length,
    announced: Array.from(document.querySelectorAll('[aria-live]')).map((n) => n.textContent ?? '').join('|'),
    title: document.title,
  }
}

describe('port discovery', () => {
  it('lists a server started with the panel open, off and labelled, and pushes nothing', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    await settle()
    expect(row(wrapper, 5173).exists()).toBe(true) // the panel is open, with the declared rows
    const before = interruptions()

    b.sockets[WS_RUNNING] = [
      { port: 5173, bind: '0.0.0.0' }, { port: 8080, bind: '0.0.0.0' }, { port: 9229, bind: '127.0.0.1' },
    ]
    scanPorts(b)
    await settle()
    expect(row(wrapper, 8080).exists()).toBe(false) // one scan is not enough
    scanPorts(b)
    await settle()

    const found = row(wrapper, 8080)
    expect(found.exists()).toBe(true)
    expect(found.findAll('[data-test="port-badge"]').map((x) => x.text())).toEqual(['discovered'])
    expect(found.find('[data-test="port-observed"]').text()).toBe('Listening on 0.0.0.0.')
    expect(found.find('[data-test="port-off"]').text()).toBe('Not previewed.')
    expect(found.find('[data-test="port-open"]').exists()).toBe(false)
    expect(btn(found, 'port-enable').exists()).toBe(true) // offered, never pressed
    const loop = row(wrapper, 9229)
    expect(loop.find('[data-test="port-observed"]').text())
      .toBe('Listening on 127.0.0.1 only, which nothing outside the container can reach.')
    // The declared row the server listens on is one row, now both.
    expect(wrapper.findAll('[data-test="port"][data-port="5173"]').length).toBe(1)
    expect(row(wrapper, 5173).findAll('[data-test="port-badge"]').map((x) => x.text())).toEqual(['declared', 'discovered'])
    expect(Object.values(b.ports).filter((p) => p.enabled).length).toBe(0)
    expect(sent(b, 'PATCH', `/api/workspaces/${WS_RUNNING}/ports/${Object.values(b.ports).find((p) => p.container_port === 8080)!.id}`)).toEqual([])

    // Nothing pushed: no alert, no announcement, no title change, no press
    // in flight — the count on the card is the only other thing that moved.
    expect(interruptions()).toEqual(before)
    expect(Object.keys(useStreamStore(pinia).inFlight)).toEqual([])
    expect(wrapper.find('[data-test="port-count"]').text()).toContain('3 listening')
  })

  it('a server that stops is said to have stopped, and one only discovery listed goes ten minutes later', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    b.sockets[WS_RUNNING] = [{ port: 5173, bind: '0.0.0.0' }, { port: 8080, bind: '0.0.0.0' }]
    scanPorts(b)
    scanPorts(b)
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    await settle()
    expect(row(wrapper, 8080).exists()).toBe(true)
    b.sockets[WS_RUNNING] = []
    scanPorts(b)
    scanPorts(b)
    await settle()
    expect(row(wrapper, 8080).exists()).toBe(true) // inside the grace
    scanPorts(b)
    await settle()
    expect(row(wrapper, 5173).find('[data-test="port-observed"]').text()).toBe('Not listening now.')
    expect(row(wrapper, 8080).find('[data-test="port-observed"]').text()).toBe('Not listening now.')
    for (let i = 3; i < SCAN_RETIRE_SCANS; i++) scanPorts(b)
    await settle()
    expect(row(wrapper, 8080).exists()).toBe(false)
    expect(row(wrapper, 5173).exists()).toBe(true) // declared: kept
  })

  it('a rescan is in flight until its port.scanned, not its 202', async () => {
    const b = freshBackend({ signedIn: true, scanMode: 'manual' })
    seedPorts(b)
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    const es = FakeEventSource.latest().open()
    const stream = useStreamStore(pinia)
    await btn(wrapper, 'ports-rescan').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/ports/rescan`).length).toBe(1)
    expect(portRescanKey(WS_RUNNING) in stream.inFlight).toBe(true)
    // Another workspace's answer is not this one's.
    es.send({ id: 9000, kind: 'port.scanned', level: 'info', message: 'Ports scanned.', at: new Date().toISOString(),
      workspace_id: '01JA0000000000000000000009', data: { discovery: 'ok', source: 'discovery' } })
    await settle()
    expect(portRescanKey(WS_RUNNING) in stream.inFlight).toBe(true)
    scanPorts(b, [WS_RUNNING])
    es.send(b.events[b.events.length - 1]!)
    await settle()
    expect(b.events[b.events.length - 1]!.kind).toBe('port.scanned')
    expect(portRescanKey(WS_RUNNING) in stream.inFlight).toBe(false)
  })

  it("the home card's count is passive text: listening and previewed, no button", async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    b.sockets[WS_RUNNING] = [{ port: 5173, bind: '0.0.0.0' }, { port: 8080, bind: '127.0.0.1' }]
    scanPorts(b)
    scanPorts(b)
    const vite = Object.values(b.ports).find((p) => p.container_port === 5173)!
    b.ports[vite.id] = { ...vite, enabled: true }
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    await settle()
    const count = runningCard(wrapper, WS_RUNNING).find('[data-test="port-count"]')
    expect(count.exists()).toBe(true)
    expect(count.text()).toContain('2 listening · 1 previewed')
    expect(count.findAll('button, a').length).toBe(0)
    // A port appearing moves the number and nothing else.
    const before = interruptions()
    b.sockets[WS_RUNNING] = [...b.sockets[WS_RUNNING]!, { port: 3000, bind: '0.0.0.0' }]
    scanPorts(b)
    scanPorts(b)
    await settle()
    expect(runningCard(wrapper, WS_RUNNING).find('[data-test="port-count"]').text()).toContain('3 listening · 1 previewed')
    expect(interruptions()).toEqual(before)
  })
})
