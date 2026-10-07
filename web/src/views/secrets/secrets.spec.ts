// The secrets screens (frontend §6.4, §2.5, design §10). Mounted as the whole
// app over the MSW backend, so the requests asserted on are the ones the
// shipping client builds. Every negative test carries its positive control in
// the same test: a screen that rendered nothing and sent nothing would pass
// every "never" here otherwise.

import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { FakeEventSource } from '../../test/fakeEventSource'
import { freshBackend, mountApp, server, settle, useMockApi } from '../../test/setup'
import { emit, recordSecretFetch, secretMeta, secretUndeliverable, WS_RUNNING, type MockBackend } from '../../mocks/backend'
import { sentenceFor } from '../../api/messages'

useMockApi()

type Mounted = Awaited<ReturnType<typeof mountApp>>

async function open(path: string, over: Partial<MockBackend> = {}): Promise<Mounted & { b: MockBackend; es: FakeEventSource }> {
  const b = freshBackend({ signedIn: true, ...over })
  const app = await mountApp(path)
  const es = FakeEventSource.latest().open().pipe(b)
  await settle()
  return { ...app, b, es }
}

const puts = (b: MockBackend) => b.secretBodies.filter((r) => r.method === 'PUT' && !r.url.endsWith('/grants'))
const grantPuts = (b: MockBackend) => b.secretBodies.filter((r) => r.method === 'PUT' && r.url.endsWith('/grants'))
const secretReads = (b: MockBackend) => b.log.filter((r) => r.method === 'GET' && r.url.endsWith('/api/secrets')).length
const rowNames = (w: Mounted['wrapper']) => w.findAll('[data-test="secret-name"]').map((n) => n.text())
const row = (w: Mounted['wrapper'], name: string) => w.findAll('[data-test="secret"]').find((r) => r.find('[data-test="secret-name"]').text() === name)!

/** A message's words, without the glyph that marks its tone (§7: never colour alone). */
const said = (el: { text(): string }) => el.text().replace(/^[×!✓]\s*/, '')

