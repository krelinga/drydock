// Host-access approval from the editor (review.ts; vscode-extension §10.4,
// §12.2's approval rows), over a Drydock in memory: the document is the web
// card's content; approve sends the hash of the snapshot the active document
// shows and nothing else; a new request cancels the box; the typed name is
// compared exactly; a stale hash reopens the document; decline is a modal.
// Each negative has its control beside it.

import { describe, expect, it } from 'vitest'
import { approvalSections, PRIVILEGED_SENTENCE, INITIALIZE_COMMAND_SENTENCE } from '../../web/src/core/lib/approval'
import { stateEvent } from '../../web/src/core/reducer.fixtures'
import { CONNECT } from './flows'
import { NO_APPROVE_SCOPE, NOT_APPROVED_NAME, REVIEW_IN_DRYDOCK, STALE, approve, decline, renderReview, reviewApproval, snapshotOf, uriOf } from './review'
import { hubItems } from './hub'
import { DIND_WS, HASH_A, HASH_B, LINK, REQ_A, REQ_B, event, flowWorld, reviewWorld, withPending, type ReviewWorld } from '../test/fakeDrydock'
import { ORIGIN, flush } from '../test/fakes'

const NAME = 'drydock-fixture/dind'

async function opened(remote?: string, req = REQ_A): Promise<ReviewWorld> {
  const f = flowWorld(remote)
  withPending(f, req)
  const r = reviewWorld(f)
  r.prompter.message = async function (level, text, actions = []) {
    this.shown.push({ level, text, actions })
    return undefined
  }
  await reviewApproval(r, DIND_WS)
  return r
}

const approvals = (r: ReviewWorld) => r.server.sent('POST', `/api/workspaces/${DIND_WS}/config-approval`)

describe('the review document', () => {
  it('opens beside the editor and says exactly what the web card says, the privileged sentence included', async () => {
    const r = await opened()
    expect(r.documents.shown).toEqual([uriOf(DIND_WS)])
    const text = r.reviews.text(DIND_WS)
    const sec = approvalSections(REQ_A)
    expect(sec.roots).toEqual([PRIVILEGED_SENTENCE])
    for (const line of [`Repository: ${NAME}`, `Request:    ${HASH_A}`, `! ${PRIVILEGED_SENTENCE}`, 'New', '  privileged  (from a Feature or the image)', '      true']) {
      expect(text).toContain(line)
    }
    expect(text).not.toContain(INITIALIZE_COMMAND_SENTENCE)
  })

  it('a value or name the repository wrote is escaped in the document', () => {
    const text = renderReview({
      workspaceId: DIND_WS, hash: HASH_A, version: 1, fullName: `evil‮name`, branch: 'b\nRequest: forged',
      view: { hash: HASH_A, added: [{ field: 'initializeCommand', source: 'repository', value: 'x‮y\nz' }], changed: [], removed: [] },
    }, DIND_WS)
    expect(text).not.toMatch(/[‮]/)
    expect(text).toContain('Repository: evil\\u202Ename')
    expect(text).toContain('Branch:     b\\u000ARequest: forged')
    expect(text.split('\n').filter((l) => l.startsWith('Request:'))).toHaveLength(1)
  })

  it('the hub offers Review for a waiting workspace, which opens the document and never approves', async () => {
    const f = flowWorld()
    withPending(f)
    const r = reviewWorld(f)
    const release = await r.live.hold()
    const item = hubItems(r.live.entities).find((i) => i.value.kind === 'review')!
    release()
    expect(item).toMatchObject({ value: { kind: 'review', id: DIND_WS } })
    r.prompter.pickAnswers = [hubItems(r.live.entities).findIndex((i) => i.value.kind === 'review')]
    const { go } = await import('./flows')
    await go(r)
    expect(r.documents.shown).toEqual([uriOf(DIND_WS)])
    expect(r.server.mutations()).toEqual([])
  })
})

