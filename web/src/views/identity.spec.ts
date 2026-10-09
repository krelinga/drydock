// The shared Claude login on screen (frontend §6.6, design §7.3), through the
// whole app: the fleet banner, the cards, Settings. The property under test is
// "one fault, one message, one button": ten running workspaces and a blanked
// login make one banner and one Sign in to Claude, not ten — and the card
// whose fault is its own keeps its own status and action.

import { describe, expect, it } from 'vitest'
import type { IdentityState, RepoView } from '../api/types'
import {
  failIdentityCheck, finishIdentityCheck, identityView, intervalIdentityCheck, refreshed, setIdentity, startIdentityCheck,
  type MockBackend, type MockWorkspace,
} from '../mocks/backend'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { CHECK_KEY } from '../stores/identity'
import { useStreamStore } from '../stores/stream'

useMockApi()

const RUNNING = 10

/** Ten running workspaces and one whose build failed, under a login in `state`. */
function fleet(state: IdentityState | null, expiresInMs?: number): MockBackend {
  const now = Date.now()
  const repos: Array<Omit<RepoView, 'workspace'>> = []
  const workspaces: Record<string, MockWorkspace> = {}
  for (let i = 1; i <= RUNNING + 1; i++) {
    repos.push({
      id: i, installation_id: 101, full_name: `krelinga/repo-${i}`, default_branch: 'main', private: true,
      archived: false, has_devcontainer: true, pushed_at: new Date(now - i * 60e3).toISOString(), removed: false,
    })
    const id = `01JB${String(i).padStart(22, '0')}`
    const failed = i === RUNNING + 1
    workspaces[id] = {
      id, repository_id: i, branch: 'main', state: failed ? 'failed' : 'running',
      state_detail: failed ? 'The image build failed.' : null, container_id: failed ? null : `c${i}`,
      created_at: new Date(now - i * 3600e3).toISOString(),
      steps: failed ? { up: { status: 'failed', at: new Date(now).toISOString() } } : {},
    }
  }
  // Room for the failed one to rebuild: at the cap (the mock's default is 4)
  // its Rebuild would be replaced by a pointer to Stop, which is the cap's
  // story, not the login's (frontend §9).
  return freshBackend({ signedIn: true, repos, workspaces, capacity: RUNNING + 1, identity: identityView(state, expiresInMs) })
}

const identityBanners = (w: Awaited<ReturnType<typeof mountApp>>['wrapper']) =>
  w.findAll('[data-test="fleet-banner"]').filter((b) => (b.attributes('data-banner') ?? '').startsWith('identity'))
const signInButtons = (w: Awaited<ReturnType<typeof mountApp>>['wrapper']) =>
  w.findAll('a, button').filter((e) => e.text() === 'Sign in to Claude')

