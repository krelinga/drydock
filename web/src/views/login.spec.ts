// The login handshake view (frontend §6.2, design §7.2), through the whole
// app over the MSW backend: Settings' Claude section, the fleet banner, the
// stream. The state is the server's — every phase arrives as auth.login —
// so each test drives the mock's login and asserts what the page shows, and
// every negative assertion carries its positive control.

import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import {
  endLogin, identityView, loginReady, loginVerdict, MOCK_LOGIN_URL, type MockBackend,
} from '../mocks/backend'
import { LOGIN_BEGIN_KEY, LOGIN_CODE_KEY } from '../stores/identity'
import { useStreamStore } from '../stores/stream'

useMockApi()

type Mounted = Awaited<ReturnType<typeof mountApp>>

async function open(over: Partial<MockBackend> = {}, path = '/settings'): Promise<Mounted & { b: MockBackend; es: FakeEventSource }> {
  const b = freshBackend({ signedIn: true, identity: identityView('absent'), ...over })
  const app = await mountApp(path)
  const es = FakeEventSource.latest().open().pipe(b)
  await settle()
  return { ...app, b, es }
}

const section = (w: Mounted['wrapper']) => w.find('[data-test="claude-login"]')
const startButton = (w: Mounted['wrapper']) =>
  section(w).findAll('[data-test="action"]').find((e) => /Sign in|Start over/.test(e.text()))
const button = (w: Mounted['wrapper'], label: string) =>
  section(w).findAll('[data-test="action"]').find((e) => e.text() === label)
