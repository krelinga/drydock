import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import type { VueWrapper } from '@vue/test-utils'
import { freshBackend, mountApp, server, settle, useMockApi } from '../test/setup'
import { useStreamStore } from '../stores/stream'
import { emit } from '../mocks/backend'
import { FakeEventSource } from '../test/fakeEventSource'
import { emptyEntities, reduce, tokenList } from '../stores/reducer'

useMockApi()

// Settings → API tokens (vscode-extension §4.2): create with approve off by
// default, the once-only display, the list from the stream, revoke settled by
// its event.

const section = (w: VueWrapper) => w.find('[data-test="api-tokens"]')

async function open(over: Parameters<typeof freshBackend>[0] = {}) {
  const b = freshBackend({ signedIn: true, ...over })
  const m = await mountApp('/settings')
  FakeEventSource.latest().open().pipe(b)
  await settle()
  return { b, ...m }
}

function storageDump(s: Storage): string {
  const out: string[] = []
  for (let i = 0; i < s.length; i++) {
    const k = s.key(i)!
    out.push(`${k}=${s.getItem(k)}`)
  }
  return out.join('\n')
}

describe('Settings → API tokens', () => {
  it('the form defaults to read and lifecycle, approve off, 90 days, a label naming this device', async () => {
    const { wrapper } = await open()
    await section(wrapper).find('[data-test="token-new"]').trigger('click')
    const checked = (s: string) => (section(wrapper).find(`[data-test="token-scope-${s}"] input`).element as HTMLInputElement).checked
    expect(checked('read')).toBe(true)
    expect(checked('lifecycle')).toBe(true)
    expect(checked('approve')).toBe(false)
    // read is every token's: shown on, and not something to untick.
    expect(section(wrapper).find('[data-test="token-scope-read"] input').attributes('disabled')).toBeDefined()
    expect(section(wrapper).find('[data-test="token-scope-lifecycle"] input').attributes('disabled')).toBeUndefined()
    expect((section(wrapper).find('[data-test="token-days"]').element as HTMLSelectElement).value).toBe('90')
    // The sample's current device is "iPhone Safari".
    expect((section(wrapper).find('[data-test="token-label"]').element as HTMLInputElement).value).toBe('VS Code on iPhone')
    // The card's sentence about what approving means sits beside it.
    expect(section(wrapper).find('[data-test="token-approve-warning"]').text()).toContain('as the drydock user or as root')
  })

  it('creates a token, shows it once, and the row comes from the event — not the 201', async () => {
    const { wrapper, b, pinia } = await open()
    const stream = useStreamStore(pinia)
    await section(wrapper).find('[data-test="token-new"]').trigger('click')
    await section(wrapper).find('[data-test="token-label"]').setValue('  Laptop  ')
    await section(wrapper).find('[data-test="token-scope-approve"] input').setValue(true)
    await section(wrapper).find('[data-test="token-days"]').setValue('30')
    await section(wrapper).find('[data-test="token-create"]').trigger('click')
    await settle()

    const posts = b.log.filter((r) => r.method === 'POST' && r.url.endsWith('/api/tokens'))
    expect(posts.length).toBe(1)
    const [id, value] = Object.entries(b.tokenValues)[0]!
    expect(b.tokens[id]!.label).toBe('Laptop')
    expect(b.tokens[id]!.scopes).toEqual(['read', 'lifecycle', 'approve'])
    expect(Date.parse(b.tokens[id]!.expires_at) - Date.parse(b.tokens[id]!.created_at)).toBe(30 * 86400e3)
    // Shown once.
    expect(section(wrapper).find('[data-test="token-value"]').text()).toBe(value)
    // The row: from api_token.created, with no token in it.
    const rows = section(wrapper).findAll('[data-test="token"]')
    expect(rows.length).toBe(1)
    expect(rows[0]!.find('[data-test="token-row-label"]').text()).toBe('Laptop')
    expect(rows[0]!.find('[data-test="token-scopes"]').text()).toBe('read · lifecycle · approve')
    expect(JSON.stringify(stream.entities.tokens)).not.toContain(value)

    // Done: the value is gone from the page.
    await section(wrapper).find('[data-test="token-done"]').trigger('click')
    await settle()
    expect(section(wrapper).find('[data-test="token-value"]').exists()).toBe(false)
    expect(document.documentElement.outerHTML).not.toContain(value)
  })

  it('THE CANARY: a minted token is nowhere but its one display — stores, storage, history, URLs, the stream, the entities', async () => {
    const { wrapper, b, pinia, router } = await open()
    await section(wrapper).find('[data-test="token-new"]').trigger('click')
    await section(wrapper).find('[data-test="token-create"]').trigger('click')
    await settle()
    const value = Object.values(b.tokenValues)[0]!
    const random = value.slice('drydock_'.length)
    // Control: it was minted and is on screen.
    expect(section(wrapper).find('[data-test="token-value"]').text()).toBe(value)
    for (const part of [value, random]) {
      expect(JSON.stringify(pinia.state.value)).not.toContain(part)
      expect(storageDump(localStorage)).not.toContain(part)
      expect(storageDump(sessionStorage)).not.toContain(part)
      expect(JSON.stringify(window.history.state)).not.toContain(part)
      expect(window.location.href).not.toContain(part)
      expect(b.log.map((r) => r.url).join('\n')).not.toContain(part)
      expect(JSON.stringify(b.events)).not.toContain(part)
    }
    // Leaving the page ends the display: nothing brings it back.
    await router.push('/')
    await settle()
    await router.push('/settings')
    await settle()
    expect(document.documentElement.outerHTML).not.toContain(value)
    expect(section(wrapper).findAll('[data-test="token"]').length).toBe(1) // control: the row is still listed
  })

  it('the 201 body is never applied: a row only the response names never appears', async () => {
    const { wrapper, pinia } = await open()
    server.use(http.post('/api/tokens', () => HttpResponse.json({
      id: 'tok_only_in_the_body', label: 'phantom', scopes: ['read'], created_at: new Date().toISOString(),
      last_used_at: null, expires_at: new Date(Date.now() + 86400e3).toISOString(), revoked_at: null,
      token: 'drydock_' + 'x'.repeat(43),
    }, { status: 201 })))
    await section(wrapper).find('[data-test="token-new"]').trigger('click')
    await section(wrapper).find('[data-test="token-create"]').trigger('click')
    await settle()
    // Control: the display shows what the response carried.
    expect(section(wrapper).find('[data-test="token-value"]').exists()).toBe(true)
    expect(useStreamStore(pinia).entities.tokens['tok_only_in_the_body']).toBeUndefined()
    expect(section(wrapper).text()).not.toContain('phantom')
  })

  it('revoke is in flight until api_token.revoked, not the 202', async () => {
    const { wrapper, b } = await open()
    await section(wrapper).find('[data-test="token-new"]').trigger('click')
    await section(wrapper).find('[data-test="token-create"]').trigger('click')
    await settle()
    await section(wrapper).find('[data-test="token-done"]').trigger('click')
    const id = Object.keys(b.tokens)[0]!
    // A 202 with no event behind it: the mark must stay.
    server.use(http.delete('/api/tokens/:id', () => HttpResponse.json({}, { status: 202 })))
    const revoke = () => section(wrapper).find(`[data-test="token-revoke-${id}"] [data-test="action"]`)
    await revoke().trigger('click')
    await settle()
    expect(revoke().attributes('disabled')).toBeDefined()
    expect(section(wrapper).find('[data-test="token-revoked"]').exists()).toBe(false)

    // The event lands (from another path, as a second device's would).
    b.tokens[id]!.revoked_at = new Date().toISOString()
    emit(b, 'api_token.revoked', { data: { token: { ...b.tokens[id]! } } })
    await settle()
    expect(section(wrapper).find('[data-test="token-revoked"]').exists()).toBe(true)
    expect(revoke().exists()).toBe(false)
  })

  it('a token minted on another device appears from the stream', async () => {
    const { wrapper, b } = await open()
    expect(section(wrapper).find('[data-test="tokens-none"]').exists()).toBe(true)
    const meta = {
      id: 'tok_elsewhere', label: 'Desktop', scopes: ['read'], created_at: new Date().toISOString(),
      last_used_at: null, expires_at: new Date(Date.now() + 86400e3).toISOString(), revoked_at: null,
    }
    b.tokens[meta.id] = meta
    emit(b, 'api_token.created', { data: { token: meta } })
    await settle()
    expect(section(wrapper).findAll('[data-test="token"]').map((r) => r.find('[data-test="token-row-label"]').text())).toEqual(['Desktop'])
  })
})

