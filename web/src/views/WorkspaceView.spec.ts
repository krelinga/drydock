import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, server, settle, useMockApi } from '../test/setup'
import { emit, playScript, WS_FAILED, WS_REMOVED, WS_RUNNING } from '../mocks/backend'
import { useStreamStore } from '../stores/stream'

useMockApi()

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
const steps = (w: Wrapper) => w.findAll('[data-test="step"]').map((s) => [s.attributes('data-step'), s.find('[data-test="step-status"]').text()])
const reads = (b: ReturnType<typeof freshBackend>, id: string) =>
  b.log.filter((r) => r.method === 'GET' && new URL(r.url).pathname === `/api/workspaces/${id}`).length
const startBtn = (w: Wrapper) => w.find('[data-test="start"] [data-test="action"]')

describe('the workspace detail', () => {
  it('fetches on entry and shows the state, the eight steps, and the failed one named', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_FAILED}`)
    expect(reads(b, WS_FAILED)).toBe(1)
    expect(wrapper.find('[data-test="ws-name"]').text()).toBe('krelinga/homelab')
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Failed while starting the container')
    expect(wrapper.find('[data-test="ws-note"]').text()).toBe('The container did not start. The service log has the build output.')
    expect(steps(wrapper)).toEqual([
      ['allocate', 'done'], ['clone', 'done'], ['resolve_config', 'done'], ['credential_volume', 'done'],
      ['broker_socket', 'done'], ['up', 'failed'], ['verify', 'not run'], ['session_server', 'not run'],
    ])
    const up = wrapper.find('[data-step="up"]')
    expect(up.classes()).toContain('failed')
    expect(up.find('[data-test="step-detail"]').text()).toContain('did not start')
    // Control: a step that did not fail is not marked.
    expect(wrapper.find('[data-step="clone"]').classes()).not.toContain('failed')
  })

  it('lists the recent events oldest to newest, from the GET', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    const msgs = wrapper.findAll('[data-test="event-message"]').map((m) => m.text())
    expect(msgs[0]).toBe('Workspace created.')
    expect(msgs[msgs.length - 1]).toBe('Running.')
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Running')
  })

  it('renders an event message as text, never as HTML', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const hostile = '<img src=x onerror="window.__pwned=1"><b>bold</b>'
    emit(b, 'token.issued', { workspace_id: WS_RUNNING, message: hostile, data: { scope: 'git' } })
    await settle()
    const events = wrapper.find('[data-test="events"]')
    // Control: the message did arrive, verbatim, as characters.
    expect(events.text()).toContain(hostile)
    expect(events.find('img').exists()).toBe(false)
    expect(events.find('b').exists()).toBe(false)
  })

  it('lives off the stream: a start plays through with no reload', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    // A 202 that claims more than it means (§4.2 step 3).
    server.use(http.post(`/api/workspaces/${WS_FAILED}/start`, ({ request }) => {
      b.log.push({ method: request.method, url: request.url, credentials: request.credentials, mode: request.mode, contentType: null })
      b.scripts[WS_FAILED] = []
      return HttpResponse.json({ state: 'running', container_id: 'deadbeef' }, { status: 202 })
    }))
    const { wrapper, pinia } = await mountApp(`/ws/${WS_FAILED}`)
    FakeEventSource.latest().open().pipe(b)
    await startBtn(wrapper).trigger('click')
    await settle()
    expect(startBtn(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Failed while starting the container')
    expect(wrapper.text()).not.toContain('deadbeef')
    expect(useStreamStore(pinia).entities.workspaces[WS_FAILED]?.state).toBe('failed')

    // The server's real start, played as events.
    emit(b, 'workspace.state', { workspace_id: WS_FAILED, message: 'Building the container.', data: { state: 'building', from: 'failed' } })
    emit(b, 'workspace.step', { workspace_id: WS_FAILED, data: { step: 'up', status: 'started' } })
    await settle()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Building · starting the container…')
    expect(wrapper.find('[data-step="up"] [data-test="step-status"]').text()).toBe('running')
    expect(wrapper.find('[data-test="start"]').exists()).toBe(false)
    const before = reads(b, WS_FAILED)
    emit(b, 'workspace.step', { workspace_id: WS_FAILED, data: { step: 'up', status: 'done' } })
    expect(wrapper.text()).not.toContain('Container')
    emit(b, 'workspace.state', {
      workspace_id: WS_FAILED, message: 'Running.', data: { state: 'running', from: 'building', container_id: 'feedfacecafe0123456789' },
    })
    await settle()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Running')
    expect(wrapper.find('[data-step="up"] [data-test="step-status"]').text()).toBe('done')
    // The move to running carries the container id: no refetch to learn it.
    expect(reads(b, WS_FAILED)).toBe(before)
    expect(wrapper.text()).toContain('Container')
    expect(wrapper.text()).toContain('feedfacecafe')
  })

  it('after a start, shows only the current run\'s steps — live, and after a reload', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp(`/ws/${WS_FAILED}`)
    FakeEventSource.latest().open().pipe(b)
    // Control: before the start, the failed run's `up` is the timeline's.
    expect(steps(wrapper)).toContainEqual(['up', 'failed'])
    await startBtn(wrapper).trigger('click')
    await settle()
    // building, resolve_config started: the start resumes at step 3.
    playScript(b, WS_FAILED, 2)
    await settle()
    const live = [
      ['allocate', 'done'], ['clone', 'done'], ['resolve_config', 'running'], ['credential_volume', 'not run'],
      ['broker_socket', 'not run'], ['up', 'not run'], ['verify', 'not run'], ['session_server', 'not run'],
    ]
    expect(steps(wrapper)).toEqual(live)
    expect(wrapper.find('[data-step="up"]').classes()).not.toContain('failed')
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Building · resolving config…')

    // A cold load mid-run: the server's `steps` still holds `up: failed`
    // from the earlier run, and the snapshot alone must not show it.
    const cold = await mountApp(`/ws/${WS_FAILED}`)
    expect(b.workspaces[WS_FAILED]!.steps.up?.status).toBe('failed')
    expect(steps(cold.wrapper)).toEqual(live)
    expect(cold.wrapper.find('[data-test="ws-state"]').text()).toBe('Building · resolving config…')
  })

  it('a stopped workspace offers Start, says the clone survived, and a refused start says why', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp(`/ws/${WS_REMOVED}`)
    FakeEventSource.latest().open().pipe(b)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Stopped')
    expect(wrapper.find('[data-test="ws-note"]').text()).toBe('The clone is intact.')
    // It moved on the server before this click: the server says in_progress.
    b.workspaces[WS_REMOVED] = { ...b.workspaces[WS_REMOVED]!, state: 'building' }
    await startBtn(wrapper).trigger('click')
    await settle()
    expect(wrapper.find('[data-test="action-note"]').text()).toBe('Already in progress.')
    expect(startBtn(wrapper).attributes('disabled')).toBeUndefined()
    // Control: once it is stopped again, the start is accepted.
    b.workspaces[WS_REMOVED] = { ...b.workspaces[WS_REMOVED]!, state: 'stopped' }
    await startBtn(wrapper).trigger('click')
    await settle()
    expect(wrapper.find('[data-test="action-note"]').exists()).toBe(false)
    expect(startBtn(wrapper).attributes('disabled')).toBeDefined()
    playScript(b, WS_REMOVED)
    await settle()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Running')
  })

  it('refetches when the stream reopens, and not before', async () => {
    const b = freshBackend({ signedIn: true })
    await mountApp(`/ws/${WS_RUNNING}`)
    const es = FakeEventSource.latest().open()
    expect(reads(b, WS_RUNNING)).toBe(1)
    es.drop().open()
    await settle()
    expect(reads(b, WS_RUNNING)).toBe(2)
  })

  it('an unknown id says so; one deleted while open says that instead', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/ws/01JA00000000000000000NOPE0')
    expect(wrapper.find('[data-test="ws-not-found"]').text()).toContain('no workspace')
    // Control: a real one renders.
    const other = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    expect(other.wrapper.find('[data-test="ws-card"]').exists()).toBe(true)
    emit(b, 'workspace.state', { workspace_id: WS_RUNNING, data: { state: 'deleting', from: 'running' } })
    await settle()
    expect(other.wrapper.find('[data-test="ws-state"]').text()).toBe('Deleting…')
    emit(b, 'workspace.gone', { workspace_id: WS_RUNNING, data: {} })
    await settle()
    expect(other.wrapper.find('[data-test="ws-card"]').exists()).toBe(false)
    expect(other.wrapper.find('[data-test="ws-deleted"]').exists()).toBe(true)
  })

  it('keeps the Workspaces tab current', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(wrapper.find('nav a[aria-current="page"]').text()).toContain('Workspaces')
  })
})
