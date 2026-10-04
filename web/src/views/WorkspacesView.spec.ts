import { afterEach, describe, expect, it, vi } from 'vitest'
import { http, HttpResponse } from 'msw'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, server, settle, useMockApi } from '../test/setup'
import { cloneScript, completeRefresh, emit, playScript, WS_FAILED, WS_RUNNING } from '../mocks/backend'
import { get } from '../api/client'
import { SLOW_AFTER_MS, useStreamStore } from '../stores/stream'

useMockApi()

const names = (w: Awaited<ReturnType<typeof mountApp>>['wrapper']) =>
  w.findAll('[data-test="repo"] .name').map((n) => n.text())

describe('the catalog', () => {
  it('lists every repository joined to its workspace state', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    expect(names(wrapper)).toEqual([
      'krelinga/drydock', 'krelinga/homelab', 'krelinga/notes', 'krelinga/old-site', 'krelinga/scratch',
    ])
    const rows = wrapper.findAll('[data-test="repo"]')
    expect(rows[0]!.find('[data-test="repo-state"]').text()).toBe('Running')
    expect(rows[1]!.find('[data-test="repo-state"]').text()).toBe('Failed while starting the container')
    // Control: a repo with no workspace shows no state at all.
    expect(rows[2]!.find('[data-test="repo-state"]').exists()).toBe(false)
  })

  it('never renders an unknown dev container as "no dev container"', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    const rows = wrapper.findAll('[data-test="repo"]')
    const notes = rows[2]! // has_devcontainer: null
    expect(notes.find('[data-test="badge-no-devcontainer"]').exists()).toBe(false)
    expect(notes.find('[data-test="badge-devcontainer"]').exists()).toBe(false)
    expect(notes.text()).not.toMatch(/no dev container/i)
    // Controls: true and false each render their own badge.
    expect(rows[0]!.find('[data-test="badge-devcontainer"]').exists()).toBe(true)
    expect(rows[1]!.find('[data-test="badge-no-devcontainer"]').exists()).toBe(true)
  })

  it('badges a removed repository read-only, with the installation settings link', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    const rows = wrapper.findAll('[data-test="repo"]')
    const removed = rows[4]!
    expect(removed.find('[data-test="badge-removed"]').text()).toBe('read-only')
    const note = removed.find('[data-test="removed-note"]')
    expect(note.text()).toContain('Unpushed work in the working tree survives')
    expect(note.find('a').attributes('href')).toBe('https://github.com/settings/installations/101')
    // Control: a covered repository carries neither.
    expect(rows[0]!.find('[data-test="badge-removed"]').exists()).toBe(false)
    expect(rows[0]!.find('[data-test="removed-note"]').exists()).toBe(false)
  })

  it('says so when no GitHub App is configured, rather than showing an empty list', async () => {
    const b = freshBackend({ signedIn: true, appConfigured: false })
    const { wrapper } = await mountApp('/')
    expect(wrapper.find('[data-test="app-not-configured"]').text()).toContain('No GitHub App is set up yet')
    expect(wrapper.find('[data-test="repos"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="catalog-empty"]').exists()).toBe(false)
    // Control: once configured, a reopen's refetch brings the list.
    b.appConfigured = true
    FakeEventSource.latest().open().drop().open()
    await settle()
    expect(wrapper.find('[data-test="app-not-configured"]').exists()).toBe(false)
    expect(names(wrapper).length).toBe(5)
  })

  it('an empty catalog says what makes it non-empty, linking to the installation settings', async () => {
    freshBackend({ signedIn: true, repos: [], workspaces: {} })
    const { wrapper } = await mountApp('/')
    const empty = wrapper.find('[data-test="catalog-empty"]')
    expect(empty.text()).toContain('Repos appear here when the GitHub App is installed on them')
    expect(empty.find('[data-test="settings-link"]').attributes('href')).toBe('https://github.com/settings/installations/101')
  })

  it('before the first refresh it says loading, not empty', async () => {
    freshBackend({ signedIn: true, repos: [], workspaces: {}, refreshedAt: null })
    const { wrapper } = await mountApp('/')
    expect(wrapper.find('[data-test="catalog-first-refresh"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="catalog-empty"]').exists()).toBe(false)
  })

  it('search filters by name, and no match keeps the where-is-my-repo line', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    await wrapper.find('[data-test="search"]').setValue('HOME')
    expect(names(wrapper)).toEqual(['krelinga/homelab'])
    expect(wrapper.find('[data-test="catalog-empty"]').exists()).toBe(false)
    await wrapper.find('[data-test="search"]').setValue('nothing-like-this')
    expect(names(wrapper)).toEqual([])
    expect(wrapper.find('[data-test="no-match"]').text()).toContain('nothing-like-this')
    expect(wrapper.find('[data-test="catalog-empty"] [data-test="settings-link"]').exists()).toBe(true)
  })
})

