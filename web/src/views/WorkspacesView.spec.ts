import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { cloneScript, emit, WS_FAILED } from '../mocks/backend'
import { get } from '../api/client'

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
    expect(rows[1]!.find('[data-test="repo-state"]').text()).toBe('Failed')
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

  it('a workspace deleted elsewhere leaves both sections', async () => {
    const b = freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const homelab = () => wrapper.findAll('[data-test="repo"]')[1]!
    expect(homelab().find('[data-test="repo-state"]').text()).toBe('Failed')
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