describe('one fault, ten cards', () => {
  it.each([
    ['blanked', 'Signed out. Sign in again.'],
    ['absent', 'No one has signed in yet.'],
  ] as const)('%s: one banner, one button, every running card waiting, the failed card its own', async (state, title) => {
    fleet(state)
    const { wrapper } = await mountApp('/')
    const rows = wrapper.findAll('[data-test="running-row"]')
    expect(rows.length).toBe(RUNNING + 1)

    const banners = identityBanners(wrapper)
    expect(banners.length).toBe(1)
    expect(banners[0]!.find('[data-test="fleet-title"]').text()).toBe(title)
    expect(signInButtons(wrapper).length).toBe(1)
    expect(banners[0]!.find('[data-test="fleet-dismiss"]').exists()).toBe(false)

    const running = rows.filter((r) => r.find('[data-test="running-state"]').text().startsWith('Running'))
    const failed = rows.filter((r) => r.text().includes('Failed'))
    expect(running.length).toBe(RUNNING)
    for (const r of running) {
      expect(r.find('[data-test="identity-waiting"]').text()).toBe('Waiting on Claude sign-in.')
      // No per-card action for the login: no sign-in, no session restart.
      // The container's own actions (Stop, Rebuild) still work, and stay.
      expect(r.findAll('a, button').filter((e) => /sign in|restart|session/i.test(e.text()))).toEqual([])
    }
    // The card whose fault is its own keeps its own status and its own action.
    expect(failed.length).toBe(1)
    expect(failed[0]!.find('[data-test="identity-waiting"]').exists()).toBe(false)
    expect(failed[0]!.find('[data-test="rebuild"]').exists()).toBe(true)
  })

  // Phase 5: the same fleet with session servers that report — each degraded,
  // the very cards that would carry ten Restart session server buttons. The
  // override drops the session half, and the waiting sentence is said once
  // per card, by the identity note, never also by the card's own line.
  it.each(['blanked', 'absent'] as const)('%s with session servers: one waiting sentence per card, no restart button', async (state) => {
    const b = fleet(state)
    b.supervisor = true
    for (const w of Object.values(b.workspaces)) {
      if (w.state === 'running') {
        w.supervisor = { state: 'degraded', reason: 'budget_spent', detail: 'Parked.', restart_count: 6, at: new Date().toISOString() }
      }
    }
    const { wrapper } = await mountApp('/')
    const running = wrapper.findAll('[data-test="running-row"]').filter((r) => !r.text().includes('Failed'))
    expect(running.length).toBe(RUNNING)
    for (const r of running) {
      expect(r.text().split('Waiting on Claude sign-in').length - 1).toBe(1)
      expect(r.find('[data-test="running-state"]').text()).toBe('Running')
      expect(r.find('[data-test="restart-session"]').exists()).toBe(false)
    }
    expect(signInButtons(wrapper).length).toBe(1)
  })

  it('control: the same session servers under ok each say degraded and offer a restart', async () => {
    const b = fleet('ok')
    b.supervisor = true
    for (const w of Object.values(b.workspaces)) {
      if (w.state === 'running') {
        w.supervisor = { state: 'degraded', reason: 'budget_spent', detail: 'Parked.', restart_count: 6, at: new Date().toISOString() }
      }
    }
    const { wrapper } = await mountApp('/')
    expect(wrapper.findAll('[data-test="running-row"] [data-test="restart-session"]').length).toBe(RUNNING)
    expect(wrapper.text()).not.toContain('Waiting on Claude sign-in')
  })

  it('blanked and absent are told apart on screen', async () => {
    fleet('blanked')
    const blanked = await mountApp('/')
    const b = identityBanners(blanked.wrapper)[0]!.text()
    blanked.wrapper.unmount()
    fleet('absent')
    const absent = await mountApp('/')
    const a = identityBanners(absent.wrapper)[0]!.text()
    expect(b).toContain('every workspace lost access')
    expect(a).not.toContain('lost access')
    expect(a).toContain('No one has signed in yet.')
    expect(b).not.toContain('No one has signed in yet.')
  })

  it('ok: no banner, no dot, no waiting line', async () => {
    fleet('ok')
    const { wrapper } = await mountApp('/')
    expect(identityBanners(wrapper).length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-dot"]').length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-waiting"]').length).toBe(0)
    expect(signInButtons(wrapper).length).toBe(0)
  })

  // The real deployment, 2026-10-08: right after a real sign-in the access
  // token's expiry was about eight hours out, and every screen said the
  // login was expiring. A healthy login with exactly that credential warns
  // nowhere — home, cards, Settings. The positive control is a blanked
  // login on the same fleet, which still makes its one banner.
  it('a fresh sign-in, access token eight hours out: no warning anywhere', async () => {
    const b = fleet('ok')
    const eight = new Date(Date.now() + 8 * 3600e3).toISOString()
    expect(b.identity.expires_at!.slice(0, 13)).toBe(eight.slice(0, 13)) // precondition: the measured shape
    const { wrapper, router } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    expect(identityBanners(wrapper).length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-dot"]').length).toBe(0)
    expect(wrapper.text()).not.toMatch(/expir/i)

    await router.push('/settings')
    await settle()
    const section = wrapper.find('[data-test="claude-identity"]')
    expect(section.find('[data-test="claude-state"]').text()).toBe('Signed in as operator@example.invalid.')
    expect(section.find('[data-test="claude-expires"]').text()).toBe('in 1 month') // the login's, not the token's
    expect(section.text()).not.toMatch(/in 8 hours|in 7 hours/)
    expect(identityBanners(wrapper).length).toBe(0)

    // Control: the fleet-wide fault still takes over every card.
    setIdentity(b, identityView('blanked'))
    await settle()
    expect(identityBanners(wrapper).length).toBe(1)
    expect(identityBanners(wrapper)[0]!.find('[data-test="fleet-title"]').text()).toBe('Signed out. Sign in again.')
    await router.push('/')
    await settle()
    expect(wrapper.findAll('[data-test="identity-waiting"]').length).toBe(RUNNING)
  })

  it('a login date already passed reads as passed, not as "expires … ago", and warns nowhere', async () => {
    const b = fleet('ok')
    b.identity = { ...b.identity, login_expires_at: new Date(Date.now() - 3600e3).toISOString() }
    const { wrapper } = await mountApp('/settings')
    expect(wrapper.find('[data-test="claude-expires"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="claude-login-date-passed"]').text()).toMatch(/ago\. The next refresh either works or signs Claude out/)
    expect(identityBanners(wrapper).length).toBe(0)
  })

  it('a lapsed access token is informational: no banner, no dot, and Settings says the next server renews it', async () => {
    fleet('expired')
    const { wrapper, router } = await mountApp('/')
    expect(identityBanners(wrapper).length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-dot"]').length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-waiting"]').length).toBe(0)
    expect(signInButtons(wrapper).length).toBe(0)
    await router.push('/settings')
    await settle()
    expect(wrapper.find('[data-test="claude-state"]').text()).toBe('Signed in. The access token has lapsed; the next session server to start renews it.')
    expect(wrapper.find('[data-test="claude-access-lapsed"]').exists()).toBe(true)
    expect(identityBanners(wrapper).length).toBe(0)
  })

  it('an expiring stored before v0.4.5 (no login date) says nothing until the next check', async () => {
    const b = fleet('ok')
    b.identity = { ...b.identity, state: 'expiring', login_expires_at: null }
    const { wrapper } = await mountApp('/')
    expect(identityBanners(wrapper).length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-dot"]').length).toBe(0)
    // Control: the same state with the login's date does warn.
    FakeEventSource.latest().open().pipe(b)
    setIdentity(b, identityView('expiring', 2 * 86400e3 + 3600e3))
    await settle()
    expect(identityBanners(wrapper).length).toBe(1)
  })

  it('expiring: a countdown that can be put away, and a dot on every running card', async () => {
    const b = fleet('expiring', 2 * 86400e3 + 3600e3)
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const banner = identityBanners(wrapper)[0]!
    expect(banner.find('[data-test="fleet-title"]').text()).toBe('The Claude login expires in 2 days.')
    expect(wrapper.findAll('[data-test="identity-dot"]').length).toBe(RUNNING)
    expect(wrapper.findAll('[data-test="identity-waiting"]').length).toBe(0) // still working

    await banner.find('[data-test="fleet-dismiss"]').trigger('click')
    await settle()
    expect(identityBanners(wrapper).length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-dot"]').length).toBe(RUNNING) // the dot stays

    // A refresh moves the access token's expiry and not the login's: the
    // same countdown, so it stays put away. Keyed by expires_at, it would
    // come back every eight hours.
    const before = b.identity.login_expires_at
    setIdentity(b, refreshed(b.identity))
    await settle()
    expect(b.identity.login_expires_at).toBe(before) // precondition: only the access token moved
    expect(useStreamStore().entities.identity?.expiresAt).toBe(b.identity.expires_at) // and the event landed
    expect(identityBanners(wrapper).length).toBe(0)

    // A new countdown is a new warning: put away is per countdown, not forever.
    setIdentity(b, identityView('expiring', 86400e3))
    await settle()
    expect(identityBanners(wrapper).length).toBe(1)
  })

  it('follows the stream: blanked arrives as an event, and a new login clears it', async () => {
    const b = fleet('ok')
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    expect(identityBanners(wrapper).length).toBe(0)

    setIdentity(b, identityView('blanked'))
    await settle()
    expect(identityBanners(wrapper).length).toBe(1)
    expect(wrapper.findAll('[data-test="identity-waiting"]').length).toBe(RUNNING)

    setIdentity(b, identityView('ok'))
    await settle()
    expect(identityBanners(wrapper).length).toBe(0)
    expect(wrapper.findAll('[data-test="identity-waiting"]').length).toBe(0)
  })
})

describe('the Claude section in Settings', () => {
  it('the banner button leads here, where the handshake is the one Sign in to Claude', async () => {
    fleet('blanked')
    const { wrapper, router } = await mountApp('/')
    await signInButtons(wrapper)[0]!.trigger('click')
    await settle()
    expect(router.currentRoute.value.path).toBe('/settings')
    expect(router.currentRoute.value.hash).toBe('#claude')
    const section = wrapper.find('[data-test="claude-identity"]')
    expect(section.find('[data-test="claude-state"]').text()).toBe('Signed out. Sign in again.')
    // The banner still says what is wrong, and offers no second button for
    // the fix: the section's own is the one on this page.
    expect(identityBanners(wrapper).length).toBe(1)
    expect(identityBanners(wrapper)[0]!.find('[data-test="fleet-action"]').exists()).toBe(false)
    expect(signInButtons(wrapper).length).toBe(1)
    expect(section.find('[data-test="claude-login"]').exists()).toBe(true)
  })

  it('shows the account and expiry beside a login, and no sign-in note', async () => {
    fleet('ok')
    const { wrapper } = await mountApp('/settings')
    const section = wrapper.find('[data-test="claude-identity"]')
    expect(section.find('[data-test="claude-state"]').text()).toBe('Signed in as operator@example.invalid.')
    expect(section.find('[data-test="claude-account"]').text()).toBe('operator@example.invalid')
    expect(section.find('[data-test="claude-expires"]').text()).toBe('in 1 month')
    expect(section.find('[data-test="claude-access-lapsed"]').exists()).toBe(false)
    expect(section.find('[data-test="claude-sign-in-next"]').exists()).toBe(false)
  })

  it('check now is in flight until the check is announced', async () => {
    const b = fleet('ok')
    b.identity = { ...b.identity, check_error: { at: new Date().toISOString(), problem: 'docker', message: 'Could not check the Claude login: Docker did not answer. The last known state is kept.' } }
    const { wrapper, pinia } = await mountApp('/settings')
    const es = FakeEventSource.latest().open()
    // A reload sees a standing failure, from the snapshot.
    expect(wrapper.find('[data-test="claude-check-error"]').text()).toContain('Docker did not answer')

    const stream = useStreamStore(pinia)
    b.subscribers.clear() // hold the event back, to see the in-flight state
    await wrapper.find('[data-test="claude-check"]').trigger('click')
    await settle()
    expect(b.identityChecks).toBe(1)
    expect(CHECK_KEY in stream.inFlight).toBe(true)
    expect(wrapper.find('[data-test="claude-check"]').attributes('disabled')).toBeDefined()

    es.pipe(b)
    setIdentity(b, { ...b.identity, check_error: null })
    await settle()
    expect(CHECK_KEY in stream.inFlight).toBe(false)
    expect(wrapper.find('[data-test="claude-check-error"]').exists()).toBe(false)
  })

  // #81's review: the watch announces auth.identity only for a change, so the
  // common healthy press — the same ok as before — used to write nothing and
  // leave the button disabled until a reload. The mock answers as the server
  // does now (finishIdentityCheck): auth.identity_checked for a requested
  // check with nothing to announce, silence for the interval's.
  const press = async (w: Awaited<ReturnType<typeof mountApp>>['wrapper']) => {
    await w.find('[data-test="claude-check"]').trigger('click')
    await settle()
  }

  it('check now settles when the login has not changed', async () => {
    const b = fleet('ok')
    b.identityCheckMode = 'manual'
    const { wrapper, pinia } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)
    const before = b.events.length

    await press(wrapper)
    // Control: the check is still running, so the press is in flight.
    expect(b.identityChecks).toBe(1)
    expect(CHECK_KEY in stream.inFlight).toBe(true)
    expect(wrapper.find('[data-test="claude-check"]').attributes('disabled')).toBeDefined()

    finishIdentityCheck(b) // the same ok
    await settle()
    expect(b.events.slice(before).map((e) => e.kind)).toEqual(['auth.identity_checked'])
    expect(CHECK_KEY in stream.inFlight).toBe(false)
    expect(wrapper.find('[data-test="claude-check"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.find('[data-test="claude-state"]').text()).toBe('Signed in as operator@example.invalid.')
  })

  it('the interval says nothing unless something changed, as the watch does', async () => {
    // The mock's realism, which the specs above lean on: a check nobody
    // asked for writes no event when nothing changed — so an unchanged
    // check's answer to a press can only be auth.identity_checked — and does
    // write auth.identity when something did. A mock that announced every
    // check would hide a client waiting on auth.identity alone.
    const b = fleet('ok')
    const { wrapper } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)

    const quiet = b.events.length
    intervalIdentityCheck(b)
    await settle()
    expect(b.events.length).toBe(quiet)

    intervalIdentityCheck(b, identityView('blanked'))
    await settle()
    expect(b.events.slice(quiet).map((e) => e.kind)).toEqual(['auth.identity'])
    expect(wrapper.find('[data-test="claude-state"]').exists()).toBe(true)
  })

  it('a press during a running check settles on the check after it', async () => {
    // As the watch does: the running check may have read the volume before
    // the press, so it does not answer it; the check queued behind it does.
    const b = fleet('ok')
    b.identityCheckMode = 'manual'
    const { wrapper, pinia } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)

    startIdentityCheck(b, false) // the interval's, running
    await press(wrapper)
    expect(b.identityChecks).toBe(1)
    expect(CHECK_KEY in stream.inFlight).toBe(true)

    const before = b.events.length
    finishIdentityCheck(b) // the interval's, unchanged: silent
    await settle()
    expect(b.events.length).toBe(before)
    expect(CHECK_KEY in stream.inFlight).toBe(true)

    finishIdentityCheck(b) // the press's own, unchanged
    await settle()
    expect(b.events.slice(before).map((e) => e.kind)).toEqual(['auth.identity_checked'])
    expect(CHECK_KEY in stream.inFlight).toBe(false)
  })

  it('a press during a running check that changes the verdict settles on its auth.identity', async () => {
    // Any auth.identity newer than the press settles it (settlesCheck), so
    // the unasked check's announcement ends the mark before the press's own
    // check runs; that check then answers too, which is harmless.
    const b = fleet('ok')
    b.identityCheckMode = 'manual'
    const { wrapper, pinia } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)

    startIdentityCheck(b, false)
    await press(wrapper)
    expect(CHECK_KEY in stream.inFlight).toBe(true)
    finishIdentityCheck(b, identityView('blanked'))
    await settle()
    expect(CHECK_KEY in stream.inFlight).toBe(false)
    expect(b.identityCheck).not.toBeNull() // the press's own check, queued, now running
  })

  it('a check refused while Drydock shuts down ends the press and says so', async () => {
    // Nothing would answer a check after the watch has stopped, so the
    // server refuses it (503 unavailable) rather than accepting a request
    // that would spin until a reload. The control is the same press accepted.
    for (const stopped of [false, true]) {
      const b = fleet('ok')
      b.identityCheckMode = 'manual'
      b.identityWatchStopped = stopped
      const { wrapper, pinia } = await mountApp('/settings')
      FakeEventSource.latest().open().pipe(b)
      const stream = useStreamStore(pinia)
      await press(wrapper)
      expect(CHECK_KEY in stream.inFlight).toBe(!stopped)
      const refused = wrapper.find('[data-test="claude-check-refused"]')
      expect(refused.exists()).toBe(stopped)
      if (stopped) expect(refused.text()).toContain('shutting down')
      wrapper.unmount()
    }
  })

  it('a failed check keeps the stored state and says why', async () => {
    const b = fleet('expiring', 2 * 86400e3 + 3600e3)
    const { wrapper } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    failIdentityCheck(b)
    await settle()
    expect(wrapper.find('[data-test="claude-state"]').text()).toBe('Signed in. The login expires in 2 days; sign in again before then.')
    expect(wrapper.find('[data-test="claude-check-error"]').text()).toContain('The last known state is kept.')
  })
})
