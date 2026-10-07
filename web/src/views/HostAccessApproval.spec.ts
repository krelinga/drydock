// Design §6's host-access approval through the UI, against the pretend
// backend: a rebuild whose configuration asks for host access stops, stopped,
// with the request on the card; Approve and continue sends the request's own
// hash and stays in flight until the build it continues ends; a stale hash is
// refused by code; Cancel takes the request away and leaves Start. Every
// negative assertion sits beside a positive control.

import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { playScript, WS_RUNNING } from '../mocks/backend'
import { useStreamStore } from '../stores/stream'
import { approveKey } from '../stores/workspaces'
import type { HostSettingView } from '../api/types'

useMockApi()

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
type Backend = ReturnType<typeof freshBackend>
const card = (w: Wrapper) => w.find('[data-test="ws-card"]')
const btn = (scope: { find: Wrapper['find'] }, name: string) => scope.find(`[data-test="${name}"] [data-test="action"]`)
const sent = (b: Backend, method: string, path: string) =>
  b.log.filter((r) => r.method === method && new URL(r.url).pathname === path)

const DIND: HostSettingView[] = [
  { field: 'privileged', source: 'feature_or_image', value: true },
  { field: 'runArgs', source: 'repository', value: ['--network=host'] },
]

/** A running workspace rebuilt after its configuration grew host access. */
async function askedOnRebuild(settings = DIND) {
  const b = freshBackend({ signedIn: true, scriptMode: 'manual' })
  const repo = b.workspaces[WS_RUNNING]!.repository_id
  b.hostAccess[repo] = { hash: 'sha256:' + 'a'.repeat(64), settings }
  const { wrapper, pinia } = await mountApp(`/ws/${WS_RUNNING}`)
  FakeEventSource.latest().open().pipe(b)
  await btn(wrapper, 'rebuild').trigger('click')
  await settle()
  playScript(b, WS_RUNNING)
  await settle()
  return { b, wrapper, pinia, repo }
}

