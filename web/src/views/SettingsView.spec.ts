import { describe, expect, it } from 'vitest'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { useSessionStore } from '../stores/session'

useMockApi()

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