describe('the catalog lives off the stream', () => {
  it('a clone on another device renders here from events alone', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    const es = FakeEventSource.latest().open().pipe(b)
    const reads = () => b.log.filter((r) => r.url.endsWith('/api/repos')).length
    const before = reads()

    const id = '01JA0000000000000000000050'
    const script = cloneScript(b, 3, id)
    script[0]!() // created
    await settle()
    const notes = () => wrapper.findAll('[data-test="repo"]')[2]!
    expect(notes().find('[data-test="repo-state"]').text()).toBe('Waiting to start')
    expect(wrapper.findAll('[data-test="running-row"]').map((r) => r.find('.name').text())).toContain('krelinga/notes')
    for (const play of script.slice(1)) play()
    await settle()
    expect(notes().find('[data-test="repo-state"]').text()).toBe('Running')
    // Placed from the events themselves: no refetch was needed.
    expect(reads()).toBe(before)
    expect(es.closed).toBe(false)
  })

  it('a failed build names its step', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    for (const play of cloneScript(b, 3, '01JA0000000000000000000051', 'up')) play()
    await settle()
    const notes = wrapper.findAll('[data-test="repo"]')[2]!
    expect(notes.find('[data-test="repo-state"]').text()).toBe('Failed while starting the container')
  })

  it('a repo.* event refetches the list', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    b.repos = [...b.repos, { ...b.repos[0]!, id: 9, full_name: 'krelinga/new-one' }]
    // Control: the backend changed, but nothing on screen moves without an event.
    await settle()
    expect(names(wrapper)).not.toContain('krelinga/new-one')
    emit(b, 'repo.refreshed', { data: { count: 6, added: 1, removed: 0 } })
    await settle()
    expect(names(wrapper)).toContain('krelinga/new-one')
  })

  it('a failed refresh is shown — even to a page loaded after it — and a good one clears it', async () => {
    const b = freshBackend({ signedIn: true })
    b.pendingRefreshes = 1
    completeRefresh(b, false) // before this page ever loaded: it missed the event
    const { wrapper } = await mountApp('/')
    const banner = wrapper.find('[data-test="refresh-error"]')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('GitHub answered 401 (Bad credentials).')
    expect(banner.text()).toContain('from the last one that worked')
    // The list is still the last good one, not emptied by the failure.
    expect(names(wrapper).length).toBeGreaterThan(0)

    FakeEventSource.latest().open().pipe(b)
    b.pendingRefreshes = 1
    completeRefresh(b, true)
    await settle()
    expect(wrapper.find('[data-test="refresh-error"]').exists()).toBe(false)
  })

  it('a workspace deleted elsewhere leaves both sections', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const homelab = () => wrapper.findAll('[data-test="repo"]')[1]!
    expect(homelab().find('[data-test="repo-state"]').text()).toBe('Failed while starting the container')
    emit(b, 'workspace.state', { workspace_id: WS_FAILED, data: { state: 'deleting', from: 'failed' } })
    emit(b, 'workspace.gone', { workspace_id: WS_FAILED, data: {} })
    await settle()
    expect(homelab().find('[data-test="repo-state"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-test="running-row"]').map((r) => r.text()).join()).not.toContain('homelab')
  })

  it('a session that lapses closes the stream and clears the catalog', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper, router } = await mountApp('/')
    const es = FakeEventSource.latest().open()
    expect(names(wrapper).length).toBe(5)
    b.signedIn = false
    await get('/api/repos').catch(() => undefined)
    await settle()
    expect(router.currentRoute.value.name).toBe('signin')
    expect(es.closed).toBe(true)
    expect(wrapper.text()).not.toContain('krelinga/drydock')
  })
})

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
const row = (w: Wrapper, name: string) => w.findAll('[data-test="repo"]').find((r) => r.find('.name').text() === name)!
const cloneBtn = (w: Wrapper, name: string) => row(w, name).find('[data-test="clone"] [data-test="action"]')
const posts = (b: ReturnType<typeof freshBackend>, path = '/api/workspaces') =>
  b.log.filter((r) => r.method === 'POST' && new URL(r.url).pathname === path)