/** Every value an input or textarea holds right now: a DOM property, which innerHTML does not show. */
const fieldValues = () => [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')].map((f) => f.value)

function storageDump(s: Storage): string {
  const out: string[] = []
  for (let i = 0; i < s.length; i++) {
    const k = s.key(i)!
    out.push(k, s.getItem(k) ?? '')
  }
  return out.join('\n')
}

async function fillCreate(w: Mounted['wrapper'], name: string, value: string, reach = 'Read the staging database only.') {
  await w.find('[data-test="name"]').setValue(name)
  await w.find('[data-test="value"]').setValue(value)
  await w.find('[data-test="reach"]').setValue(reach)
}

describe('the secrets list', () => {
  it('shows name, reach, description, grants and last access — and never a value', async () => {
    const { wrapper, b } = await open('/secrets')
    expect(rowNames(wrapper)).toEqual(['NPM_READ_TOKEN', 'STAGING_DB_URL', 'STRIPE_TEST_KEY'])
    expect(row(wrapper, 'NPM_READ_TOKEN').find('[data-test="badge-all"]').text()).toBe('all repositories')
    expect(row(wrapper, 'STAGING_DB_URL').find('[data-test="badge-none"]').text()).toBe('granted to nothing')
    const stripe = row(wrapper, 'STRIPE_TEST_KEY')
    expect(stripe.find('[data-test="secret-reach"]').text()).toContain('Test mode only')
    expect(stripe.find('[data-test="secret-description"]').text()).toContain('Roll it there')
    expect(stripe.find('[data-test="secret-grants"]').text()).toBe('Granted to krelinga/drydock')
    expect(stripe.find('[data-test="secret-access"]').text()).toMatch(/^Last fetched 2 minutes ago by krelinga\/drydock$/)
    expect(row(wrapper, 'STAGING_DB_URL').find('[data-test="secret-access"]').text()).toBe('Never fetched by any workspace')

    // Never a value: the server holds three, and none is anywhere on the page.
    const values = Object.values(b.secrets).map((s) => s.value)
    expect(values.length).toBe(3)
    for (const v of values) expect(document.body.innerHTML).not.toContain(v)
    // And no affordance that implies one could be had (§2.5).
    expect(wrapper.findAll('input, textarea').length).toBe(0)
    expect(wrapper.text()).not.toMatch(/reveal|copy|show value|edit value|current value/i)
    // Control: the page did render, and offers the one thing it should.
    expect(wrapper.find('[data-test="new-secret"]').attributes('href')).toBe('/secrets/new')
  })

  it("renders another device's create and delete from the stream, with no refetch", async () => {
    const { wrapper, b } = await open('/secrets')
    const reads = secretReads(b)
    b.secrets.SENTRY_DSN = {
      name: 'SENTRY_DSN', value: 'https://mock@sentry.invalid/1', reach: 'Report errors to the test project.', description: '',
      all_repos: false, grants: [], created_at: new Date().toISOString(), rotated_at: null, last_access_at: null, accessed_by: [],
    }
    emit(b, 'secret.created', { data: { secret: secretMeta(b, b.secrets.SENTRY_DSN) } })
    await settle()
    expect(rowNames(wrapper)).toContain('SENTRY_DSN')
    delete b.secrets.STAGING_DB_URL
    emit(b, 'secret.deleted', { data: { name: 'STAGING_DB_URL' } })
    await settle()
    expect(rowNames(wrapper)).toEqual(['NPM_READ_TOKEN', 'SENTRY_DSN', 'STRIPE_TEST_KEY'])
    expect(secretReads(b)).toBe(reads)
  })

  it('refetches on reopen, which is the only way last access moves', async () => {
    const { wrapper, b, es } = await open('/secrets')
    const access = () => row(wrapper, 'STRIPE_TEST_KEY').find('[data-test="secret-access"]').text()
    expect(access()).toContain('2 minutes ago')
    recordSecretFetch(b, WS_RUNNING) // a secret_access row, and no event
    await settle()
    expect(access()).toContain('2 minutes ago')
    es.drop().open()
    await settle()
    expect(access()).toContain('just now')
  })

  it('says when no secrets key is configured, rather than showing an empty list', async () => {
    const { wrapper, b, es } = await open('/secrets', { secretsKey: false })
    expect(wrapper.find('[data-test="secrets-not-configured"]').text()).toContain('--secrets-key')
    expect(wrapper.find('[data-test="secrets-empty"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="new-secret"]').exists()).toBe(false)
    // Control: with a key, the reopen's refetch brings the list.
    b.secretsKey = true
    es.drop().open()
    await settle()
    expect(rowNames(wrapper).length).toBe(3)
  })

  it('an empty list says what a secret is, and offers to make one', async () => {
    const { wrapper } = await open('/secrets', { secrets: {} })
    expect(wrapper.find('[data-test="secrets-empty"]').text()).toContain('never shown again')
    expect(wrapper.find('[data-test="new-secret"]').exists()).toBe(true)
  })
})

describe('undeliverable secrets (frontend §4.5 #12)', () => {
  const banner = (w: Mounted['wrapper']) => w.find('[data-test="fleet-banner"]')
  const STRIPE_LOST = { since: new Date(Date.now() - 5 * 60e3).toISOString(), secrets: [{ name: 'STRIPE_TEST_KEY', reason: 'does_not_open' }] }

  it('is one banner on every screen, naming the secret and its repair, and a write that repairs nothing leaves it', async () => {
    const { wrapper, b, router } = await open('/')
    expect(banner(wrapper).exists()).toBe(false) // control: a healthy fleet has none
    secretUndeliverable(b)
    await settle()
    expect(banner(wrapper).attributes('role')).toBe('alert')
    expect(banner(wrapper).find('b').text()).toBe('Stored secrets cannot be delivered.')
    expect(banner(wrapper).text()).toContain("Every workspace's commands fail until this is fixed.")
    expect(wrapper.findAll('[data-test="fleet-item"]').map((i) => i.text()))
      .toEqual(["STRIPE_TEST_KEY: Drydock's secrets key cannot open it. Store its value again."])
    await router.push('/settings')
    await settle()
    expect(banner(wrapper).exists()).toBe(true)
    // A write is not a repair: the banner stays, unchanged, until the server says so.
    emit(b, 'secret.updated', { data: { secret: secretMeta(b, b.secrets.NPM_READ_TOKEN!) } })
    await settle()
    expect(banner(wrapper).classes()).toContain('bad')
    expect(wrapper.findAll('[data-test="fleet-item"]').length).toBe(1)
  })

  it('a reload shows a standing fault from GET /api/secrets, on a screen that is not about secrets', async () => {
    // Nothing on the stream: the fault predates this page, as after a reload.
    const { wrapper, b, es } = await open('/', { undeliverable: STRIPE_LOST })
    expect(b.events.some((e) => e.kind === 'secret.undeliverable')).toBe(false)
    expect(banner(wrapper).exists()).toBe(true)
    expect(banner(wrapper).text()).toContain('Since 5 minutes ago.')
    expect(wrapper.find('[data-test="fleet-item"]').text()).toContain('STRIPE_TEST_KEY')
    // Control: the snapshot decides both ways. Repaired while this page
    // missed the event (the server restarted, say), the next refetch clears it.
    b.undeliverable = null
    es.drop().open()
    await settle()
    expect(banner(wrapper).exists()).toBe(false)
  })

  it('storing the value again clears it, by secret.deliverable — on every device, not only the one that repaired it', async () => {
    const { wrapper, b, router } = await open('/secrets/STRIPE_TEST_KEY', { undeliverable: STRIPE_LOST })
    expect(banner(wrapper).exists()).toBe(true)
    await wrapper.find('[data-test="edit"]').trigger('click')
    await wrapper.find('[data-test="value"]').setValue('sk_test_restored')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(b.events.at(-1)?.kind).toBe('secret.deliverable')
    expect(banner(wrapper).exists()).toBe(false)
    await router.push('/')
    await settle()
    expect(banner(wrapper).exists()).toBe(false)
    // Control: break it again and it is back; the clear was the event, not a one-way latch.
    secretUndeliverable(b)
    await settle()
    expect(banner(wrapper).exists()).toBe(true)
  })

  it('a repair made on another device clears it here, with no refetch', async () => {
    const { wrapper, b } = await open('/', { undeliverable: STRIPE_LOST })
    expect(banner(wrapper).exists()).toBe(true)
    const reads = secretReads(b)
    b.undeliverable = null
    emit(b, 'secret.deliverable', { data: {} })
    await settle()
    expect(banner(wrapper).exists()).toBe(false)
    expect(secretReads(b)).toBe(reads)
  })
})

describe('the create form', () => {
  it('labels reach with the literal question, a multi-line field, rules beside it', async () => {
    const { wrapper } = await open('/secrets/new')
    const label = wrapper.find('[data-test="reach-label"]')
    expect(label.text()).toBe('What can someone do with this?')
    const reach = wrapper.find('[data-test="reach"]')
    expect(reach.element.tagName).toBe('TEXTAREA')
    expect(label.attributes('for')).toBe(reach.attributes('id'))
    // The question is the label, never a placeholder that vanishes on typing.
    expect(reach.attributes('placeholder')).toBeUndefined()
    expect(wrapper.findAll('[data-test="rules"] li').length).toBe(4)
    expect(wrapper.find('[data-test="rules"]').text()).toContain('Never store one that can spend money')
  })

  it('the value field starts empty and asks the browser to keep nothing', async () => {
    const { wrapper } = await open('/secrets/new')
    const v = wrapper.find('[data-test="value"]')
    expect(v.element.tagName).toBe('TEXTAREA') // an <input> strips a pasted newline silently
    expect((v.element as HTMLTextAreaElement).value).toBe('')
    expect(v.attributes()).toMatchObject({ autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off', autocorrect: 'off' })
    expect(v.attributes('name')).toBeUndefined()
    // No <form>: a native submission would GET the named fields into the URL.
    expect(document.querySelector('form')).toBeNull()
    expect(wrapper.find('[data-test="value-help"]').text()).toContain('Write-only')
  })

  it('refuses a newline before sending, in the server\'s words, and sends once it is fixed', async () => {
    const { wrapper, b, router } = await open('/secrets/new')
    await fillCreate(wrapper, 'TEST_DB', 'line one\nline two')
    const err = () => wrapper.find('[data-test="value-error"]')
    const errText = () => said(err())
    // At keystroke time, before any save.
    expect(errText()).toBe(sentenceFor('secret_value_control_character',
      'It contains a newline (U+000A) at byte 8. A multi-line credential, such as a PEM, goes in as base64.'))
    expect(errText()).not.toContain('line one')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b)).toEqual([])
    // Control: the same form, fixed, sends.
    await wrapper.find('[data-test="value"]').setValue('line one')
    expect(err().exists()).toBe(false)
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b).length).toBe(1)
    expect(router.currentRoute.value.fullPath).toBe('/secrets/TEST_DB')
  })

  it('says why a reserved name is reserved as it is typed, and sends nothing', async () => {
    const { wrapper, b } = await open('/secrets/new')
    await fillCreate(wrapper, 'DO_NOT_TRACK', 'v')
    expect(said(wrapper.find('[data-test="name-error"]')))
      .toBe('That name is reserved. Drydock refuses it because it disables Remote Control (design §2.1).')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b)).toEqual([])
    // Control: a name that is not reserved clears it and sends.
    await wrapper.find('[data-test="name"]').setValue('DO_TRACK_THIS')
    expect(wrapper.find('[data-test="name-error"]').exists()).toBe(false)
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b).map((p) => new URL(p.url).pathname)).toEqual(['/api/secrets/DO_TRACK_THIS'])
  })

  it('refuses a name that is already stored as it is typed, sends nothing, and offers that secret instead (#29)', async () => {
    const { wrapper, b, router } = await open('/secrets/new')
    const before = { ...b.secrets.STRIPE_TEST_KEY! }
    await fillCreate(wrapper, 'STRIPE_TEST_KEY', 'sk_test_replacement', 'Something else entirely.')
    expect(said(wrapper.find('[data-test="name-error"]'))).toBe(sentenceFor('secret_exists', ''))
    const link = wrapper.find('[data-test="edit-existing"] a')
    expect(link.attributes('href')).toBe('/secrets/STRIPE_TEST_KEY')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b)).toEqual([])
    expect(b.secrets.STRIPE_TEST_KEY).toEqual(before)
    // The offer leads to the secret's own page, where replacing is the labelled act.
    await link.trigger('click')
    await settle()
    expect(router.currentRoute.value.fullPath).toBe('/secrets/STRIPE_TEST_KEY')

    // Control: a new name is sent — once, as a create that cannot replace.
    const again = await open('/secrets/new')
    await fillCreate(again.wrapper, 'BRAND_NEW_KEY', 'fresh value')
    expect(again.wrapper.find('[data-test="name-error"]').exists()).toBe(false)
    expect(again.wrapper.find('[data-test="edit-existing"]').exists()).toBe(false)
    await again.wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(again.b).map((p) => [new URL(p.url).pathname, p.ifNoneMatch])).toEqual([['/api/secrets/BRAND_NEW_KEY', '*']])
    expect(again.b.secrets.BRAND_NEW_KEY?.value).toBe('fresh value')
  })

  it('a name another device stored after the list loaded is refused by the server, and nothing is replaced', async () => {
    const { wrapper, b } = await open('/secrets/new')
    await fillCreate(wrapper, 'RACED_KEY', 'mine')
    // Stored elsewhere a moment ago; this page's list has not heard yet.
    b.secrets.RACED_KEY = { ...b.secrets.STAGING_DB_URL!, name: 'RACED_KEY', value: 'theirs' }
    expect(wrapper.find('[data-test="name-error"]').exists()).toBe(false)
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b).length).toBe(1)
    expect(said(wrapper.find('[data-test="name-error"]'))).toBe(sentenceFor('secret_exists', ''))
    expect(wrapper.find('[data-test="edit-existing"] a').attributes('href')).toBe('/secrets/RACED_KEY')
    expect(b.secrets.RACED_KEY.value).toBe('theirs')
  })

  it('will not save without a reach', async () => {
    const { wrapper, b } = await open('/secrets/new')
    await fillCreate(wrapper, 'K', 'v', '   ')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(wrapper.find('[data-test="reach-error"]').text()).toContain('Say what someone could do with this secret')
    expect(puts(b)).toEqual([])
    await wrapper.find('[data-test="reach"]').setValue('Nothing outside the sandbox.')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(puts(b).length).toBe(1)
  })

  // The server stays the authority: every refusal it can send, forced on a
  // write the form passed, lands on its field in the sentence for its code.
  const refusals: Array<[code: string, status: number, field: string, detail?: string]> = [
    ['secret_name_invalid', 400, 'name-error'],
    ['secret_name_reserved', 400, 'name-error', 'Drydock refuses it because a rule the client does not know yet.'],
    ['secret_value_empty', 400, 'value-error'],
    ['secret_value_control_character', 400, 'value-error', 'It contains the control character U+0085 at byte 3. A multi-line credential, such as a PEM, goes in as base64.'],
    ['secret_value_too_long', 400, 'value-error'],
    ['secret_value_required', 400, 'value-error'],
    ['secret_reach_required', 400, 'reach-error'],
    ['secret_reach_too_long', 400, 'reach-error'],
    ['secret_description_too_long', 400, 'description-error'],
    ['secret_description_invalid', 400, 'description-error'],
    ['secrets_not_configured', 503, 'form-error'],
    ['bad_request', 400, 'form-error'],
    ['secret_exists', 412, 'name-error'],
  ]
  for (const [code, status, field, detail] of refusals) {
    it(`shows the server's ${code} on its field`, async () => {
      const { wrapper, b } = await open('/secrets/new')
      b.refuseNextSecret = { status, code, message: 'server prose the UI must not show', ...(detail ? { detail } : {}) }
      await fillCreate(wrapper, 'FINE_NAME', 'fine value')
      await wrapper.find('[data-test="save"]').trigger('click')
      await settle()
      // Control: it was the server that refused — the request went.
      expect(puts(b).length).toBe(1)
      const shown = said(wrapper.find(`[data-test="${field}"]`))
      expect(shown).toBe(sentenceFor(code, detail))
      expect(shown).not.toContain('server prose')
      if (detail) expect(shown).toContain(detail)
      expect(wrapper.text()).not.toContain('fine value')
    })
  }

  it('THE CANARY: a submitted value is nowhere afterwards — DOM, fields, stores, storage, history', async () => {
    const canary = `cnry_${crypto.randomUUID().replaceAll('-', '')}_Zq9`
    const { wrapper, b, pinia, router } = await open('/secrets/new')
    await fillCreate(wrapper, 'CANARY_KEY', canary)
    // While typing it is a field's value, and only that (§4.2).
    expect(fieldValues()).toContain(canary)
    expect(JSON.stringify(pinia.state.value)).not.toContain(canary)

    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()

    // Control: it was sent. The request body carried it, once.
    const sent = puts(b)
    expect(sent.length).toBe(1)
    expect(JSON.parse(sent[0]!.body)).toMatchObject({ value: canary })
    expect(router.currentRoute.value.fullPath).toBe('/secrets/CANARY_KEY')
    // ...and the secret it made is on screen, so the page did not merely go blank.
    expect(wrapper.find('[data-test="grants-none"]').exists()).toBe(true)

    // And now it is nowhere.
    expect(document.documentElement.outerHTML).not.toContain(canary)
    expect(fieldValues().join('\n')).not.toContain(canary)
    expect(JSON.stringify(pinia.state.value)).not.toContain(canary)
    expect(storageDump(localStorage)).not.toContain(canary)
    expect(storageDump(sessionStorage)).not.toContain(canary)
    expect(JSON.stringify(window.history.state)).not.toContain(canary)
    expect(window.location.href).not.toContain(canary)
    expect(b.log.map((r) => r.url).join('\n')).not.toContain(canary)
    // Nor on the stream: the server's events, which the reducer saw, carry metadata only.
    expect(JSON.stringify(b.events)).not.toContain(canary)

    // Opening the replace form on the same secret finds an empty field.
    await wrapper.find('[data-test="edit"]').trigger('click')
    expect((wrapper.find('[data-test="value"]').element as HTMLTextAreaElement).value).toBe('')
    expect(fieldValues().join('\n')).not.toContain(canary)
  })

  it('a value being typed does not survive leaving the page', async () => {
    const { wrapper, router } = await open('/secrets/new')
    await wrapper.find('[data-test="value"]').setValue('half-typed-value-123')
    expect(fieldValues()).toContain('half-typed-value-123') // control
    await router.push('/secrets')
    await settle()
    await router.push('/secrets/new')
    await settle()
    expect((wrapper.find('[data-test="value"]').element as HTMLTextAreaElement).value).toBe('')
    expect(document.documentElement.outerHTML + fieldValues().join()).not.toContain('half-typed-value-123')
    expect(storageDump(localStorage) + storageDump(sessionStorage)).not.toContain('half-typed-value-123')
  })
})