describe('approve exactly what was shown', () => {
  it('the right name sends one POST with the snapshot\'s hash, then follows the run to running', async () => {
    const r = await opened()
    r.prompter.typed = [NAME]
    r.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return text.includes(' is running.') ? CONNECT : undefined
    }
    const done = approve(r)
    await flush()
    expect(approvals(r).map((q) => JSON.parse(q.body!))).toEqual([{ hash: HASH_A }])
    // The prompt repeats the root sentence, names the repository in prose, and is not pre-filled.
    expect(r.prompter.inputs[0]!.prompt).toContain(PRIVILEGED_SENTENCE)
    expect(r.prompter.inputs[0]!.prompt).toContain(NAME)
    expect(r.prompter.inputs[0]).not.toHaveProperty('placeHolder', NAME)
    r.server.workspaces = r.server.workspaces.map((w) => (w.id === DIND_WS ? { ...w, state: 'running', vscode: { configured: true, url: LINK } } : w))
    r.server.emit(stateEvent(400, DIND_WS, 'running', { from: 'building' }))
    await done
    expect(r.opener.folders).toHaveLength(1)
  })

  it('a stream lagging the 202 does not end the follow at the request just approved', async () => {
    const r = await opened()
    r.server.holdApproveEvents = true
    r.prompter.typed = [NAME]
    r.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return text.includes(' is running.') ? CONNECT : undefined
    }
    const done = approve(r)
    await flush()
    expect(approvals(r)).toHaveLength(1)
    // Still following: the entity still shows the approved request.
    expect(r.prompter.texts().filter((t) => t.includes('needs host-access approval'))).toEqual([])
    r.server.workspaces = r.server.workspaces.map((w) => (w.id === DIND_WS ? { ...w, state: 'running', approval: null, vscode: { configured: true, url: LINK } } : w))
    r.server.emit(event(500, 'config.approved', DIND_WS, { repository_id: 3, hash: HASH_A }), stateEvent(501, DIND_WS, 'running', { from: 'building' }))
    await done
    expect(r.opener.folders).toHaveLength(1)
  })

  it('the name must be exactly the full name: case, whitespace and the short name each send nothing', async () => {
    for (const wrong of ['Drydock-fixture/dind', ` ${NAME}`, `${NAME} `, 'dind', '']) {
      const r = await opened()
      r.prompter.typed = [wrong]
      await approve(r)
      expect(approvals(r), JSON.stringify(wrong)).toEqual([])
      expect(r.prompter.texts()).toContain(NOT_APPROVED_NAME)
    }
  })

  it('arguments are ignored: a hash or workspace passed in changes nothing (the dispatcher passes none, and approve reads none)', async () => {
    const r = await opened()
    r.prompter.typed = [NAME]
    r.prompter.dismissNow = true
    await (approve as (d: ReviewWorld, ...rest: unknown[]) => Promise<void>)(r, { hash: HASH_B, workspace: 'other' })
    expect(approvals(r).map((q) => JSON.parse(q.body!))).toEqual([{ hash: HASH_A }])
  })

  it('with the entity moved on to B but the document still showing A, A is sent and the server refuses; the document reopens on B', async () => {
    const r = await opened()
    // The server's request is B; the client has not seen it.
    r.server.pending = { id: DIND_WS, hash: HASH_B }
    r.server.workspaces = r.server.workspaces.map((w) => (w.id === DIND_WS ? { ...w, approval: REQ_B } : w))
    r.prompter.typed = [NAME]
    await approve(r)
    expect(approvals(r).map((q) => JSON.parse(q.body!))).toEqual([{ hash: HASH_A }])
    expect(r.prompter.texts()).toContain(STALE)
    expect(r.documents.shown).toEqual([uriOf(DIND_WS), uriOf(DIND_WS)])
    expect(r.reviews.snapshot(DIND_WS)?.hash).toBe(HASH_B)
    expect(r.reviews.text(DIND_WS)).toContain(INITIALIZE_COMMAND_SENTENCE)
  })

  it('a new request while the box is open cancels it and sends nothing; the document re-renders on B', async () => {
    const r = await opened()
    let release!: (v: string | undefined) => void
    r.prompter.inputGate = new Promise((res) => { release = res })
    const done = approve(r)
    await flush()
    expect(r.prompter.inputs).toHaveLength(1)
    r.server.emit(event(401, 'workspace.state', DIND_WS, { state: 'stopped', from: 'stopped', approval: REQ_B }))
    await done
    release(NAME)
    expect(approvals(r)).toEqual([])
    expect(r.documents.changes).toContain(uriOf(DIND_WS))
    expect(r.reviews.snapshot(DIND_WS)?.hash).toBe(HASH_B)
  })

  it('with no review document active, approve opens the pick and the document, never the input box', async () => {
    const f = flowWorld()
    withPending(f)
    const r = reviewWorld(f)
    r.prompter.pickAnswers = [0]
    r.prompter.typed = [NAME]
    await approve(r)
    expect(r.prompter.inputs).toEqual([])
    expect(r.documents.shown).toEqual([uriOf(DIND_WS)])
    expect(approvals(r)).toEqual([])
  })

  it('a token without approve: the action is Review in Drydock, and nothing is sent', async () => {
    const f = flowWorld()
    withPending(f)
    f.server.scopes = ['read', 'lifecycle', 'self']
    const r = reviewWorld(f)
    r.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      return actions.includes(REVIEW_IN_DRYDOCK) ? REVIEW_IN_DRYDOCK : undefined
    }
    await reviewApproval(r, DIND_WS)
    r.prompter.typed = [NAME]
    await approve(r)
    expect(approvals(r)).toEqual([])
    expect(r.prompter.inputs).toEqual([])
    expect(r.prompter.texts()).toContain(NO_APPROVE_SCOPE)
    expect(r.opener.opened).toEqual([`${ORIGIN}/ws/${DIND_WS}`])
  })
})

describe('decline', () => {
  it('a modal, then DELETE; declined at the modal, nothing is sent (control)', async () => {
    const no = await opened()
    no.prompter.confirms = [false]
    await decline(no)
    expect(no.server.mutations()).toEqual([])
    const yes = await opened()
    yes.prompter.confirms = [true]
    await decline(yes)
    expect(yes.server.sent('DELETE', `/api/workspaces/${DIND_WS}/config-approval`)).toHaveLength(1)
    await flush()
    expect(snapshotOf(yes.live.entities, DIND_WS)).toBeNull()
  })
})
