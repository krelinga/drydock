// Design §12's failure rows that the UI states, each provoked here with its
// positive control: the build output and the clone kept for a failed build;
// GitHub access in §12's sentences, never a git error; a workspace whose
// repository left the installation badged read-only; and failed sign-ins
// reported once after the sign-in that learned of them.

import { describe, expect, it } from 'vitest'
import type { VueWrapper } from '@vue/test-utils'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, settle, useMockApi } from '../test/setup'
import { MOCK_PASSWORD, WS_FAILED, WS_REMOVED, WS_RUNNING } from '../mocks/backend'
import type { StreamEvent } from '../api/types'

useMockApi()

const tokenEvent = (id: number, kind: string, reason?: string): StreamEvent => ({
  id, kind, workspace_id: WS_RUNNING, level: kind === 'token.refused' ? 'warn' : 'info',
  message: 'server prose about git', at: new Date(Date.UTC(2026, 9, 8, 12, 0, 0)).toISOString(),
  data: reason ? { scope: 'git', reason } : { scope: 'git' },
})

describe('a failed build', () => {
  it('shows the build’s last lines and says the clone is kept', async () => {
    const b = freshBackend({ signedIn: true })
    b.workspaces[WS_FAILED]!.buildLog = { lines: ['Step 3/9 : RUN make', 'make: *** [all] Error 2'], at: null, held: true }
    const { wrapper } = await mountApp(`/ws/${WS_FAILED}`)
    const log = wrapper.find('[data-test="build-log"]')
    expect(log.text()).toContain('make: *** [all] Error 2')
    expect(log.text()).toContain('The clone is kept')
  })

  it('shows nothing when the server holds none, nor for a running workspace', async () => {
    freshBackend({ signedIn: true })
    const failed = await mountApp(`/ws/${WS_FAILED}`)
    expect(failed.wrapper.find('[data-test="build-log"]').exists()).toBe(false)
    const b = freshBackend({ signedIn: true })
    b.workspaces[WS_RUNNING]!.buildLog = { lines: ['old'], at: null, held: true }
    const running = await mountApp(`/ws/${WS_RUNNING}`)
    expect(running.wrapper.find('[data-test="build-log"]').exists()).toBe(false)
    expect(b.log.some((r) => r.url.endsWith(`/${WS_RUNNING}/build-log`))).toBe(false)
  })
})

describe('GitHub access', () => {
  async function detailAfter(ev: StreamEvent): Promise<VueWrapper> {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    FakeEventSource.latest().open().send(ev)
    await settle()
    return wrapper
  }

  it('a rate limit says GitHub is refusing requests, with no retry', async () => {
    const w = await detailAfter(tokenEvent(900, 'token.refused', 'rate_limited'))
    const line = w.find('[data-test="github-access-line"]')
    expect(line.text()).toContain('GitHub is refusing requests')
    expect(line.text()).not.toMatch(/try again/i)
    expect(w.find('[data-test="github-access"] button').exists()).toBe(false) // never a retry that will fail too
    expect(line.text()).not.toContain('server prose')
    expect(line.classes()).toContain('bad')
  })

  it('a revocation says read-only; an issued token, the control, says working', async () => {
    const revoked = await detailAfter(tokenEvent(900, 'token.refused', 'revoked'))
    expect(revoked.find('[data-test="github-access-line"]').text()).toContain('read-only')
    const ok = await detailAfter(tokenEvent(901, 'token.issued'))
    const line = ok.find('[data-test="github-access-line"]')
    expect(line.text()).toContain('Working')
    expect(line.text()).not.toContain('refusing')
  })
})

describe('a repository removed from the installation', () => {
  it('badges its workspace read-only, with the installation settings', async () => {
    freshBackend({ signedIn: true })
    const { wrapper } = await mountApp(`/ws/${WS_REMOVED}`)
    const note = wrapper.find('[data-test="read-only"]')
    expect(note.text()).toContain('Unpushed work in the working tree survives')
    expect(note.find('a').attributes('href')).toBe('https://github.com/settings/installations/101')
    // Control: a covered repository's workspace carries none.
    freshBackend({ signedIn: true })
    const other = await mountApp(`/ws/${WS_RUNNING}`)
    expect(other.wrapper.find('[data-test="read-only"]').exists()).toBe(false)
  })
})

describe('failed sign-ins', () => {
  async function signIn(wrapper: VueWrapper, pw: string): Promise<void> {
    await wrapper.find('#signin-password').setValue(pw)
    await wrapper.find('form').trigger('submit')
    await settle()
  }

  it('are reported once after the sign-in that learned of them, and dismissed', async () => {
    freshBackend()
    const { wrapper } = await mountApp('/signin')
    await signIn(wrapper, 'wrong')
    await signIn(wrapper, 'wrong again')
    await signIn(wrapper, MOCK_PASSWORD)
    const notice = wrapper.find('[data-test="failed-sign-ins"]')
    expect(notice.text()).toContain('2 failed sign-ins since the last successful one, from 192.0.2.66')
    expect(notice.find('a').attributes('href')).toBe('/settings')
    await notice.find('[data-test="failed-sign-ins-dismiss"]').trigger('click')
    expect(wrapper.find('[data-test="failed-sign-ins"]').exists()).toBe(false)
  })

  it('say nothing when nothing failed', async () => {
    freshBackend()
    const { wrapper } = await mountApp('/signin')
    await signIn(wrapper, MOCK_PASSWORD)
    expect(wrapper.find('[data-test="failed-sign-ins"]').exists()).toBe(false)
    expect(wrapper.find('h1').exists()).toBe(true)
  })
})