describe('replacing a value', () => {
  it('opens empty, says the current value is not shown, and pre-fills only the prose', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    expect(wrapper.find('[data-test="value"]').exists()).toBe(false) // no field until asked
    await wrapper.find('[data-test="edit"]').trigger('click')
    const value = wrapper.find('[data-test="value"]').element as HTMLTextAreaElement
    expect(value.value).toBe('')
    const help = wrapper.find('[data-test="value-help"]').text()
    expect(help).toContain('The current value is not shown. Leave this empty to keep it')
    // §4.5 #13 is closed: nothing asks for the current value to be typed again.
    expect(help).not.toMatch(/again|current value again|re-?enter/i)
    // Control: the form is pre-filled — with the reach, which is metadata.
    expect((wrapper.find('[data-test="reach"]').element as HTMLTextAreaElement).value).toBe(b.secrets.STRIPE_TEST_KEY!.reach)
    expect(fieldValues()).not.toContain(b.secrets.STRIPE_TEST_KEY!.value)
  })

  it('splits the stale workspaces into two kinds, with the restart warning on the second only', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    const rotate = async (v: string) => {
      await wrapper.find('[data-test="edit"]').trigger('click')
      await wrapper.find('[data-test="value"]').setValue(v)
      await wrapper.find('[data-test="save"]').trigger('click')
      await settle()
    }
    const section = (k: string) => wrapper.find(`[data-test="stale-${k}"]`)

    // Phase 4: every one picks it up on its next command; the second kind is
    // rendered, empty, and carries no warning.
    await rotate('sk_test_new_1')
    expect(section('new-commands').findAll('[data-test="stale-row"]').map((r) => r.text())).toEqual(['krelinga/drydock'])
    expect(section('new-commands').find('a').attributes('href')).toBe(`/ws/${WS_RUNNING}`)
    expect(section('needs-restart').find('[data-test="restart-none"]').text()).toBe('None.')
    expect(wrapper.find('[data-test="restart-warning"]').exists()).toBe(false)

    // Phase 5's hook says this one holds a frozen environment: it moves to
    // the second kind, and the warning appears there, attached to it.
    await wrapper.find('[data-test="dismiss"]').trigger('click')
    b.staleRestart = [WS_RUNNING]
    await rotate('sk_test_new_2')
    expect(section('needs-restart').findAll('[data-test="stale-row"]').map((r) => r.text())).toEqual(['krelinga/drydock'])
    expect(section('needs-restart').find('[data-test="restart-warning"]').text()).toContain('ends every session it is serving')
    expect(section('new-commands').text()).toContain('None.')
    expect(section('new-commands').find('[data-test="restart-warning"]').exists()).toBe(false)
  })

  it('changing only the reach sends no value key and is not a rotation; a value edit sends one', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    const stored = b.secrets.STRIPE_TEST_KEY!.value
    await wrapper.find('[data-test="edit"]').trigger('click')
    expect(wrapper.find('[data-test="save"]').text()).toBe('Save changes')
    await wrapper.find('[data-test="reach"]').setValue('Test mode charges only, in the sandbox account.')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    // No `value` key at all: not "", not null — absent is the request that means "keep".
    const sent = JSON.parse(puts(b)[0]!.body) as Record<string, unknown>
    expect(Object.keys(sent).sort()).toEqual(['description', 'reach'])
    expect(b.secrets.STRIPE_TEST_KEY!.value).toBe(stored)
    // No client-side "needs a value" on the way, and the result says the value was kept.
    expect(wrapper.find('[data-test="value-error"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="result-unchanged"]').text()).toContain('The value was kept as it is')
    expect(wrapper.find('[data-test="stale-new-commands"]').exists()).toBe(false)
    expect(b.events.at(-1)?.kind).toBe('secret.updated')
    expect(wrapper.find('[data-test="detail-reach"]').text()).toBe('Test mode charges only, in the sandbox account.')

    // Control: typing a value sends it, the button says so, and it is a rotation.
    await wrapper.find('[data-test="dismiss"]').trigger('click')
    await wrapper.find('[data-test="edit"]').trigger('click')
    await wrapper.find('[data-test="value"]').setValue('sk_test_rotated')
    expect(wrapper.find('[data-test="save"]').text()).toBe('Replace value')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(JSON.parse(puts(b)[1]!.body)).toMatchObject({ value: 'sk_test_rotated' })
    // An edit is the replace: it is not sent as a create (#29), and it replaces.
    expect(puts(b).map((p) => p.ifNoneMatch)).toEqual([null, null])
    expect(b.secrets.STRIPE_TEST_KEY!.value).toBe('sk_test_rotated')
    expect(b.events.at(-1)?.kind).toBe('secret.rotated')
    expect(wrapper.find('[data-test="stale-new-commands"]').exists()).toBe(true)
  })

  it('a new secret still needs a value: the create form refuses an empty one before sending', async () => {
    const { wrapper, b } = await open('/secrets/new')
    await fillCreate(wrapper, 'NEW_KEY', '')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(said(wrapper.find('[data-test="value-error"]'))).toBe(sentenceFor('secret_value_empty'))
    expect(puts(b)).toEqual([])
    // Control: with a value it sends, and the value is in the body.
    await wrapper.find('[data-test="value"]').setValue('v')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(JSON.parse(puts(b)[0]!.body)).toMatchObject({ value: 'v' })
  })

  it('the same value again is not a rotation, and nothing is stale', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    await wrapper.find('[data-test="edit"]').trigger('click')
    await wrapper.find('[data-test="value"]').setValue(b.secrets.STRIPE_TEST_KEY!.value)
    await wrapper.find('[data-test="reach"]').setValue('Test mode charges only.')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    expect(wrapper.find('[data-test="result-unchanged"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="stale-new-commands"]').exists()).toBe(false)
    // The reach moved, by its event.
    expect(wrapper.find('[data-test="detail-reach"]').text()).toBe('Test mode charges only.')
  })

  it('applies nothing from the 200 body: the secret changes when its event arrives', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    server.use(http.put('/api/secrets/:name', () => HttpResponse.json({
      created: false, rotated: true, stale: { new_commands: [], needs_supervisor_restart: [] },
      secret: { ...secretMeta(b, b.secrets.STRIPE_TEST_KEY!), reach: 'FROM-THE-BODY' },
    })))
    await wrapper.find('[data-test="edit"]').trigger('click')
    await wrapper.find('[data-test="value"]').setValue('sk_test_x')
    await wrapper.find('[data-test="save"]').trigger('click')
    await settle()
    // The operation's result is shown...
    expect(wrapper.find('[data-test="save-result"]').exists()).toBe(true)
    // ...but the entity did not take the body's reach.
    expect(wrapper.find('[data-test="detail-reach"]').text()).not.toContain('FROM-THE-BODY')
    // Control: the event is what moves it.
    emit(b, 'secret.rotated', { data: { secret: { ...secretMeta(b, b.secrets.STRIPE_TEST_KEY!), reach: 'FROM-THE-EVENT' } } })
    await settle()
    expect(wrapper.find('[data-test="detail-reach"]').text()).toBe('FROM-THE-EVENT')
  })
})