const fieldValues = () => [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')].map((f) => f.value)

function storageDump(s: Storage): string {
  const out: string[] = []
  for (let i = 0; i < s.length; i++) {
    const k = s.key(i)!
    out.push(`${k}=${s.getItem(k)}`)
  }
  return out.join('\n')
}

async function startAndWait(w: Mounted['wrapper'], b: MockBackend): Promise<void> {
  await startButton(w)!.trigger('click')
  await settle()
  if (b.loginMode === 'manual') {
    loginReady(b)
    await settle()
  }
}

async function typeAndSubmit(w: Mounted['wrapper'], code: string): Promise<void> {
  await section(w).find('[data-test="login-code"]').setValue(code)
  await button(w, 'Submit code')!.trigger('click')
  await settle()
}

describe('starting a sign-in', () => {
  it('absent: one Sign in to Claude, which starts a login that is in flight until the link is up', async () => {
    const { wrapper, b, pinia } = await open({ loginMode: 'manual' })
    const stream = useStreamStore(pinia)
    expect(startButton(wrapper)!.text()).toBe('Sign in to Claude')
    expect(section(wrapper).text()).toContain('One sign-in covers every workspace')

    await startButton(wrapper)!.trigger('click')
    await settle()
    // The receipt (starting) is on screen, and the button is still in flight.
    expect(wrapper.find('[data-test="login-starting"]').exists()).toBe(true)
    expect(LOGIN_BEGIN_KEY in stream.inFlight).toBe(true)
    loginReady(b)
    await settle()
    expect(LOGIN_BEGIN_KEY in stream.inFlight).toBe(false)
    const link = section(wrapper).find('[data-test="login-url"]')
    expect(link.attributes('href')).toBe(MOCK_LOGIN_URL)
    expect(link.attributes('target')).toBe('_blank')
    expect(link.attributes('rel')).toBe('noopener noreferrer')
    expect(section(wrapper).find('[data-test="login-copy"]').exists()).toBe(true)
    expect(section(wrapper).find('[data-test="login-deadline"]').text()).toMatch(/Time left: [45]:\d\d/)
    expect(section(wrapper).text()).toContain('every workspace')
  })

  it('the code field: no form, no autofill, no capitalisation, no spellcheck', async () => {
    const { wrapper, b } = await open()
    await startAndWait(wrapper, b)
    const field = section(wrapper).find('[data-test="login-code"]')
    expect(field.exists()).toBe(true) // control: there is a field
    const el = field.element as HTMLInputElement
    expect(el.closest('form')).toBeNull()
    expect(el.getAttribute('autocomplete')).toBe('off')
    expect(el.getAttribute('autocapitalize')).toBe('off')
    expect(el.getAttribute('spellcheck')).toBe('false')
    expect(el.getAttribute('name')).toBeNull()
  })
})

describe('the code', () => {
  it('a wrong code is a loop: the field opens empty beside the error, same link, countdown running; then the right one signs in', async () => {
    const { wrapper, b, pinia } = await open({ loginMode: 'manual' })
    const stream = useStreamStore(pinia)
    await startAndWait(wrapper, b)

    await typeAndSubmit(wrapper, 'wrongcode#mockstate')
    // Cleared on submit, before any verdict.
    expect((section(wrapper).find('[data-test="login-code"]').element as HTMLInputElement).value).toBe('')
    expect(LOGIN_CODE_KEY in stream.inFlight).toBe(true)
    expect(section(wrapper).find('[data-test="login-checking"]').exists()).toBe(true)
    loginVerdict(b)
    await settle()
    expect(LOGIN_CODE_KEY in stream.inFlight).toBe(false)
    expect(section(wrapper).find('[data-test="login-invalid"]').text()).toContain('Paste it again: the link is still valid.')
    expect(section(wrapper).find('[data-test="login-url"]').attributes('href')).toBe(MOCK_LOGIN_URL)
    expect(section(wrapper).find('[data-test="login-deadline"]').exists()).toBe(true)
    expect((section(wrapper).find('[data-test="login-code"]').element as HTMLInputElement).disabled).toBe(false)

    await typeAndSubmit(wrapper, '  mockcode123#mockstate \n')
    loginVerdict(b)
    await settle()
    expect(section(wrapper).find('[data-test="login-succeeded"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="claude-state"]').text()).toBe('Signed in as operator@example.invalid.')
    // Sent trimmed, as the server types it.
    expect(JSON.parse(b.loginBodies[1]!.body)).toEqual({ code: 'mockcode123#mockstate' })
    // The fleet's fault is gone with it: no banner says "no one has signed in".
    expect(wrapper.findAll('[data-test="fleet-banner"]').filter((e) => (e.attributes('data-banner') ?? '').startsWith('identity')).length).toBe(0)
  })

  it.each([
    ['half a code', 'mockcode123', 'only half the code'],
    ['the other half missing', 'mockcode123#', 'only half the code'],
    ['two codes run together', 'a#b#c', 'more than one #'],
    ['a space inside', 'mock code#state', 'no spaces'],
  ])('%s is refused in the field and never sent', async (_, code, says) => {
    const { wrapper, b } = await open()
    await startAndWait(wrapper, b)
    await typeAndSubmit(wrapper, code)
    expect(section(wrapper).find('[data-test="login-shape"]').text()).toContain(says)
    expect(b.loginBodies.length).toBe(0)
    // Control: a whole code is sent.
    await section(wrapper).find('[data-test="login-code"]').setValue('mockcode123#mockstate')
    await button(wrapper, 'Submit code')!.trigger('click')
    await settle()
    expect(b.loginBodies.length).toBe(1)
  })

  it('THE CANARY: a submitted code is nowhere afterwards — DOM, fields, stores, storage, history, URLs, the stream', async () => {
    const canary = `cnry${crypto.randomUUID().replaceAll('-', '')}#cnrystate${crypto.randomUUID().replaceAll('-', '')}`
    const [left, right] = canary.split('#') as [string, string]
    const { wrapper, b, pinia } = await open({ loginAccepts: canary })
    await startAndWait(wrapper, b)
    await section(wrapper).find('[data-test="login-code"]').setValue(canary)
    // While typing it is a field's value, and only that (§4.2).
    expect(fieldValues()).toContain(canary)
    expect(JSON.stringify(pinia.state.value)).not.toContain(left)

    await button(wrapper, 'Submit code')!.trigger('click')
    await settle()

    // Control: it was sent, once, in a body — and it signed in.
    expect(b.loginBodies.length).toBe(1)
    expect(JSON.parse(b.loginBodies[0]!.body)).toEqual({ code: canary })
    expect(section(wrapper).find('[data-test="login-succeeded"]').exists()).toBe(true)

    for (const part of [canary, left, right]) {
      expect(document.documentElement.outerHTML).not.toContain(part)
      expect(fieldValues().join('\n')).not.toContain(part)
      expect(JSON.stringify(pinia.state.value)).not.toContain(part)
      expect(storageDump(localStorage)).not.toContain(part)
      expect(storageDump(sessionStorage)).not.toContain(part)
      expect(JSON.stringify(window.history.state)).not.toContain(part)
      expect(window.location.href).not.toContain(part)
      expect(b.log.map((r) => r.url).join('\n')).not.toContain(part)
      expect(JSON.stringify(b.events)).not.toContain(part)
    }
  })

  it('a code being typed does not survive leaving the page', async () => {
    const { wrapper, b, router } = await open()
    await startAndWait(wrapper, b)
    await section(wrapper).find('[data-test="login-code"]').setValue('halftyped123#abc')
    expect(fieldValues()).toContain('halftyped123#abc') // control
    await router.push('/')
    await settle()
    await router.push('/settings')
    await settle()
    expect((section(wrapper).find('[data-test="login-code"]').element as HTMLInputElement).value).toBe('')
    expect(document.documentElement.outerHTML + fieldValues().join()).not.toContain('halftyped123')
    // The login itself survived: it is the server's.
    expect(section(wrapper).find('[data-test="login-url"]').exists()).toBe(true)
  })
})

describe('the login lives on the server', () => {
  it('a reload mid-handshake comes back to the same link and countdown (§2.4)', async () => {
    const b = freshBackend({ signedIn: true, identity: identityView('blanked') })
    b.login = {
      login_id: '0000000000000000000000ab', phase: 'awaiting_code', url: MOCK_LOGIN_URL,
      deadline: new Date(Date.now() + 3 * 60e3).toISOString(), started_at: new Date().toISOString(),
      ended_at: null, attempts: 0, problem: null, message: '',
    }
    const { wrapper } = await mountApp('/settings')
    await settle()
    expect(section(wrapper).find('[data-test="login-url"]').attributes('href')).toBe(MOCK_LOGIN_URL)
    expect(section(wrapper).find('[data-test="login-deadline"]').text()).toMatch(/Time left: [23]:\d\d/)
    expect(startButton(wrapper)).toBeUndefined()
  })

  it('cancel: in flight until the login is announced over, then Start over', async () => {
    const { wrapper, b } = await open()
    await startAndWait(wrapper, b)
    await button(wrapper, 'Cancel sign-in')!.trigger('click')
    await settle()
    expect(b.login!.phase).toBe('cancelled')
    expect(section(wrapper).find('[data-test="login-ended"]').text()).toContain('The login was cancelled.')
    expect(startButton(wrapper)!.text()).toBe('Start over')
    expect(section(wrapper).find('[data-test="login-code"]').exists()).toBe(false)
  })

  it.each([
    ['timed_out', 'The login was not finished in time. Start over.'],
    ['failed', 'The login could not start: Docker did not answer.'],
  ] as const)('%s: the server says what happened, and Start over begins again', async (phase, message) => {
    const { wrapper, b } = await open()
    await startAndWait(wrapper, b)
    endLogin(b, phase, message, phase === 'failed' ? 'docker' : null)
    await settle()
    expect(section(wrapper).find('[data-test="login-ended"]').text()).toContain(message)
    await startButton(wrapper)!.trigger('click')
    await settle()
    expect(b.login!.phase).toBe('awaiting_code')
    expect(section(wrapper).find('[data-test="login-url"]').exists()).toBe(true)
  })

  it('signed in already: the button offers to sign in again, not to sign in', async () => {
    const { wrapper } = await open({ identity: identityView('ok') })
    expect(startButton(wrapper)!.text()).toBe('Sign in again')
    // Control: blanked says Sign in to Claude.
    const blanked = await open({ identity: identityView('blanked') })
    expect(startButton(blanked.wrapper)!.text()).toBe('Sign in to Claude')
  })
})
