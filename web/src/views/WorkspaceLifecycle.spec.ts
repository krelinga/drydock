// Phase 6's three buttons — Stop, Rebuild, Delete — through §4.2's lifecycle
// against the pretend backend, and §6.5's confirm. Every negative assertion
// sits beside a positive control in the same test.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { playScript, WS_FAILED, WS_REMOVED, WS_RUNNING } from '../mocks/backend'
import { SLOW_AFTER_MS, useStreamStore } from '../stores/stream'
import { deleteKey, rebuildKey, stopKey } from '../stores/workspaces'

useMockApi()
afterEach(() => vi.useRealTimers())

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
type Backend = ReturnType<typeof freshBackend>
const runningCard = (w: Wrapper, id: string) =>
  w.findAll('[data-test="running-row"]').find((r) => r.find('[data-test="ws-link"]').attributes('href') === `/ws/${id}`)!
const btn = (scope: { find: Wrapper['find'] }, name: string) => scope.find(`[data-test="${name}"] [data-test="action"]`)
const sent = (b: Backend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path)

describe('Stop', () => {
  it('is the running card\'s action, and only a running one\'s', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    // Control: the running workspace offers Stop.
    expect(btn(runningCard(wrapper, WS_RUNNING), 'stop').exists()).toBe(true)
    // The failed one offers Rebuild instead — never a Stop, disabled or not.
    const failed = runningCard(wrapper, WS_FAILED)
    expect(failed.find('[data-test="stop"]').exists()).toBe(false)
    expect(btn(failed, 'rebuild').exists()).toBe(true)
    // And in the catalog, the stopped one (scratch) offers nothing that stops.
    const scratch = wrapper.findAll('[data-test="repo"]').find((r) => r.find('.name').text() === 'krelinga/scratch')!
    expect(scratch.find('[data-test="stop"]').exists()).toBe(false)
  })

  it('stays in flight through its sub-steps, says which, and settles on stopped', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper, pinia } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)
    await btn(runningCard(wrapper, WS_RUNNING), 'stop').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/stop`).length).toBe(1)
    expect(btn(runningCard(wrapper, WS_RUNNING), 'stop').attributes('disabled')).toBeDefined()
    playScript(b, WS_RUNNING, 3) // session_server started, done; container started
    await settle()
    // A sub-step is not the outcome: still in flight, and the card says where it is.
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(true)
    expect(runningCard(wrapper, WS_RUNNING).find('[data-test="running-state"]').text()).toBe('Stopping · stopping the container…')
    playScript(b, WS_RUNNING)
    await settle()
    expect(stopKey(WS_RUNNING) in stream.inFlight).toBe(false)
    // Stopped leaves the Running section; its catalog row offers Start.
    expect(wrapper.findAll('[data-test="running-row"]').some((r) => r.text().includes('krelinga/drydock'))).toBe(false)
    const row = wrapper.findAll('[data-test="repo"]').find((r) => r.find('.name').text() === 'krelinga/drydock')!
    expect(row.find('[data-test="repo-state"]').text()).toBe('Stopped')
    expect(btn(row, 'start').exists()).toBe(true)
  })

  it('a stop that fails settles, keeps the workspace running, and offers Stop again', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', failAction: 'container' })
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    await btn(wrapper.find('[data-test="ws-card"]'), 'stop').trigger('click')
    await settle()
    playScript(b, WS_RUNNING)
    await settle()
    expect(stopKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Stop failed while stopping the container')
    expect(wrapper.find('[data-test="ws-note"]').text()).toBe("docker could not stop the workspace's container.")
    expect(wrapper.find('[data-test="action-run"] [data-step="container"] [data-test="step-status"]').text()).toBe('failed')
    // Control: the button is back and works — the second stop is accepted.
    expect(btn(wrapper.find('[data-test="ws-card"]'), 'stop').attributes('disabled')).toBeUndefined()
    await btn(wrapper.find('[data-test="ws-card"]'), 'stop').trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/stop`).length).toBe(2)
    expect(wrapper.find('[data-test="action-error"]').exists()).toBe(false)
  })

  it('says "no response yet" after ten seconds, and never turns it into a failure', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp('/')
    FakeEventSource.latest().open().pipe(b)
    await btn(runningCard(wrapper, WS_RUNNING), 'stop').trigger('click')
    await settle()
    expect(runningCard(wrapper, WS_RUNNING).find('[data-test="action-slow"]').exists()).toBe(false)
    vi.advanceTimersByTime(SLOW_AFTER_MS + 10 * 60_000)
    await settle()
    expect(runningCard(wrapper, WS_RUNNING).find('[data-test="action-slow"]').text()).toContain('No response yet')
    expect(runningCard(wrapper, WS_RUNNING).find('[data-test="action-error"]').exists()).toBe(false)
    expect(btn(runningCard(wrapper, WS_RUNNING), 'stop').attributes('disabled')).toBeDefined()
  })
})

