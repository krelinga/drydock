// Phase 5's session half, against the pretend backend playing the
// supervisor (`supervisor: true`): the serving card's link and capacity, the
// restart action, Stop moving to the detail view and asking first while
// sessions are live, and the log view. Negative assertions sit beside their
// controls.

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { emit, mockEnvironmentId, playScript, STOP_FAILED_SENTENCE, WS_RUNNING } from '../mocks/backend'
import { useStreamStore } from '../stores/stream'
import { sessionKey } from '../stores/workspaces'

useMockApi()

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
type Backend = ReturnType<typeof freshBackend>
const runningCard = (w: Wrapper, id: string) =>
  w.findAll('[data-test="running-row"]').find((r) => r.find('[data-test="ws-link"]').attributes('href') === `/ws/${id}`)!
const btn = (scope: { find: Wrapper['find'] }, name: string) => scope.find(`[data-test="${name}"] [data-test="action"]`)
const sent = (b: Backend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path)

describe('the session half of a running card', () => {
  it('serving: the capacity fraction and Open in Claude, linking the environment; no Stop on the card', async () => {
    freshBackend({ signedIn: true, supervisor: true })
    const { wrapper } = await mountApp('/')
    const card = runningCard(wrapper, WS_RUNNING)
    expect(card.find('[data-test="running-state"]').text()).toBe('Capacity 1 / 4')
    const open = card.find('[data-test="open-in-claude"]')
    expect(open.attributes('href')).toBe(`https://claude.ai/code?environment=${mockEnvironmentId(WS_RUNNING)}`)
    expect(open.attributes('rel')).toContain('noopener')
    expect(card.find('[data-test="stop"]').exists()).toBe(false)
  })

  it('degraded offers Restart session server, which stays in flight until the supervisor reports', async () => {
    const b = freshBackend({ signedIn: true, supervisor: true, scriptMode: 'manual' })
    emit(b, 'supervisor.state', {
      workspace_id: WS_RUNNING, level: 'error', message: 'Parked.',
      data: { state: 'degraded', from: 'starting', reason: 'budget_spent', detail: 'It stopped 7 times in 10 min.', restart_count: 6 },
    })
    const { wrapper, pinia } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const card = () => runningCard(wrapper, WS_RUNNING)
    expect(card().find('[data-test="running-state"]').text()).toBe('Session degraded')
    expect(card().find('.detail').text()).toBe('It stopped 7 times in 10 min.')
    await btn(card(), 'restart-session').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/supervisor`)).toHaveLength(1)
    playScript(b, WS_RUNNING, 1) // exited: the old server stopping is not the end of it
    await settle()
    expect(sessionKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(true)
    playScript(b, WS_RUNNING)
    await settle()
    expect(sessionKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    expect(card().find('[data-test="running-state"]').text()).toBe('Capacity 1 / 4')
  })

  // A restart whose stop half fails (design §8, internal/supervisor Stop)
  // starts nothing and writes degraded with the cause's reason: the mark ends
  // on that event, and the card offers the one action that can work. The
  // control is the spec above, where the old server's `exited` does not end
  // the mark. Before the fix the server wrote nothing here, and the button
  // spun until a reload.
  it.each([
    ['stop_failed', 'Session server did not stop', 'restart-session'],
    ['survived_kill', 'Session server would not stop', 'rebuild'],
  ] as const)('a restart whose stop fails (%s) settles, saying so, and offers what can fix it', async (reason, line, action) => {
    const b = freshBackend({ signedIn: true, supervisor: true, scriptMode: 'manual' })
    const { wrapper, pinia } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const card = () => runningCard(wrapper, WS_RUNNING)
    // Serving offers no restart; get one to press from a parked server.
    emit(b, 'supervisor.state', {
      workspace_id: WS_RUNNING, level: 'error', message: 'Parked.',
      data: { state: 'degraded', from: 'starting', reason: 'budget_spent', detail: 'It stopped 7 times in 10 min.', restart_count: 6 },
    })
    await settle()
    b.supervisorStopFails = reason
    await btn(card(), 'restart-session').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/supervisor`)).toHaveLength(1)
    expect(sessionKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(true)
    playScript(b, WS_RUNNING)
    await settle()
    expect(sessionKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    expect(card().find('[data-test="running-state"]').text()).toBe(line)
    expect(card().find('.detail').text()).toBe(STOP_FAILED_SENTENCE[reason])
    expect(btn(card(), action).exists()).toBe(true)
    expect(btn(card(), action).attributes('disabled')).toBeUndefined()
    if (reason === 'stop_failed') {
      // Asking again is answered again, though it says the same thing.
      await btn(card(), 'restart-session').trigger('click')
      await settle()
      expect(sessionKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(true)
      playScript(b, WS_RUNNING)
      await settle()
      expect(sessionKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    }
  })

  it('the mock says the stop failures in the server’s words', () => {
    const go = readFileSync(resolve(process.cwd(), '../internal/supervisor/supervisor.go'), 'utf8')
    for (const s of Object.values(STOP_FAILED_SENTENCE)) expect(go).toContain(JSON.stringify(s))
  })

  it('waiting_registration says it is waiting, offers nothing, and never says failed', async () => {
    const b = freshBackend({ signedIn: true, supervisor: true })
    emit(b, 'supervisor.state', {
      workspace_id: WS_RUNNING, level: 'warn', message: 'Waiting.',
      data: { state: 'waiting_registration', from: 'starting', reason: 'wait_registration', detail: 'Waiting.', restart_count: 0 },
    })
    const { wrapper } = await mountApp('/')
    const card = runningCard(wrapper, WS_RUNNING)
    expect(card.find('[data-test="running-state"]').text()).toBe('Waiting for the previous session server to release the folder')
    expect(card.find('[data-test="waiting-since"]').exists()).toBe(true)
    expect(card.findAll('button')).toHaveLength(0)
    expect(card.text()).not.toMatch(/fail/i)
  })
})

describe('Stop, in the detail view', () => {
  it('asks first while sessions are live, then stops', async () => {
    const b = freshBackend({ signedIn: true, supervisor: true })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    const block = () => wrapper.find('[data-test="stop-block"]')
    expect(btn(block(), 'stop').exists()).toBe(false)
    await block().find('[data-test="stop-ask"]').trigger('click')
    expect(block().find('[data-test="stop-sessions"]').text()).toContain('One session is live')
    await btn(block(), 'stop').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/stop`)).toHaveLength(1)
  })

  it('control: with no live session it is one tap', async () => {
    const b = freshBackend({ signedIn: true, supervisor: true })
    emit(b, 'session.status', {
      workspace_id: WS_RUNNING, message: 'Capacity 0/4.',
      data: { environment_id: mockEnvironmentId(WS_RUNNING), capacity_used: 0, capacity_total: 4, sessions: 1 },
    })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    const block = wrapper.find('[data-test="stop-block"]')
    expect(block.find('[data-test="stop-ask"]').exists()).toBe(false)
    expect(btn(block, 'stop').exists()).toBe(true)
  })
})

describe('the log view', () => {
  it('renders the redacted lines as text, from the detail view’s link', async () => {
    freshBackend({ signedIn: true, supervisor: true })
    const { wrapper, router } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(wrapper.find('[data-test="logs-link"]').exists()).toBe(true)
    await router.push(`/ws/${WS_RUNNING}/logs`)
    await settle()
    const log = wrapper.find('[data-test="logs"]')
    expect(log.text()).toContain(`Environment ID: ${mockEnvironmentId(WS_RUNNING)}`)
    expect(log.text()).toContain('[redacted]')
    expect(log.find('a').exists()).toBe(false) // text, never HTML
  })

  it('says when Drydock holds no log', async () => {
    freshBackend({ signedIn: true, supervisor: false })
    const { wrapper, router } = await mountApp('/')
    await router.push(`/ws/${WS_RUNNING}/logs`)
    await settle()
    expect(wrapper.find('[data-test="logs-none"]').exists()).toBe(true)
  })
})