describe('Host-access approval', () => {
  it('a rebuild that asks stops, says so, and shows exactly what it asks for', async () => {
    const { wrapper } = await askedOnRebuild()
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Needs approval')
    const panel = card(wrapper).find('[data-test="approval"]')
    expect(panel.exists()).toBe(true)
    expect(panel.text()).toContain('this configuration asks for host access')
    const items = panel.findAll('[data-test="approval-setting"]').map((i) => i.text())
    expect(items).toHaveLength(2)
    expect(items[0]).toContain('privileged')
    expect(items[0]).toContain('a Feature or the image')
    expect(items[1]).toContain('["--network=host"]')
    // privileged is named as root on the host.
    expect(panel.find('[data-test="approval-privileged"]').exists()).toBe(true)
    // The step is not a failure.
    expect(wrapper.find('[data-step="resolve_config"] [data-test="step-status"]').text()).toBe('needs approval')
    // No Start beside it: the request is the action.
    expect(card(wrapper).find('[data-test="start"]').exists()).toBe(false)
  })

  it('a request without privileged has no root warning (control)', async () => {
    const { wrapper } = await askedOnRebuild([{ field: 'runArgs', source: 'repository', value: ['--network=host'] }])
    const panel = card(wrapper).find('[data-test="approval"]')
    expect(panel.exists()).toBe(true)
    expect(panel.find('[data-test="approval-privileged"]').exists()).toBe(false)
  })

  it('Approve and continue sends the shown hash and settles when the build ends, not on the 202', async () => {
    const { b, wrapper, pinia } = await askedOnRebuild()
    const stream = useStreamStore(pinia)
    await btn(card(wrapper), 'approve').trigger('click')
    await settle()
    const reqs = sent(b, 'POST', `/api/workspaces/${WS_RUNNING}/config-approval`)
    expect(reqs).toHaveLength(1)
    expect(JSON.parse(b.approvalBodies[0]!)).toEqual({ hash: 'sha256:' + 'a'.repeat(64) })
    // Accepted, and the build it continues is under way: still in flight.
    expect(approveKey(WS_RUNNING) in stream.inFlight).toBe(true)
    playScript(b, WS_RUNNING)
    await settle()
    expect(approveKey(WS_RUNNING) in stream.inFlight).toBe(false)
    expect(wrapper.find('[data-test="ws-state"]').text()).not.toBe('Needs approval')
    expect(card(wrapper).find('[data-test="approval"]').exists()).toBe(false)
    // The same subset on the next rebuild: no request.
    await btn(wrapper, 'rebuild').trigger('click')
    await settle()
    playScript(b, WS_RUNNING)
    await settle()
    expect(card(wrapper).find('[data-test="approval"]').exists()).toBe(false)
    // Less than was approved: no request either. More: one, naming only what is new.
    const repo = b.workspaces[WS_RUNNING]!.repository_id
    b.hostAccess[repo] = { hash: 'sha256:' + 'c'.repeat(64), settings: [DIND[0]!] }
    await btn(wrapper, 'rebuild').trigger('click')
    await settle()
    playScript(b, WS_RUNNING)
    await settle()
    expect(card(wrapper).find('[data-test="approval"]').exists()).toBe(false)
    b.hostAccess[repo] = { hash: 'sha256:' + 'd'.repeat(64), settings: [...DIND, { field: 'appPort', source: 'repository', value: [80] }] }
    await btn(wrapper, 'rebuild').trigger('click')
    await settle()
    playScript(b, WS_RUNNING)
    await settle()
    const asked = card(wrapper).findAll('[data-test="approval-added"] [data-test="approval-setting"]').map((i) => i.text())
    expect(asked).toHaveLength(1)
    expect(asked[0]).toContain('appPort')
  })

  it('a configuration that changed after it was shown is refused as stale, with nothing approved', async () => {
    const { b, wrapper, pinia, repo } = await askedOnRebuild()
    // The server's request moved on (the configuration changed again).
    b.workspaces[WS_RUNNING]!.approval!.hash = 'sha256:' + 'b'.repeat(64)
    await btn(card(wrapper), 'approve').trigger('click')
    await settle()
    expect(card(wrapper).find('[data-test="action-error"]').text()).toContain('changed after this was shown')
    expect(approveKey(WS_RUNNING) in useStreamStore(pinia).inFlight).toBe(false)
    expect(b.approved[repo]).toBeUndefined()
    // Control: the panel is still there to review.
    expect(card(wrapper).find('[data-test="approval"]').exists()).toBe(true)
  })

  it('Cancel takes the request away, records nothing, and leaves Start, which asks again', async () => {
    const { b, wrapper, repo } = await askedOnRebuild()
    await btn(card(wrapper), 'decline').trigger('click')
    await settle()
    expect(sent(b, 'DELETE', `/api/workspaces/${WS_RUNNING}/config-approval`)).toHaveLength(1)
    expect(card(wrapper).find('[data-test="approval"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="ws-state"]').text()).toBe('Stopped')
    expect(b.approved[repo]).toBeUndefined()
    await btn(card(wrapper), 'start').trigger('click')
    await settle()
    playScript(b, WS_RUNNING)
    await settle()
    expect(card(wrapper).find('[data-test="approval"]').exists()).toBe(true)
  })

  it('reads the same from a cold load of the list (the request is on the row)', async () => {
    const { b } = await askedOnRebuild()
    const { wrapper } = await mountApp('/')
    const row = wrapper.findAll('[data-test="repo"]').find((r) => r.find('[data-test="repo-state"]').exists() &&
      r.find('[data-test="repo-state"]').text() === 'Needs approval')
    expect(row).toBeDefined()
    expect(row!.find('[data-test="approval"]').exists()).toBe(true)
    expect(b.workspaces[WS_RUNNING]!.approval).not.toBeNull()
  })
})
