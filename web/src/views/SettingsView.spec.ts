import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { freshBackend, mountApp, server, settle, useMockApi } from '../test/setup'
import { useSessionStore } from '../stores/session'
import { useStreamStore } from '../stores/stream'
import { completeRefresh } from '../mocks/backend'
import { FakeEventSource } from '../test/fakeEventSource'

useMockApi()

describe('SettingsView catalog refresh', () => {
  it('shows only in-flight until the event arrives, and applies nothing from the 202', async () => {
    const b = freshBackend({ signedIn: true, refreshMode: 'manual' })
    // A 202 that carries a body the UI must not apply (§4.2 step 3).
    server.use(http.post('/api/repos/refresh', ({ request }) => {
      b.log.push({ method: request.method, url: request.url, credentials: request.credentials, mode: request.mode, contentType: null })
      b.pendingRefreshes = 1
      return HttpResponse.json({ count: 999, ok: true }, { status: 202 })
    }))
    const { wrapper, pinia } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)
    const button = () => wrapper.find('[data-test="refresh-catalog"]')
    const status = () => wrapper.find('[data-test="refresh-status"]').text()

    await button().trigger('click')
    await settle()
    expect(b.log.filter((r) => r.method === 'POST' && r.url.endsWith('/api/repos/refresh')).length).toBe(1)
    // Accepted, and still only "in flight": the response landed, nothing settled.
    expect(button().attributes('disabled')).toBeDefined()
    expect(status()).toBe('Asking GitHub…')
    expect(stream.entities.lastRefresh).toBeNull()
    expect(wrapper.text()).not.toContain('999')

    completeRefresh(b, true)
    await settle()
    expect(button().attributes('disabled')).toBeUndefined()
    expect(status()).toMatch(/^Refreshed just now: 5 repositories\.$/)
  })

  it('a failed refresh reports the event, and a refused request clears in-flight', async () => {
    const b = freshBackend({ signedIn: true, refreshMode: 'manual' })
    const { wrapper } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    await wrapper.find('[data-test="refresh-catalog"]').trigger('click')
    await settle()
    completeRefresh(b, false)
    await settle()
    expect(wrapper.find('[data-test="refresh-status"]').text()).toContain('Bad credentials')

    // The request itself refused: no event will ever come, so in-flight ends
    // and the error says why.
    b.appConfigured = false
    await wrapper.find('[data-test="refresh-catalog"]').trigger('click')
    await settle()
    expect(wrapper.find('[data-test="refresh-error"]').text()).toContain('No GitHub App is set up yet')
    expect(wrapper.find('[data-test="refresh-catalog"]').attributes('disabled')).toBeUndefined()
  })

  it('a refresh started on another device settles here too', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    expect(wrapper.find('[data-test="refresh-status"]').text()).toContain('every 15 minutes')
    completeRefresh(b, true)
    await settle()
    expect(wrapper.find('[data-test="refresh-status"]').text()).toContain('5 repositories')
  })
})

describe('SettingsView device list', () => {
  it('lists every device and marks exactly the current one', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/settings')
    const rows = wrapper.findAll('[data-test="device"]')
    expect(rows.map((r) => r.find('.label').text())).toEqual(['iPhone Safari', 'Mac Firefox', 'Linux Chrome'])
    const marked = rows.filter((r) => r.find('[data-test="this-device"]').exists())
    expect(marked.length).toBe(1)
    expect(marked[0]!.text()).toContain('iPhone Safari')
    // Control: the other rows render, they are just not marked.
    expect(rows[1]!.text()).toContain('192.168.1.31')
    expect(rows[1]!.find('[data-test="this-device"]').exists()).toBe(false)
  })

  it('sign out everywhere warns that it includes this device, and cancel sends nothing', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper, router } = await mountApp('/settings')
    const deletes = () => b.log.filter((r) => r.method === 'DELETE')

    await wrapper.find('[data-test="sign-out-everywhere"]').trigger('click')
    const sheet = wrapper.find('[data-test="confirm-everywhere"]')
    expect(sheet.exists()).toBe(true)
    expect(sheet.find('[data-test="includes-current"]').text()).toMatch(/includes this device\s+and 2 others/)
    // The first click asked; it did not act.
    expect(deletes()).toEqual([])

    await sheet.find('[data-test="cancel"]').trigger('click')
    await settle()
    expect(deletes()).toEqual([])
    expect(b.signedIn).toBe(true)
    expect(router.currentRoute.value.name).toBe('settings')

    // Positive control: confirming sends ?all=true and signs this device out.
    await wrapper.find('[data-test="sign-out-everywhere"]').trigger('click')
    await wrapper.find('[data-test="confirm"]').trigger('click')
    await settle()
    expect(deletes().map((r) => new URL(r.url).search)).toEqual(['?all=true'])
    expect(router.currentRoute.value.name).toBe('signin')
    expect(router.currentRoute.value.query.return).toBeUndefined()
    expect(useSessionStore().devices).toEqual([])
  })

  it('sign out of this device sends a plain DELETE and clears state', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper, router } = await mountApp('/settings')
    expect(useSessionStore().devices.length).toBe(3)

    await wrapper.find('[data-test="sign-out"]').trigger('click')
    await settle()
    const del = b.log.filter((r) => r.method === 'DELETE')
    expect(del.length).toBe(1)
    expect(new URL(del[0]!.url).search).toBe('')
    expect(router.currentRoute.value.name).toBe('signin')
    expect(useSessionStore().devices).toEqual([])
  })
})