describe('grants', () => {
  it('a secret with no grants says nothing receives it', async () => {
    const { wrapper } = await open('/secrets/STAGING_DB_URL')
    expect(wrapper.find('[data-test="grants-none"]').text())
      .toBe('Granted to nothing. No workspace receives this secret until you grant it a repository.')
    expect(wrapper.find('[data-test="grants-all"]').exists()).toBe(false)
  })

  it('sends exactly the chosen repositories, and says nothing needs restarting', async () => {
    const { wrapper, b } = await open('/secrets/STAGING_DB_URL')
    await wrapper.find('[data-test="grants-edit"]').trigger('click')
    const box = (id: number) => wrapper.find(`[data-test="grant-choice"][data-repo="${id}"]`)
    await box(2).setValue(true)
    await box(3).setValue(true)
    await box(3).setValue(false)
    await wrapper.find('[data-test="grants-save"]').trigger('click')
    await settle()
    expect(grantPuts(b).map((p) => JSON.parse(p.body))).toEqual([{ repository_ids: [2], all_repos: false }])
    expect(wrapper.find('[data-test="grants-list"]').text()).toBe('krelinga/homelab')
    expect(wrapper.find('[data-test="grants-none"]').exists()).toBe(false)
    // A grant is not a rotation (§6.4): no stale list, no restart.
    expect(wrapper.find('[data-test="grants-no-restart"]').text()).toContain('Nothing needs restarting')
    expect(wrapper.find('[data-test="save-result"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="restart-warning"]').exists()).toBe(false)
  })

  it('all repositories takes a confirm that names the count and the repositories added later', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    await wrapper.find('[data-test="grants-all-ask"]').trigger('click')
    const sheet = wrapper.find('[data-test="confirm-all"]')
    expect(sheet.text()).toContain('all 5 repositories at once')
    expect(sheet.text()).toContain('every repository added to the installation later')
    // Asking is not acting, and cancel sends nothing.
    expect(grantPuts(b)).toEqual([])
    await sheet.find('[data-test="confirm-all-cancel"]').trigger('click')
    await settle()
    expect(grantPuts(b)).toEqual([])
    expect(wrapper.find('[data-test="grants-all"]').exists()).toBe(false)
    // Control: confirming sends all_repos, keeping the explicit grant.
    await wrapper.find('[data-test="grants-all-ask"]').trigger('click')
    await wrapper.find('[data-test="confirm-all-yes"]').trigger('click')
    await settle()
    expect(grantPuts(b).map((p) => JSON.parse(p.body))).toEqual([{ repository_ids: [1], all_repos: true }])
    expect(wrapper.find('[data-test="grants-all"]').text()).toContain('All repositories.')
  })

  it("shows the server's unknown_repository with its detail", async () => {
    const { wrapper, b } = await open('/secrets/STAGING_DB_URL')
    b.repos = b.repos.filter((r) => r.id !== 2) // gone from the server's catalog, still on screen
    await wrapper.find('[data-test="grants-edit"]').trigger('click')
    await wrapper.find('[data-test="grant-choice"][data-repo="2"]').setValue(true)
    await wrapper.find('[data-test="grants-save"]').trigger('click')
    await settle()
    expect(grantPuts(b).length).toBe(1)
    expect(said(wrapper.find('[data-test="grants-error"]')))
      .toBe('That repository is not in the catalog. No repository has id 2. Refresh the repository list and try again.')
    expect(wrapper.find('[data-test="grants-editor"]').exists()).toBe(true) // the draft is kept to fix
  })
})

