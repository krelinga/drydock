// The ports panel's diagnosis (port forwarding §11, §13 step 6) against the
// pretend backend: each condition the table lists, said from structured state
// — the row's bind address and observed state, the workspace's state,
// discovery's reported state — and a refused action replaced by the one that
// can work, never shown disabled. The mock's discovery events are held to the
// server's by src/mocks/discovery.spec.ts. Every negative assertion sits
// beside its positive control.

import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { addMockPort, scanPorts, seedPorts, WS_REMOVED, WS_RUNNING } from '../mocks/backend'
import { sentenceFor } from '../api/messages'
import { useStreamStore } from '../stores/stream'
import { portRescanKey } from '../stores/ports'

useMockApi()

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
type Backend = ReturnType<typeof freshBackend>
const row = (w: Wrapper, port: number) => w.find(`[data-test="port"][data-port="${port}"]`)
const btn = (scope: { find: Wrapper['find'] }, name: string) => scope.find(`[data-test="${name}"] [data-test="action"]`)
const sent = (b: Backend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path)
const portId = (b: Backend, port: number) => Object.values(b.ports).find((p) => p.container_port === port && !p.retired)!.id

/** A running workspace whose container listens as given, scanned twice so discovery lists it. */
async function listening(socks: Array<{ port: number; bind: string }>, over: Partial<Backend> = {}) {
  const b = freshBackend({ signedIn: true, ...over })
  seedPorts(b)
  b.sockets[WS_RUNNING] = socks
  scanPorts(b)
  scanPorts(b)
  const app = await mountApp(`/ws/${WS_RUNNING}`)
  FakeEventSource.latest().open().pipe(b)
  await settle()
  return { b, ...app }
}

