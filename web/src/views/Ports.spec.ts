// The ports panel (frontend §6.3; port forwarding §13 step 4) through §4.2's
// lifecycle against the pretend backend: off until enabled, a switch in
// flight until the event that ends it — never the 202 — the full host shown
// and opened in a new tab, a disable taking the link away, retire, add and
// the probe. Every negative assertion sits beside its positive control.

import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { addMockPort, seedPorts, WS_FAILED, WS_RUNNING } from '../mocks/backend'
import { useStreamStore } from '../stores/stream'
import { portAddKey, portEnableKey, portRetireKey } from '../stores/ports'

useMockApi()

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
type Backend = ReturnType<typeof freshBackend>
const row = (w: Wrapper, port: number) => w.find(`[data-test="port"][data-port="${port}"]`)
const btn = (scope: { find: Wrapper['find'] }, name: string) => scope.find(`[data-test="${name}"] [data-test="action"]`)
const sent = (b: Backend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path)
const portId = (b: Backend, port: number) => Object.values(b.ports).find((p) => p.container_port === port && !p.retired)!.id

describe('the ports panel', () => {
  it('lists the declared ports off, with no address and no link', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    const vite = row(wrapper, 5173)
    expect(vite.exists()).toBe(true)
    expect(vite.find('[data-test="port-label"]').text()).toBe('vite dev server')
    expect(vite.findAll('[data-test="port-badge"]').map((x) => x.text())).toEqual(['declared'])
    expect(vite.find('[data-test="port-off"]').text()).toBe('Not previewed.')
    expect(vite.find('[data-test="port-open"]').exists()).toBe(false)
    expect(vite.text()).not.toContain('drydock-preview.test')
    // Control: the switch that can work is offered.
    expect(btn(vite, 'port-enable').exists()).toBe(true)
    expect(sent(b, 'GET', `/api/workspaces/${WS_RUNNING}/ports`).length).toBe(1)
    expect(new URL(b.log.find((r) => new URL(r.url).pathname.endsWith('/ports'))!.url).searchParams.get('hidden')).toBe('true')
  })

  it('enables on the event, not the 202, and then shows the full host opening in a new tab', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    const es = FakeEventSource.latest().open() // not piped: the stream is ours to deliver
    const stream = useStreamStore(pinia)
    const id = portId(b, 5173)
    await btn(row(wrapper, 5173), 'port-enable').trigger('click')
    await settle()
    // The server accepted it and wrote its event; neither has reached this page.
    expect(sent(b, 'PATCH', `/api/workspaces/${WS_RUNNING}/ports/${id}`).length).toBe(1)
    expect(b.ports[id]!.enabled).toBe(true)
    expect(portEnableKey(id) in stream.inFlight).toBe(true)
    expect(row(wrapper, 5173).find('[data-test="port-open"]').exists()).toBe(false)
    expect(btn(row(wrapper, 5173), 'port-enable').attributes('disabled')).toBeDefined()
    // The event arrives: the mark ends and the row is what it says.
    es.send(b.events[b.events.length - 1]!)
    await settle()
    expect(portEnableKey(id) in stream.inFlight).toBe(false)
    const link = row(wrapper, 5173).find('[data-test="port-open"]')
    const host = `${b.ports[id]!.slug}.drydock-preview.test`
    expect(link.text()).toBe(host)
    expect(link.attributes('href')).toBe(`https://${host}/`)
    expect(link.attributes('target')).toBe('_blank')
    expect(link.attributes('rel')).toBe('noopener noreferrer')
    expect(btn(row(wrapper, 5173), 'port-disable').exists()).toBe(true)
    // Its neighbour, never touched, stays off.
    expect(row(wrapper, 6006).find('[data-test="port-open"]').exists()).toBe(false)
  })

  it('disables: the link goes once the event says so', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const id = portId(b, 5173)
    await btn(row(wrapper, 5173), 'port-enable').trigger('click')
    await settle()
    expect(row(wrapper, 5173).find('[data-test="port-open"]').exists()).toBe(true)
    await btn(row(wrapper, 5173), 'port-disable').trigger('click')
    await settle()
    expect(portEnableKey(id) in useStreamStore(pinia).inFlight).toBe(false)
    expect(row(wrapper, 5173).find('[data-test="port-open"]').exists()).toBe(false)
    expect(btn(row(wrapper, 5173), 'port-enable').exists()).toBe(true)
    const bodies = sent(b, 'PATCH', `/api/workspaces/${WS_RUNNING}/ports/${id}`)
    expect(bodies.length).toBe(2)
    expect(b.events.slice(-2).map((e) => e.kind)).toEqual(['port.enabled', 'port.disabled'])
  })

  it('a change made on another device reaches this page by its event alone', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    await settle() // the first open's refetch (stream.ts)
    // Another device lists 3000 and enables it: two events, no request from here.
    const before = b.log.length
    const p = addMockPort(b, WS_RUNNING, 3000, { label: 'api' })
    await settle()
    expect(row(wrapper, 3000).find('[data-test="port-label"]').text()).toBe('api')
    expect(b.log.slice(before).map((r) => r.method + " " + new URL(r.url).pathname)).toEqual([])
    await fetch(`/api/workspaces/${WS_RUNNING}/ports/${p.id}`, { method: 'PATCH', body: '{"enabled":true}' })
    await settle()
    expect(row(wrapper, 3000).find('[data-test="port-open"]').exists()).toBe(true)
  })

  it('adds a port by number, refusing a bad number or a listed one before sending', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const form = wrapper.find('[data-test="port-add-form"]')
    for (const bad of ['0', '70000', 'abc', '80.5']) {
      await form.find('[data-test="port-add-number"]').setValue(bad)
      expect(form.find('[data-test="port-add-invalid"]').exists()).toBe(true)
      expect(btn(form, 'port-add').attributes('disabled')).toBeDefined()
    }
    await form.find('[data-test="port-add-number"]').setValue('5173')
    expect(form.find('[data-test="port-add-taken"]').exists()).toBe(true)
    expect(btn(form, 'port-add').attributes('disabled')).toBeDefined()
    // Control: a free number is sent, and the row arrives off by its event.
    await form.find('[data-test="port-add-number"]').setValue('8080')
    await form.find('[data-test="port-add-label"]').setValue('  api  ')
    await btn(form, 'port-add').trigger('click')
    await settle()
    const posts = sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/ports`)
    expect(posts.length).toBe(1)
    expect(portAddKey(WS_RUNNING, 8080) in useStreamStore(pinia).inFlight).toBe(false)
    const added = row(wrapper, 8080)
    expect(added.find('[data-test="port-label"]').text()).toBe('api')
    expect(added.findAll('[data-test="port-badge"]').map((x) => x.text())).toEqual(['added by hand'])
    expect(added.find('[data-test="port-open"]').exists()).toBe(false)
  })

  it('removes a port only after its confirm, and the row goes on port.retired', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const id = portId(b, 6006)
    await row(wrapper, 6006).find('[data-test="port-remove"]').trigger('click')
    expect(sent(b, 'DELETE', `/api/workspaces/${WS_RUNNING}/ports/${id}`).length).toBe(0)
    expect(row(wrapper, 6006).find('[data-test="port-remove-confirm"]').text()).toContain('retires')
    await btn(row(wrapper, 6006), 'port-retire').trigger('click')
    await settle()
    expect(sent(b, 'DELETE', `/api/workspaces/${WS_RUNNING}/ports/${id}`).length).toBe(1)
    expect(portRetireKey(id) in useStreamStore(pinia).inFlight).toBe(false)
    expect(row(wrapper, 6006).exists()).toBe(false)
    expect(row(wrapper, 5173).exists()).toBe(true)
    // Listed again, it is a new row with a new address.
    const again = addMockPort(b, WS_RUNNING, 6006)
    await settle()
    expect(again.id).not.toBe(id)
    expect(again.slug).not.toBe(b.ports[id]!.slug)
    expect(row(wrapper, 6006).exists()).toBe(true)
  })

  it('checks a port through the probe and says what the proxy would', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    await row(wrapper, 5173).find('[data-test="port-probe"]').trigger('click')
    await settle()
    expect(row(wrapper, 5173).find('[data-test="port-probe-result"]').text())
      .toBe("Something is answering on port 5173 in this workspace's container.")
    await row(wrapper, 6006).find('[data-test="port-probe"]').trigger('click')
    await settle()
    expect(row(wrapper, 6006).find('[data-test="port-probe-result"]').text()).toContain('Nothing is answering on port 6006')
    // A probe is a read: no event, no entity.
    expect(b.events.at(-1)!.kind).toBe('port.added')
  })

  it('switches the Host header, and hides a row behind "show hidden"', async () => {
    const b = freshBackend({ signedIn: true })
    seedPorts(b)
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    expect(row(wrapper, 5173).find('[data-test="port-host-mode"]').text()).toContain('Host: localhost:5173')
    await btn(row(wrapper, 5173), 'port-host').trigger('click')
    await settle()
    expect(b.ports[portId(b, 5173)]!.host_header).toBe('passthrough')
    expect(row(wrapper, 5173).find('[data-test="port-host-mode"]').text()).toContain("preview's own host name")
    await btn(row(wrapper, 6006), 'port-hide').trigger('click')
    await settle()
    expect(row(wrapper, 6006).exists()).toBe(false)
    await wrapper.find('[data-test="ports-show-hidden"] input').setValue(true)
    expect(row(wrapper, 6006).exists()).toBe(true)
  })

  it('with previews off, lists ports and offers no switch that cannot work', async () => {
    const b = freshBackend({ signedIn: true, previewDomain: null })
    seedPorts(b)
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(wrapper.find('[data-test="previews-off"]').exists()).toBe(true)
    expect(row(wrapper, 5173).exists()).toBe(true)
    expect(row(wrapper, 5173).find('[data-test="port-enable"]').exists()).toBe(false)
    // Control: with a domain, the same row offers it.
    const b2 = freshBackend({ signedIn: true })
    seedPorts(b2)
    const { wrapper: w2 } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(w2.find('[data-test="previews-off"]').exists()).toBe(false)
    expect(btn(row(w2, 5173), 'port-enable').exists()).toBe(true)
  })

  it('says a workspace with nothing listed has nothing yet, and how to add one', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_FAILED}`)
    expect(wrapper.find('[data-test="ports-empty"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="port-add-form"]').exists()).toBe(true)
  })
})