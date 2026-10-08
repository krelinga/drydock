// Memory and disk on the card, and the disk-full banner and refusal (design
// §6 *Resources*, §12 *Disk full*; frontend §6.1, §9), through the real app:
// the list's copy on load, then the stream's named `resources` frame.

import { describe, expect, it } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { WS_RUNNING } from '../mocks/backend'
import type { HostDiskView, ResourcesView } from '../api/types'

useMockApi()

const T = (s: number) => new Date(Date.UTC(2026, 9, 8, 12, 0, s)).toISOString()
const res = (round: number, mem: number | null, disk: number): ResourcesView => ({
  boot: 'b00t', round,
  memory: mem === null ? null : { bytes: mem, at: T(round), stale: false },
  disk: { bytes: disk, directory_bytes: disk, container_bytes: null, partial: false, at: T(round), stale: false },
})
const host = (round: number, used: number): HostDiskView =>
  ({ used_bytes: used, total_bytes: 100, limit_percent: 90, over: used >= 90, at: T(round), boot: 'b00t', round })

function backend() {
  const b = freshBackend({ signedIn: true })
  b.workspaces[WS_RUNNING]!.resources = res(1, 1_200_000_000, 3_400_000_000)
  b.disk = host(1, 50)
  return b
}

describe('the card', () => {
  it('shows memory and disk from the list, then from each frame', async () => {
    backend()
    const { wrapper } = await mountApp('/')
    const card = () => wrapper.find('[data-test="running-row"] [data-test="resources"]')
    expect(card().text()).toContain('mem 1.2 GB · disk 3.4 GB')
    FakeEventSource.latest().open().named('resources', {
      boot: 'b00t', round: 2, at: T(2), workspaces: { [WS_RUNNING]: res(2, 2_500_000_000, 3_500_000_000) }, host: host(2, 50),
    })
    await settle()
    expect(card().text()).toContain('mem 2.5 GB · disk 3.5 GB')
    // An older round arriving late changes nothing.
    FakeEventSource.latest().named('resources', {
      boot: 'b00t', round: 1, at: T(1), workspaces: { [WS_RUNNING]: res(1, 1, 1) }, host: host(1, 50),
    })
    await settle()
    expect(card().text()).toContain('mem 2.5 GB')
  })

  it('a workspace never measured says so, never 0', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp('/')
    const line = wrapper.find('[data-test="running-row"] [data-test="resources"]')
    expect(line.text()).toContain('mem — · disk —')
    expect(line.text()).not.toMatch(/\b0 ?B\b/)
    // The failed one is in the catalog: no memory figure at all.
    const failedRow = wrapper.findAll('[data-test="repo"]')[1]!
    expect(failedRow.find('[data-test="resources"]').text()).toContain('disk —')
    expect(failedRow.find('[data-test="resources"]').text()).not.toContain('mem')
  })
})

describe('the disk banner', () => {
  it('appears when the workspace disk crosses the limit, listing the largest workspaces', async () => {
    backend()
    const { wrapper } = await mountApp('/')
    expect(wrapper.find('[data-test="disk-banner"]').exists()).toBe(false) // control: 50%
    FakeEventSource.latest().open().named('resources', {
      boot: 'b00t', round: 2, at: T(2), workspaces: { [WS_RUNNING]: res(2, 1_000_000_000, 12_000_000_000) }, host: host(2, 93),
    })
    await settle()
    const banner = wrapper.find('[data-test="disk-banner"]')
    expect(banner.text()).toContain('The workspace disk is 93% full.')
    expect(banner.text()).toContain('at 90%')
    expect(banner.text()).toContain('Delete a workspace you no longer need')
    expect(banner.find('[data-test="disk-largest"]').text()).toContain('12 GB')
    expect(banner.find('a').attributes('href')).toBe(`/ws/${WS_RUNNING}`)
    // Freed: the next round clears it.
    FakeEventSource.latest().named('resources', { boot: 'b00t', round: 3, at: T(3), workspaces: {}, host: host(3, 70) })
    await settle()
    expect(wrapper.find('[data-test="disk-banner"]').exists()).toBe(false)
  })
})

describe('the disk-full refusal', () => {
  it('a clone refused by the pre-flight names the disk and the delete that fixes it', async () => {
    const b = backend()
    b.disk = host(1, 93)
    const { wrapper } = await mountApp('/')
    expect(wrapper.find('[data-test="disk-banner"]').exists()).toBe(true) // from the list alone
    await wrapper.find('[data-test="clone"] [data-test="action"]').trigger('click')
    await settle()
    const err = wrapper.find('[data-test="action-error"]')
    expect(err.attributes('role')).toBe('alert')
    expect(err.text()).toContain('too full to clone or build another, so nothing was started')
    expect(err.text()).toContain('Delete a workspace you no longer need')
    expect(err.text()).toContain('93% full; Drydock refuses at 90%')
    // Control: with room, the same tap is accepted.
    b.disk = host(2, 50)
    await wrapper.find('[data-test="clone"] [data-test="action"]').trigger('click')
    await settle()
    expect(wrapper.find('[data-test="action-error"]').exists()).toBe(false)
  })
})