describe('the clone button (§4.2)', () => {
  afterEach(() => vi.useRealTimers())

  it('is offered only where nothing holds the repository, and never on a removed one', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    expect(cloneBtn(wrapper, 'krelinga/notes').exists()).toBe(true)
    expect(cloneBtn(wrapper, 'krelinga/old-site').exists()).toBe(true)
    // A repo with a workspace offers that workspace's action instead, and a
    // removed one is read-only.
    expect(cloneBtn(wrapper, 'krelinga/drydock').exists()).toBe(false)
    expect(cloneBtn(wrapper, 'krelinga/scratch').exists()).toBe(false)
  })

  it('marks in flight, discards a misleading 202, and clears only on the workspace\'s event', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    let sent: unknown = null
    // A 202 whose body lies: an id nothing will ever name, already "running".
    server.use(http.post('/api/workspaces', async ({ request }) => {
      b.log.push({ method: request.method, url: request.url, credentials: request.credentials, mode: request.mode, contentType: request.headers.get('Content-Type') })
      sent = await request.json()
      return HttpResponse.json({
        id: '01JZ0000000000000000BOGUS0', state: 'running', repository_id: 3, full_name: 'krelinga/notes',
      }, { status: 202 })
    }))
    const { wrapper, pinia } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)

    await cloneBtn(wrapper, 'krelinga/notes').trigger('click')
    await settle()
    expect(sent).toEqual({ repository_id: 3 })
    expect(posts(b).length).toBe(1)
    // Accepted, and only in flight: nothing from the body was applied.
    expect(cloneBtn(wrapper, 'krelinga/notes').attributes('disabled')).toBeDefined()
    expect(cloneBtn(wrapper, 'krelinga/notes').attributes('aria-busy')).toBe('true')
    expect(stream.entities.workspaces['01JZ0000000000000000BOGUS0']).toBeUndefined()
    expect(row(wrapper, 'krelinga/notes').find('[data-test="repo-state"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('BOGUS')

    // An event for another repository does not settle it.
    emit(b, 'workspace.state', { workspace_id: WS_RUNNING, data: { state: 'stopped', from: 'running' } })
    await settle()
    expect('repo:3:clone' in stream.inFlight).toBe(true)

    // The create's own event does, and the row renders from the event.
    const id = '01JC0000000000000000000077'
    cloneScript(b, 3, id)[0]!()
    await settle()
    expect('repo:3:clone' in stream.inFlight).toBe(false)
    expect(row(wrapper, 'krelinga/notes').find('[data-test="repo-state"]').text()).toBe('Waiting to start')
    expect(cloneBtn(wrapper, 'krelinga/notes').exists()).toBe(false)
  })

  it('a double tap sends one request', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp('/')
    const btn = cloneBtn(wrapper, 'krelinga/notes')
    void btn.trigger('click')
    void btn.trigger('click')
    await settle()
    expect(posts(b).length).toBe(1)
    // Control: a different repository's button is its own request.
    await cloneBtn(wrapper, 'krelinga/old-site').trigger('click')
    await settle()
    expect(posts(b).length).toBe(2)
  })

  it('says "no response yet" after ten seconds, and never turns it into a failure', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    await cloneBtn(wrapper, 'krelinga/notes').trigger('click')
    await settle()
    const notes = () => row(wrapper, 'krelinga/notes')
    expect(notes().find('[data-test="action-slow"]').exists()).toBe(false)
    vi.advanceTimersByTime(SLOW_AFTER_MS)
    await settle()
    expect(notes().find('[data-test="action-slow"]').text()).toContain('No response yet')
    vi.advanceTimersByTime(10 * 60_000)
    await settle()
    expect(notes().find('[data-test="action-error"]').exists()).toBe(false)
    expect(cloneBtn(wrapper, 'krelinga/notes').attributes('disabled')).toBeDefined()
    // Control: the event still settles it.
    const id = Object.keys(b.scripts)[0]!
    playScript(b, id, 1)
    await settle()
    expect(notes().find('[data-test="action-slow"]').exists()).toBe(false)
    expect(notes().find('[data-test="repo-state"]').text()).toBe('Waiting to start')
  })

  it('in_progress from the server is a note, not an error, and frees the button', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    // Another device created one; its event has not reached this page.
    b.workspaces['01JC0000000000000000000088'] = {
      id: '01JC0000000000000000000088', repository_id: 3, branch: 'main', state: 'pending',
      state_detail: null, container_id: null, created_at: new Date().toISOString(), steps: {},
    }
    await cloneBtn(wrapper, 'krelinga/notes').trigger('click')
    await settle()
    const notes = row(wrapper, 'krelinga/notes')
    expect(notes.find('[data-test="action-note"]').text()).toBe('Already in progress.')
    expect(notes.find('[data-test="action-error"]').exists()).toBe(false)
    expect(cloneBtn(wrapper, 'krelinga/notes').attributes('disabled')).toBeUndefined()
  })

  it('at_capacity says to stop one in the Running section; under the cap it is accepted', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', capacity: 1 })
    const { wrapper, pinia } = await mountApp('/')
    await cloneBtn(wrapper, 'krelinga/notes').trigger('click')
    await settle()
    const err = row(wrapper, 'krelinga/notes').find('[data-test="action-error"]')
    expect(err.attributes('role')).toBe('alert')
    expect(err.text()).toContain('Stop one in the Running section')
    expect(cloneBtn(wrapper, 'krelinga/notes').attributes('disabled')).toBeUndefined()
    expect('repo:3:clone' in useStreamStore(pinia).inFlight).toBe(false)
    // Control: with room, the same tap is accepted and in flight.
    b.capacity = 4
    await cloneBtn(wrapper, 'krelinga/notes').trigger('click')
    await settle()
    expect(row(wrapper, 'krelinga/notes').find('[data-test="action-error"]').exists()).toBe(false)
    expect('repo:3:clone' in useStreamStore(pinia).inFlight).toBe(true)
  })

  it('a clone started here plays through to running from events alone', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    await cloneBtn(wrapper, 'krelinga/notes').trigger('click')
    await settle()
    const id = Object.keys(b.scripts)[0]!
    playScript(b, id, 5) // pending … clone started
    await settle()
    expect(row(wrapper, 'krelinga/notes').find('[data-test="repo-state"]').text()).toBe('Cloning · cloning…')
    playScript(b, id)
    await settle()
    expect(row(wrapper, 'krelinga/notes').find('[data-test="repo-state"]').text()).toBe('Running')
    const running = wrapper.findAll('[data-test="running-row"]').map((r) => r.find('[data-test="ws-link"]').text())
    expect(running).toContain('krelinga/notes')
  })
})

