// Open in VS Code on the card and the workspace page (design §6, "Opening a
// workspace in VS Code"), through the real app against the mock: a plain
// link when the server has one, a note on the page — never a disabled
// control — when it has no --vscode-ssh-host, and nothing when it says
// nothing.

import { describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { FakeEventSource } from '../test/fakeEventSource'
import { freshBackend, mountApp, server, settle, useMockApi } from '../test/setup'
import { emit, WS_FAILED, WS_RUNNING, workspaceList } from '../mocks/backend'

useMockApi()

const GOOD =
  'vscode://vscode-remote/attached-container+7b22636f6e7461696e65724e616d65223a222f6d6f636b5f31227d' +
  '@ssh-remote+7b22686f73744e616d65223a22646576626f782e6c616e222c2275736572223a226f776e6572227d/workspaces/repo1'

type Wrapper = Awaited<ReturnType<typeof mountApp>>['wrapper']
const card = (w: Wrapper) => w.find('[data-test="running-row"]')
const page = (w: Wrapper) => w.find('[data-test="ws-card"]')
/** Nothing VS Code-ish rendered as a control: no button, nothing disabled, no anchor. */
const noControl = (w: ReturnType<Wrapper['find']>) => {
  expect(w.find('[data-test="open-vscode"]').exists()).toBe(false)
  expect(w.findAll('button').filter((b) => /vs ?code/i.test(b.text()))).toHaveLength(0)
  expect(w.findAll('[disabled], [aria-disabled="true"]').filter((b) => /vs ?code/i.test(b.text()))).toHaveLength(0)
  expect(w.findAll('a').filter((a) => /vs ?code/i.test(a.text()))).toHaveLength(0)
}

describe('Open in VS Code', () => {
  it('is a plain link with the server’s URL, on the card and on the page', async () => {
    freshBackend({ signedIn: true, vscodeHost: 'owner@devbox.lan' })
    const home = await mountApp('/')
    const a = card(home.wrapper).find('[data-test="open-vscode"]')
    expect(a.element.tagName).toBe('A')
    expect(a.attributes('href')).toBe(GOOD)
    expect(a.attributes('disabled')).toBeUndefined()
    expect(a.text()).toBe('Open in VS Code')

    const detail = await mountApp(`/ws/${WS_RUNNING}`)
    const b = page(detail.wrapper).find('[data-test="open-vscode"]')
    expect(b.element.tagName).toBe('A')
    expect(b.attributes('href')).toBe(GOOD)
    expect(page(detail.wrapper).find('[data-test="vscode-off"]').exists()).toBe(false)
  })

  it('is not offered for a workspace that is not running', async () => {
    freshBackend({ signedIn: true, vscodeHost: 'owner@devbox.lan' })
    const { wrapper } = await mountApp(`/ws/${WS_FAILED}`)
    noControl(page(wrapper))
    expect(page(wrapper).find('[data-test="vscode-off"]').exists()).toBe(false)
  })

  it('unconfigured: the page says how to turn it on, the card says nothing, and nothing is disabled', async () => {
    freshBackend({ signedIn: true, vscodeHost: null })
    const detail = await mountApp(`/ws/${WS_RUNNING}`)
    const note = page(detail.wrapper).find('[data-test="vscode-off"]')
    expect(note.text()).toBe(
      'Open in VS Code is off: start Drydock with --vscode-ssh-host USER@HOST (the installer\'s --vscode-ssh-host). ' +
      'The SSH user needs Docker access.')
    noControl(page(detail.wrapper))

    const home = await mountApp('/')
    expect(card(home.wrapper).exists()).toBe(true) // control: the card is there
    noControl(card(home.wrapper))
    expect(card(home.wrapper).find('[data-test="vscode-off"]').exists()).toBe(false)
  })

  it('a server that says nothing gets nothing', async () => {
    freshBackend({ signedIn: true })
    const detail = await mountApp(`/ws/${WS_RUNNING}`)
    noControl(page(detail.wrapper))
    expect(page(detail.wrapper).find('[data-test="vscode-off"]').exists()).toBe(false)
  })

  it('never renders a URL from the wire that internal/vscode could not have built', async () => {
    const b = freshBackend({ signedIn: true, vscodeHost: 'owner@devbox.lan' })
    for (const bad of ['javascript:alert(1)', 'https://evil.example/', GOOD + '"><img src=x>']) {
      server.use(http.get('/api/workspaces', () => {
        const body = workspaceList(b)
        for (const v of body.workspaces) if (v.vscode) v.vscode = { configured: true, url: bad }
        return HttpResponse.json(body)
      }))
      const { wrapper } = await mountApp('/')
      expect(card(wrapper).exists(), bad).toBe(true)
      noControl(card(wrapper))
      expect(wrapper.html()).not.toContain('javascript:')
      expect(wrapper.html()).not.toContain('evil.example')
    }
  })

  it('a link older than a move to running is hidden, and the move refetches a fresh one', async () => {
    const b = freshBackend({ signedIn: true, vscodeHost: 'owner@devbox.lan' })
    const { wrapper } = await mountApp(`/ws/${WS_RUNNING}`)
    expect(page(wrapper).find('[data-test="open-vscode"]').attributes('href')).toBe(GOOD)
    // The detail GET fails until `answer` is set: what is shown meanwhile
    // comes from what the entities hold, and a stale link is not among it.
    let reads = 0
    let answer = false
    server.use(http.get(`/api/workspaces/${WS_RUNNING}`, () => {
      reads++
      if (!answer) return HttpResponse.json({}, { status: 500 })
      const v = workspaceList(b).workspaces.find((w) => w.id === WS_RUNNING)!
      return HttpResponse.json({ ...v, events: [] })
    }))
    FakeEventSource.latest().open().pipe(b)
    await settle()
    const opened = reads // the stream's first open refetches by itself
    emit(b, 'workspace.state', { workspace_id: WS_RUNNING, message: 'Rebuilding.', data: { state: 'building', from: 'running' } })
    await settle()
    expect(reads).toBe(opened) // control: a move to building asks nothing
    emit(b, 'workspace.state', { workspace_id: WS_RUNNING, message: 'Running.', data: { state: 'running', from: 'building', container_id: 'c0ffee99' } })
    await settle()
    expect(page(wrapper).find('[data-test="ws-state"]').text()).toBe('Running')
    expect(page(wrapper).find('[data-test="open-vscode"]').exists()).toBe(false)
    expect(reads).toBe(opened + 1) // the move to running asked again
    // The next move to running is answered: the fresh link is shown.
    answer = true
    emit(b, 'workspace.state', { workspace_id: WS_RUNNING, message: 'Running.', data: { state: 'running', from: 'running' } })
    await settle()
    expect(page(wrapper).find('[data-test="open-vscode"]').attributes('href')).toBe(GOOD)
  })
})