describe('the token reducer', () => {
  const meta = (id: string, over: Record<string, unknown> = {}) => ({
    id, label: id, scopes: ['read', 'bogus', 'session'], created_at: '2026-10-13T12:00:00Z',
    last_used_at: null, expires_at: '2027-01-11T12:00:00Z', revoked_at: null, token: 'drydock_leak', ...over,
  })
  const ev = (id: number, kind: string, token: unknown) => ({ type: 'event' as const, event: { id, kind, level: 'info' as const, message: '', at: '', data: { token } } })

  it('keeps named fields only, versions by event id, and never un-revokes', () => {
    let e = reduce(emptyEntities(), ev(5, 'api_token.created', meta('tok_a')))
    expect(e.tokens['tok_a']!.scopes).toEqual(['read'])
    expect(JSON.stringify(e)).not.toContain('drydock_leak')
    e = reduce(e, ev(6, 'api_token.revoked', meta('tok_a', { revoked_at: '2026-10-13T13:00:00Z' })))
    expect(e.tokens['tok_a']!.revokedAt).toBe('2026-10-13T13:00:00Z')
    // An older event, or a later one without the revocation, changes nothing.
    expect(reduce(e, ev(5, 'api_token.created', meta('tok_a')))).toBe(e)
    expect(reduce(e, ev(7, 'api_token.created', meta('tok_a'))).tokens['tok_a']!.revokedAt).toBe('2026-10-13T13:00:00Z')
  })

  it('a list applies at its position: newer events win, newer tokens stay, the list is the rest', () => {
    let e = reduce(emptyEntities(), ev(10, 'api_token.revoked', meta('tok_a', { revoked_at: '2026-10-13T13:00:00Z' })))
    e = reduce(e, ev(11, 'api_token.created', meta('tok_new')))
    e = reduce(e, { type: 'tokens', at: 8, view: { tokens: [meta('tok_a'), meta('tok_old')] } })
    expect(e.tokensLoaded).toBe(true)
    expect(e.tokens['tok_a']!.revokedAt).not.toBeNull()
    expect(tokenList(e).map((t) => t.id).sort()).toEqual(['tok_a', 'tok_new', 'tok_old'])
  })
})