describe('the Running section', () => {
  it('lists every workspace the list has, not only each repository\'s newest', async () => {
    const b = freshBackend({ signedIn: true })
    // An older workspace on repo 2 still running beside its newer, failed one.
    b.workspaces['01JA0000000000000000000000'] = {
      id: '01JA0000000000000000000000', repository_id: 2, branch: 'old', state: 'running',
      state_detail: null, container_id: 'c0ffee', created_at: new Date().toISOString(), steps: {},
    }
    const { wrapper } = await mountApp('/')
    const rows = wrapper.findAll('[data-test="running-row"]')
    expect(rows.map((r) => r.find('[data-test="running-state"]').text())).toEqual([
      'Failed while starting the container', 'Running', 'Running',
    ])
    expect(rows.filter((r) => r.find('[data-test="ws-link"]').text() === 'krelinga/homelab').length).toBe(2)
    // Control: the catalog row still joins the newest.
    expect(row(wrapper, 'krelinga/homelab').find('[data-test="repo-state"]').text()).toBe('Failed while starting the container')
  })

  it('a failed card offers Start, which stays in flight until the workspace moves', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const card = () => wrapper.findAll('[data-test="running-row"]')[0]!
    expect(card().find('[data-test="ws-link"]').text()).toBe('krelinga/homelab')
    await card().find('[data-test="start"] [data-test="action"]').trigger('click')
    await settle()
    expect(posts(b, `/api/workspaces/${WS_FAILED}/start`).length).toBe(1)
    expect(card().find('[data-test="start"] [data-test="action"]').attributes('disabled')).toBeDefined()
    expect(card().find('[data-test="running-state"]').text()).toBe('Failed while starting the container')
    playScript(b, WS_FAILED, 1) // failed → building
    await settle()
    expect(card().find('[data-test="running-state"]').text()).toBe('Building')
    expect(card().find('[data-test="start"]').exists()).toBe(false)
  })

  it('the cards link to the detail route', async () => {
    freshBackend({ signedIn: true })
    const { wrapper, router } = await mountApp('/')
    await wrapper.findAll('[data-test="ws-link"]')[0]!.trigger('click')
    await settle()
    expect(router.currentRoute.value.fullPath).toBe(`/ws/${WS_FAILED}`)
  })
})