describe('Rebuild', () => {
  it('is offered beside Stop on a running workspace, and stays in flight until it leaves building', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)
    const rebuild = () => btn(wrapper.find('[data-test="rebuild-block"]'), 'rebuild')
    expect(rebuild().exists()).toBe(true)
    await rebuild().trigger('click')
    await settle()
    expect(sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/rebuild`).length).toBe(1)
    playScript(b, WS_RUNNING, 3) // building, resolve_config started and done
    await settle()
    // Into building is the server accepting it, not the outcome.
    expect(rebuildKey(WS_RUNNING) in stream.inFlight).toBe(true)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Building')
    // Nothing to press while it builds but Delete (which would cancel it).
    expect(wrapper.find('[data-test="rebuild-block"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="ws-card"] [data-test="stop"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="delete"]').exists()).toBe(true)
    playScript(b, WS_RUNNING)
    await settle()
    expect(rebuildKey(WS_RUNNING) in stream.inFlight).toBe(false)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Running')
  })

  it('at the cap a stopped workspace\'s rebuild is refused with the cap sentence; with room it is accepted', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', capacity: 1 })
    const { wrapper } = await mountApp(`/ws/${WS_REMOVED}`)
    const rebuild = () => btn(wrapper.find('[data-test="rebuild-block"]'), 'rebuild')
    await rebuild().trigger('click')
    await settle()
    expect(wrapper.find('[data-test="rebuild-block"] [data-test="action-error"]').text())
      .toContain('Stop one under Running to make room — its clone survives, and Start brings it back.')
    b.capacity = 4
    await rebuild().trigger('click')
    await settle()
    expect(wrapper.find('[data-test="rebuild-block"] [data-test="action-error"]').exists()).toBe(false)
    expect(sent(b, 'POST', `/api/workspaces/${WS_REMOVED}/rebuild`).length).toBe(2)
  })
})

describe('Delete and its confirm (§6.5)', () => {
  const open = async (w: Wrapper) => {
    await w.find('[data-test="delete"]').trigger('click')
    await settle()
  }
  const confirmBtn = (w: Wrapper) => btn(w.find('[data-test="confirm-delete"]'), 'delete-confirm')

  it('says what goes and what survives, asks for no password, and enables only on the exact name', async () => {
    freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(wrapper.find('[data-test="confirm-delete"]').exists()).toBe(false)
    await open(wrapper)
    const sheet = wrapper.find('[data-test="confirm-delete"]')
    expect(sheet.find('[data-test="delete-goes"]').text()).toMatch(/container.*clone.*unpushed work/s)
    expect(sheet.find('[data-test="delete-survives"]').text()).toContain('Nothing of the workspace survives')
    expect(sheet.find('[data-test="delete-survives"]').text()).toContain('The repository on GitHub is untouched')
    expect(sheet.text()).toContain('krelinga/drydock')
    // No re-auth (design §13.5): the typed name is the only field.
    expect(sheet.findAll('input').length).toBe(1)
    expect(sheet.find('input[type="password"]').exists()).toBe(false)
    expect(document.activeElement).toBe(sheet.find('[data-test="delete-input"]').element)

    const input = sheet.find('[data-test="delete-input"]')
    for (const near of ['', 'krelinga/drydoc', ' krelinga/drydock', 'krelinga/drydock ', 'Krelinga/drydock', 'KRELINGA/DRYDOCK', 'drydock']) {
      await input.setValue(near)
      expect(confirmBtn(wrapper).attributes('disabled'), JSON.stringify(near)).toBeDefined()
    }
    // Control: the exact name enables it.
    await input.setValue('krelinga/drydock')
    expect(confirmBtn(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('sends exactly what was typed, stays in flight past the 202 and the move to deleting, and settles on the gone', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)
    await open(wrapper)
    await wrapper.find('[data-test="delete-input"]').setValue('krelinga/drydock')
    await confirmBtn(wrapper).trigger('click')
    await settle()
    const req = sent(b, 'DELETE', `/api/workspaces/${WS_RUNNING}`)
    expect(req.length).toBe(1)
    expect(new URL(req[0]!.url).search).toBe('?confirm=krelinga%2Fdrydock')
    // The 202 is not the outcome.
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(true)
    playScript(b, WS_RUNNING, 3) // deleting, session_server started, done
    await settle()
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(true)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Deleting · stopping the session server…')
    expect(confirmBtn(wrapper).attributes('aria-busy')).toBe('true')
    expect(wrapper.find('[data-test="delete-input"]').attributes('disabled')).toBeDefined()
    playScript(b, WS_RUNNING)
    await settle()
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(false)
    expect(wrapper.find('[data-test="ws-deleted"]').exists()).toBe(true)
  })

  it('a confirm the server refuses says why, and deletes nothing', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    await open(wrapper)
    await wrapper.find('[data-test="delete-input"]').setValue('krelinga/drydock')
    // The repository was renamed on the server since this page loaded.
    b.repos = b.repos.map((r) => (r.id === 1 ? { ...r, full_name: 'krelinga/drydock-renamed' } : r))
    await confirmBtn(wrapper).trigger('click')
    await settle()
    expect(wrapper.find('[data-test="confirm-delete"] [data-test="action-error"]').text()).toContain('nothing was deleted')
    expect(deleteKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    expect(b.workspaces[WS_RUNNING]?.state).toBe('running')
    // Control: the name the server now has is accepted.
    await wrapper.find('[data-test="delete-input"]').setValue('krelinga/drydock-renamed')
    // The page still knows the old name, so its own check holds the button —
    // and the server, not the page, is what decided.
    expect(confirmBtn(wrapper).attributes('disabled')).toBeDefined()
  })

  it('a delete that sticks settles, names its sub-step, and Delete again resumes it to the gone', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', failAction: 'files' })
    const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().pipe(b)
    const stream = useStreamStore(pinia)
    await open(wrapper)
    await wrapper.find('[data-test="delete-input"]').setValue('krelinga/drydock')
    await confirmBtn(wrapper).trigger('click')
    await settle()
    const script = b.scripts[WS_RUNNING]!
    playScript(b, WS_RUNNING, script.length - 1) // everything but the annotation
    await settle()
    // The sub-step failed, but the stuck annotation has not landed: still in flight.
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(true)
    expect(wrapper.find('[data-test="delete-again"]').exists()).toBe(false)
    playScript(b, WS_RUNNING)
    await settle()
    expect(deleteKey(WS_RUNNING) in stream.inFlight).toBe(false)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Delete stopped part-way')
    expect(wrapper.find('[data-test="ws-note"]').text()).toContain('Delete again to retry.')
    expect(wrapper.find('[data-test="action-run"] [data-step="files"] [data-test="step-status"]').text()).toBe('failed')
    expect(wrapper.find('[data-test="action-run"] [data-step="containers"] [data-test="step-status"]').text()).toBe('done')
    // The sheet has done its job; the card's own action is the one that can work.
    expect(wrapper.find('[data-test="confirm-delete"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="delete"]').exists()).toBe(false)
    const again = btn(wrapper.find('[data-test="ws-card"]'), 'delete-again')
    expect(again.attributes('disabled')).toBeUndefined()

    await again.trigger('click')
    await settle()
    const reqs = sent(b, 'DELETE', `/api/workspaces/${WS_RUNNING}`)
    expect(reqs.length).toBe(2)
    expect(new URL(reqs[1]!.url).searchParams.get('confirm')).toBe('krelinga/drydock')
    playScript(b, WS_RUNNING, 2) // the resume's first sub-step
    await settle()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Deleting · stopping the session server…')
    expect(wrapper.find('[data-test="ws-card"] [data-test="delete-again"]').exists()).toBe(false)
    playScript(b, WS_RUNNING)
    await settle()
    expect(wrapper.find('[data-test="ws-deleted"]').exists()).toBe(true)
  })

  it('a stuck delete is shown under Running, with Delete again, after a reload', async () => {
    const b = freshBackend({ signedIn: true, scriptMode: 'manual', failAction: 'containers' })
    const first = await mountApp(`/ws/${WS_RUNNING}`)
    await open(first.wrapper)
    await first.wrapper.find('[data-test="delete-input"]').setValue('krelinga/drydock')
    await confirmBtn(first.wrapper).trigger('click')
    await settle()
    playScript(b, WS_RUNNING)
    // A different page, loaded cold after it stuck.
    const { wrapper } = await mountApp('/')
    const card = runningCard(wrapper, WS_RUNNING)
    expect(card.find('[data-test="running-state"]').text()).toBe('Delete stopped part-way')
    expect(btn(card, 'delete-again').exists()).toBe(true)
    // Control: the catalog row offers the same, and no Clone.
    const row = wrapper.findAll('[data-test="repo"]').find((r) => r.find('.name').text() === 'krelinga/drydock')!
    expect(btn(row, 'delete-again').exists()).toBe(true)
    expect(row.find('[data-test="clone"]').exists()).toBe(false)
  })
})