describe('delete', () => {
  it('says it removes the value and every grant; cancel sends nothing, confirm deletes', async () => {
    const { wrapper, b, router } = await open('/secrets/STRIPE_TEST_KEY')
    const deletes = () => b.secretBodies.filter((r) => r.method === 'DELETE')
    await wrapper.find('[data-test="delete"]').trigger('click')
    expect(wrapper.find('[data-test="delete-says"]').text()).toContain('removes the stored value and every grant')
    expect(deletes()).toEqual([])
    await wrapper.find('[data-test="delete-cancel"]').trigger('click')
    await settle()
    expect(deletes()).toEqual([])
    expect(b.secrets.STRIPE_TEST_KEY).toBeDefined()
    // Control.
    await wrapper.find('[data-test="delete"]').trigger('click')
    await wrapper.find('[data-test="delete-confirm"]').trigger('click')
    await settle()
    expect(deletes().map((d) => new URL(d.url).pathname)).toEqual(['/api/secrets/STRIPE_TEST_KEY'])
    expect(router.currentRoute.value.name).toBe('secrets')
    expect(rowNames(wrapper)).toEqual(['NPM_READ_TOKEN', 'STAGING_DB_URL'])
  })

  it('a secret deleted on another device shows as gone, and its form goes with it', async () => {
    const { wrapper, b } = await open('/secrets/STRIPE_TEST_KEY')
    await wrapper.find('[data-test="edit"]').trigger('click')
    await wrapper.find('[data-test="value"]').setValue('typed-before-delete')
    delete b.secrets.STRIPE_TEST_KEY
    emit(b, 'secret.deleted', { data: { name: 'STRIPE_TEST_KEY' } })
    await settle()
    expect(wrapper.find('[data-test="secret-missing"]').text()).toContain('There is no secret named STRIPE_TEST_KEY')
    expect(fieldValues().join()).not.toContain('typed-before-delete')
  })
})
