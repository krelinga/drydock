// The 401 path end to end in the DOM (frontend §2.2, §4.4): the boot probe,
// and a session that lapses mid-use.

import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { freshBackend, mountApp, settle, server, useMockApi } from './test/setup'
import { handlersFor } from './mocks/backend'
import { useSessionStore } from './stores/session'
import { get, send } from './api/client'

useMockApi()

describe('boot', () => {
  it('sends a signed-out visitor to /signin carrying where they were going', async () => {
    freshBackend({ signedIn: false })
    const { router, wrapper } = await mountApp('/settings')
    expect(router.currentRoute.value.name).toBe('signin')
    expect(router.currentRoute.value.query.return).toBe('/settings')
    // No shell for a signed-out visitor: no nav, no device list.
    expect(wrapper.find('nav[aria-label="Main"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="devices"]').exists()).toBe(false)
  })

  it('lets a signed-in visitor stay where they asked to be', async () => {
    freshBackend({ signedIn: true })
    const { router, wrapper } = await mountApp('/settings')
    expect(router.currentRoute.value.name).toBe('settings')
    expect(wrapper.find('nav[aria-label="Main"]').exists()).toBe(true)
    expect(wrapper.findAll('[data-test="device"]').length).toBe(3)
  })

  it('does not mistake an unreachable server for a signed-out one', async () => {
    const b = freshBackend({ signedIn: true })
    // The probe fails at the network: say so, keep the route, offer retry.
    server.use(http.get('/api/auth/session', () => HttpResponse.error()))
    const { router, wrapper } = await mountApp('/settings')
    expect(router.currentRoute.value.name).toBe('settings')
    expect(wrapper.text()).toContain('Could not reach Drydock')
    // Control: once the server answers, Try again brings the page back.
    server.resetHandlers(...handlersFor(b))
    await wrapper.find('button').trigger('click')
    await settle()
    expect(wrapper.text()).not.toContain('Could not reach Drydock')
    expect(wrapper.findAll('[data-test="device"]').length).toBe(3)
  })
})

describe('a session that lapses mid-use', () => {
  it('clears entity state and lands on /signin with return, not on stale data', async () => {
    const b = freshBackend({ signedIn: true })
    const { router, wrapper, pinia } = await mountApp('/settings')
    const session = useSessionStore(pinia)

    // Positive control: signed in, devices on screen and in the store.
    expect(router.currentRoute.value.fullPath).toBe('/settings')
    expect(session.devices.length).toBe(3)
    expect(wrapper.findAll('[data-test="device"]').length).toBe(3)

    // The 14-day idle window lapses in a background tab. The next request —
    // any request — comes back 401.
    b.signedIn = false
    await get('/api/no-such-route').catch(() => undefined)
    await settle()

    expect(router.currentRoute.value.name).toBe('signin')
    expect(router.currentRoute.value.query.return).toBe('/settings')
    expect(session.status).toBe('signed-out')
    expect(session.devices).toEqual([])
    expect(session.current).toBe('')
    expect(wrapper.find('[data-test="devices"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Mac Firefox')
  })

  it('a bad password on /signin does not bounce the sign-in page', async () => {
    freshBackend({ signedIn: false })
    const { router } = await mountApp('/signin?return=/settings')
    const before = router.currentRoute.value.fullPath
    // A bad-password 401 goes through the same handler; it must not rewrite
    // `return` to point at /signin itself.
    await send('POST', '/api/auth/session', { password: 'wrong' }).catch(() => undefined)
    await settle()
    expect(router.currentRoute.value.fullPath).toBe(before)
    // Control: the handler did run — state is signed-out, not unknown.
    expect(useSessionStore().status).toBe('signed-out')
  })
})
