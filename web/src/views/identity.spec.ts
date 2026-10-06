// The shared Claude login on screen (frontend §6.6, design §7.3), through the
// whole app: the fleet banner, the cards, Settings. The property under test is
// "one fault, one message, one button": ten running workspaces and a blanked
// login make one banner and one Sign in to Claude, not ten — and the card
// whose fault is its own keeps its own status and action.

import { describe, expect, it } from 'vitest'
import type { IdentityState, RepoView } from '../api/types'
import { failIdentityCheck, identityView, setIdentity, type MockBackend, type MockWorkspace } from '../mocks/backend'
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
    ['expired', 'The Claude login has expired. Sign in again.'],
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
  it.each(['blanked', 'absent', 'expired'] as const)('%s with session servers: one waiting sentence per card, no restart button', async (state) => {
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

  it('a failed check keeps the stored state and says why', async () => {
    const b = fleet('expiring')
    const { wrapper } = await mountApp('/settings')
    FakeEventSource.latest().open().pipe(b)
    failIdentityCheck(b)
    await settle()
    expect(wrapper.find('[data-test="claude-state"]').text()).toBe('Signed in, and the login expires soon.')
    expect(wrapper.find('[data-test="claude-check-error"]').text()).toContain('The last known state is kept.')
  })
})