describe('port diagnosis', () => {
  it('a server on loopback says to use --host 0.0.0.0, is greyed, and offers Look again instead of the switch', async () => {
    const { b, wrapper, pinia } = await listening([{ port: 5173, bind: '127.0.0.1' }, { port: 8080, bind: '0.0.0.0' }, { port: 9229, bind: '::1' }])
    const loop = row(wrapper, 5173)
    expect(loop.attributes('data-diagnosis')).toBe('loopback')
    expect(loop.classes()).toContain('loop')
    expect(loop.find('[data-test="port-diagnosis"]').text())
      .toBe('Listening on 127.0.0.1:5173, which is only reachable from inside the container. Start it with --host 0.0.0.0.')
    expect(loop.find('[data-test="port-diagnosis"] code').text()).toBe('--host 0.0.0.0')
    expect(row(wrapper, 9229).find('[data-test="port-diagnosis"]').text()).toContain('Listening on [::1]:9229,')
    // Replaced, not disabled: no switch at all, and the action that can help.
    expect(loop.find('[data-test="port-enable"]').exists()).toBe(false)
    expect(loop.find('[data-test="port-off"]').text()).toBe('Not previewable while it listens on loopback.')
    expect(btn(loop, 'port-look-again').attributes('disabled')).toBeUndefined()
    // Control: the 0.0.0.0 row is offered the switch and says no diagnosis.
    const open = row(wrapper, 8080)
    expect(open.attributes('data-diagnosis')).toBeUndefined()
    expect(btn(open, 'port-enable').exists()).toBe(true)
    expect(open.find('[data-test="port-observed"]').text()).toBe('Listening on 0.0.0.0.')

    // Look again is the rescan, settled by its port.scanned.
    await btn(loop, 'port-look-again').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/ports/rescan`).length).toBe(1)
    expect(portRescanKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)

    // The server restarted on 0.0.0.0: two scans later the switch is back.
    b.sockets[WS_RUNNING] = [{ port: 5173, bind: '0.0.0.0' }]
    scanPorts(b)
    scanPorts(b)
    await settle()
    expect(row(wrapper, 5173).attributes('data-diagnosis')).toBeUndefined()
    expect(btn(row(wrapper, 5173), 'port-enable').exists()).toBe(true)
  })

  it('an enable that races a move to loopback is refused by code, with the sentence the code names', async () => {
    const { b, wrapper } = await listening([{ port: 5173, bind: '0.0.0.0' }])
    const id = portId(b, 5173)
    // The server moved; the scan that says so has not reached this page.
    b.ports[id] = { ...b.ports[id]!, bind_addr: '127.0.0.1', loopback: true }
    await btn(row(wrapper, 5173), 'port-enable').trigger('click')
    await settle()
    expect(b.ports[id]!.enabled).toBe(false)
    expect(row(wrapper, 5173).find('[data-test="action-error"]').text()).toContain(sentenceFor('port_loopback')!)
  })

  it('a loopback port already previewed keeps Turn off, loses its link, and the probe says why without a dial', async () => {
    const { b, wrapper } = await listening([{ port: 5173, bind: '0.0.0.0' }])
    const id = portId(b, 5173)
    b.ports[id] = { ...b.ports[id]!, enabled: true }
    b.sockets[WS_RUNNING] = [{ port: 5173, bind: '127.0.0.1' }]
    scanPorts(b)
    scanPorts(b)
    await settle()
    const p = row(wrapper, 5173)
    expect(p.attributes('data-diagnosis')).toBe('loopback')
    expect(p.find('[data-test="port-open"]').exists()).toBe(false)
    expect(btn(p, 'port-disable').exists()).toBe(true)
    await p.find('[data-test="port-probe"]').trigger('click')
    await settle()
    expect(row(wrapper, 5173).find('[data-test="port-probe-result"]').text())
      .toBe('Listening on 127.0.0.1:5173, which is only reachable from inside the container. Start it with --host 0.0.0.0.')
  })

  it('an enabled port nothing listens on says so, keeping its address and its switch', async () => {
    const { b, wrapper } = await listening([{ port: 5173, bind: '0.0.0.0' }])
    const id = portId(b, 5173)
    b.ports[id] = { ...b.ports[id]!, enabled: true }
    b.sockets[WS_RUNNING] = []
    for (let i = 0; i < 3; i++) scanPorts(b) // the grace
    await settle()
    const p = row(wrapper, 5173)
    expect(p.attributes('data-diagnosis')).toBe('not_listening')
    expect(p.find('[data-test="port-diagnosis"]').text())
      .toMatch(/^Nothing is listening on port 5173 now, and nothing has since .+: the dev server has not been started, or it has stopped\. Its preview answers once it listens again\.$/)
    expect(p.find('[data-test="port-open"]').exists()).toBe(true)
    expect(btn(p, 'port-disable').exists()).toBe(true)
    await p.find('[data-test="port-probe"]').trigger('click')
    await settle()
    expect(row(wrapper, 5173).find('[data-test="port-probe-result"]').text())
      .toBe("Nothing is listening on port 5173 in this workspace's container: the dev server has not been started, or it has stopped.")
    // Control: the same row switched off is only "not listening now".
    const storybook = row(wrapper, 6006) // declared, never seen, off
    expect(storybook.attributes('data-diagnosis')).toBeUndefined()
  })

  it('an enabled port never seen listening says so, once discovery is known to work', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const id = portId(b, 6006)
    b.ports[id] = { ...b.ports[id]!, enabled: true }
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    await settle()
    expect(row(wrapper, 6006).find('[data-test="port-diagnosis"]').text())
      .toBe('Nothing has listened on port 6006 yet. Its preview answers once a dev server listens on it, on 0.0.0.0.')
    // Control: with discovery unavailable, nothing is claimed about it.
    b.scanUnavailable = [WS_RUNNING]
    scanPorts(b)
    await settle()
    expect(row(wrapper, 6006).find('[data-test="port-diagnosis"]').exists()).toBe(false)
  })

  it('discovery unavailable is said on the panel, unasked and in the list, and goes when it recovers', async () => {
    const { b, wrapper } = await listening([{ port: 5173, bind: '0.0.0.0' }])
    expect(wrapper.find('[data-test="discovery-unavailable"]').exists()).toBe(false) // the control
    b.scanUnavailable = [WS_RUNNING]
    scanPorts(b)
    await settle()
    const badge = wrapper.find('[data-test="discovery-unavailable"]')
    expect(badge.exists()).toBe(true)
    expect(badge.text()).toContain('discovery unavailable')
    expect(badge.attributes('role')).toBeUndefined() // a state on the panel, not an alert
    expect(row(wrapper, 5173).exists()).toBe(true) // the rows are kept
    expect(wrapper.find('[data-test="port-add-form"]').exists()).toBe(true) // and adding works

    // A panel opened afresh reads it from the list.
    const { wrapper: w2 } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(w2.find('[data-test="discovery-unavailable"]').exists()).toBe(true)

    b.scanUnavailable = []
    scanPorts(b)
    await settle()
    expect(wrapper.find('[data-test="discovery-unavailable"]').exists()).toBe(false)
  })

  it('a budget holding changes back is said, and Look for listening ports now settles on limited, not ok', async () => {
    const { b, wrapper, pinia } = await listening([{ port: 5173, bind: '0.0.0.0' }], { scanLimited: [WS_RUNNING] })
    b.sockets[WS_RUNNING] = [{ port: 5173, bind: '0.0.0.0' }, { port: 3000, bind: '0.0.0.0' }]
    expect(wrapper.find('[data-test="discovery-limited"]').exists()).toBe(false)
    await btn(wrapper, 'ports-rescan').trigger('click')
    await settle()
    scanPorts(b) // the second sighting
    await settle()
    await btn(wrapper, 'ports-rescan').trigger('click')
    await settle()
    const scanned = b.events.filter((e) => e.kind === 'port.scanned').map((e) => (e.data as { discovery: string }).discovery)
    expect(scanned[scanned.length - 1]).toBe('limited')
    expect(portRescanKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    expect(row(wrapper, 3000).exists()).toBe(false)
    const said = wrapper.find('[data-test="discovery-limited"]')
    expect(said.exists()).toBe(true)
    expect(said.text()).toContain('some that are listening are not listed yet')

    // The budget refills: the held port is listed, and the panel says nothing more.
    b.scanLimited = []
    await btn(wrapper, 'ports-rescan').trigger('click')
    await settle()
    expect(row(wrapper, 3000).exists()).toBe(true)
    expect(wrapper.find('[data-test="discovery-limited"]').exists()).toBe(false)
  })

  it("a stopped workspace's preview lands on its page, saying why; running, the same note links the preview", async () => {
    const b = freshBackend({ signedIn: true })
    const p = addMockPort(b, WS_REMOVED, 3000, {})
    b.ports[p.id] = { ...b.ports[p.id]!, enabled: true }
    const { wrapper } = await mountApp(`/ws/${WS_REMOVED}?preview=${p.id}`)
    FakeEventSource.latest().open().pipe(b)
    await settle()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Stopped')
    expect(wrapper.find('[data-test="preview-not-running"]').text())
      .toBe("This workspace is not running, so port 3000's preview cannot answer. It answers again once the workspace is running.")
    // Control: no ?preview, no note; and a port of another workspace says nothing.
    const { wrapper: plain } = await mountApp(`/ws/${WS_REMOVED}`)
    expect(plain.find('[data-test="preview-not-running"]').exists()).toBe(false)
    const other = addMockPort(b, WS_RUNNING, 4000, {})
    const { wrapper: foreign } = await mountApp(`/ws/${WS_REMOVED}?preview=${other.id}`)
    expect(foreign.find('[data-test="preview-not-running"]').exists()).toBe(false)
  })
})

describe('the activity feed', () => {
  it("carries the operator's port changes and none of discovery's", async () => {
    const { b, wrapper } = await listening([{ port: 5173, bind: '0.0.0.0' }, { port: 8080, bind: '0.0.0.0' }])
    await btn(row(wrapper, 8080), 'port-enable').trigger('click')
    await settle()
    for (let i = 0; i < 3; i++) {
      await btn(wrapper, 'ports-rescan').trigger('click')
      await settle()
    }
    const feed = wrapper.findAll('[data-test="event-message"]').map((e) => e.text())
    expect(feed.some((m) => m.startsWith('Port 8080 previewed'))).toBe(true) // the control
    expect(feed.filter((m) => m.startsWith('Ports scanned') || m.includes('is listening'))).toEqual([])
    expect(b.events.some((e) => e.kind === 'port.scanned')).toBe(true) // the stream carried them
    // A reload reads the feed from GET /api/workspaces/:id, which leaves them out too.
    const { wrapper: again } = await mountApp(`/ws/${WS_RUNNING}`)
    const reloaded = again.findAll('[data-test="event-message"]').map((e) => e.text())
    expect(reloaded.some((m) => m.startsWith('Port 8080 previewed'))).toBe(true)
    expect(reloaded.filter((m) => m.startsWith('Ports scanned') || m.includes('is listening'))).toEqual([])
  })
})
