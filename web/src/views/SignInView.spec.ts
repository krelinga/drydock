import { describe, expect, it, vi } from 'vitest'
import type { VueWrapper } from '@vue/test-utils'
import { http, HttpResponse } from 'msw'
import { freshBackend, mountApp, settle, server, useMockApi } from '../test/setup'
import { MOCK_PASSWORD } from '../mocks/backend'
import { navigation } from '../lib/returnPath'

useMockApi()

async function attempt(wrapper: VueWrapper, password: string): Promise<void> {
  await wrapper.find('#signin-password').setValue(password)
  await wrapper.find('form').trigger('submit')
  await settle()
}

describe('SignInView', () => {
  it('says a wrong password is wrong, then signs in and follows return', async () => {
    const b = freshBackend()
    const { wrapper, router } = await mountApp('/signin?return=/settings')

    await attempt(wrapper, 'not it')
    expect(wrapper.find('[role="alert"]').text()).toContain('That password is not right.')
    expect(router.currentRoute.value.name).toBe('signin')
    expect(b.signedIn).toBe(false)

    // Positive control: the right password lands on the requested page.
    await attempt(wrapper, MOCK_PASSWORD)
    expect(router.currentRoute.value.fullPath).toBe('/settings')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('never follows a return that leaves the origin', async () => {
    freshBackend()
    const { wrapper, router } = await mountApp('/signin?return=//evil.example/x')
    await attempt(wrapper, MOCK_PASSWORD)
    expect(router.currentRoute.value.fullPath).toBe('/')

    // Control, same test: a same-origin return is followed.
    freshBackend()
    const second = await mountApp('/signin?return=/secrets')
    await attempt(second.wrapper, MOCK_PASSWORD)
    // /secrets is a lazy chunk, and its first import is compiled on demand —
    // slower than settle()'s ticks on a cold CI runner. Wait for the
    // navigation itself rather than for a fixed number of ticks.
    await vi.waitFor(() => expect(second.router.currentRoute.value.fullPath).toBe('/secrets'))
  })

  it('loads a return the server answers — the preview handshake — rather than routing to it', async () => {
    freshBackend()
    const assign = vi.spyOn(navigation, 'assign').mockImplementation(() => {})
    try {
      const authorize = '/preview/authorize?return=https%3A%2F%2Fmyapp-5173-p2mq.drydock-preview.test%2Fpage'
      const { wrapper, router } = await mountApp(`/signin?return=${encodeURIComponent(authorize)}`)
      await attempt(wrapper, MOCK_PASSWORD)
      expect(assign).toHaveBeenCalledWith(authorize)
      expect(router.currentRoute.value.name).toBe('signin')

      // Control, same test: an app path is routed, not loaded.
      assign.mockClear()
      freshBackend()
      const second = await mountApp('/signin?return=/settings')
      await attempt(second.wrapper, MOCK_PASSWORD)
      expect(second.router.currentRoute.value.fullPath).toBe('/settings')
      expect(assign).not.toHaveBeenCalled()
    } finally {
      assign.mockRestore()
    }
  })

  it('reports a lockout with its wait, from the code and Retry-After', async () => {
    freshBackend()
    server.use(
      http.post('/api/auth/session', () =>
        HttpResponse.json(
          { error: { code: 'locked_out', message: 'ZZZ prose' } },
          { status: 429, headers: { 'Retry-After': '120' } },
        ),
      ),
    )
    const { wrapper, router } = await mountApp('/signin')
    await attempt(wrapper, MOCK_PASSWORD)
    const alert = wrapper.find('[role="alert"]').text()
    expect(alert).toContain('Too many failed sign-ins. Try again in 2 minutes.')
    expect(alert).not.toContain('ZZZ')
    expect(router.currentRoute.value.name).toBe('signin')
  })

  it('locks out after repeated failures, as the server does', async () => {
    freshBackend()
    const { wrapper } = await mountApp('/signin')
    for (let i = 0; i < 5; i++) {
      await attempt(wrapper, `wrong ${i}`)
      expect(wrapper.find('[role="alert"]').text()).toContain('That password is not right.')
    }
    await attempt(wrapper, MOCK_PASSWORD)
    expect(wrapper.find('[role="alert"]').text()).toMatch(/Too many failed sign-ins\. Try again in \d+ minutes?\./)
  })

  it('never submits natively, and the password field carries no name', async () => {
    freshBackend()
    const { wrapper } = await mountApp('/signin')
    const form = wrapper.find('form')
    // No action/method a native submission could follow, and no `name` for
    // it to serialise: a dead script cannot leak the password into a URL.
    expect(form.attributes('action')).toBeUndefined()
    expect(wrapper.find('#signin-password').attributes('name')).toBeUndefined()
    expect(wrapper.find('#signin-password').attributes('type')).toBe('password')
    // Control: the submit handler is attached — a submit event signs in.
    await attempt(wrapper, MOCK_PASSWORD)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })
})
