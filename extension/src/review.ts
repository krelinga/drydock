// Host-access approvals from the editor (vscode-extension §10.4).
//
// *Review host access…* opens a read-only virtual document,
// `drydock-approval:/<workspace id>`, with exactly the web card's content
// (core's lib/approval.ts). It is rendered from a snapshot — the workspace,
// the request's hash and view, and the state version they came with — and
// `drydock.approve` reads the workspace and the hash **only from the active
// document's snapshot**: never from the entity at send time, never from a
// command's arguments. When the entity's request changes (a new hash, or
// none), the document re-renders from the new snapshot and an approval in
// progress is cancelled: the operator reads again and types again. A change
// the client has not seen is the server's `409 approval_stale`; the
// extension then refetches and reopens the document on the new request.
//
// Approving is confirmed by typing the repository's full name, compared
// exactly, in a box that is never pre-filled and never shows the name as a
// placeholder: an intent check that no stray keystroke can satisfy. No
// keybinding, hub item or notification approves. Declining is a plain modal.
// Both are mutations (§8 rule 1), and approving needs the token's `approve`
// scope; without it the action is *Review in Drydock*.

import type { ApprovalView } from '../../web/src/core/types'
import { ApiError } from '../../web/src/core/errors'
import {
  APPROVAL_CLOSING, APPROVAL_INTRO, APPROVAL_TITLE, approvalSections,
} from '../../web/src/core/lib/approval'
import { escapeFormat } from '../../web/src/core/lib/text'
import { OVER, overIn, settlesApprove, settlesDecline, settlesJobOr } from '../../web/src/core/settle'
import type { Entities } from '../../web/src/core/reducer'
import { follow, openInDrydock, type FlowDeps } from './flows'
import type { Cancel } from './ports'
import { pathFor } from './routes'

export const SCHEME = 'drydock-approval'
export const REVIEW_IN_DRYDOCK = 'Review in Drydock'
export const REVIEW = 'Review'
export const CANCEL_REQUEST = 'Cancel the request'
export const NOT_APPROVED_NAME = 'That is not this repository\'s full name, so nothing was approved.'
export const STALE = 'The configuration changed after it was shown, so nothing was approved. Review it again.'
export const NO_APPROVE_SCOPE =
  'This VS Code\'s Drydock token cannot approve host access (it was created without approve). Review and approve it in Drydock.'

/** What a review document shows, and what approving it sends. */
export interface Snapshot {
  workspaceId: string
  /** The request's hash: the one approving sends. */
  hash: string
  view: ApprovalView
  /** The state version the request came with (the reducer's stateAt). */
  version: number
  /** The repository's full name as the server wrote it: what must be typed. */
  fullName: string | null
  branch: string | null
}

export const uriOf = (id: string) => `${SCHEME}:/${id}`

/** The workspace id a review document's URI names, or null for any other URI. */
export function idOfURI(uri: string | undefined): string | null {
  if (uri === undefined) return null
  const m = new RegExp(`^${SCHEME}:/([0-9A-HJKMNP-TV-Z]{26})$`).exec(uri)
  return m === null ? null : m[1]!
}

/** The snapshot of workspace `id`'s pending request in `e`, or null when there is none. */
export function snapshotOf(e: Entities, id: string): Snapshot | null {
  const w = e.workspaces[id]
  if (w === undefined || e.gone[id] !== undefined || w.state !== 'stopped' || w.approval === null) return null
  const fullName = w.fullName ?? (w.repositoryId !== null ? e.repos[w.repositoryId]?.fullName ?? null : null)
  return { workspaceId: id, hash: w.approval.hash, view: w.approval, version: w.stateAt, fullName, branch: w.branch }
}

/** The review document's text: the web card's content, as text, every repository-written string escaped. */
export function renderReview(s: Snapshot | null, id: string): string {
  if (s === null) {
    return `${APPROVAL_TITLE.lead} nothing is pending for workspace ${id}.\n\nThe request was approved, cancelled or replaced. Review host access again to see what is pending now.\n`
  }
  const sec = approvalSections(s.view)
  const lines: string[] = [
    `${APPROVAL_TITLE.lead} ${APPROVAL_TITLE.rest}`,
    '',
    `Repository: ${escapeFormat(s.fullName ?? 'unknown')}`,
    ...(s.branch !== null ? [`Branch:     ${escapeFormat(s.branch)}`] : []),
    `Workspace:  ${s.workspaceId}`,
    `Request:    ${s.hash}`,
    '',
    `! ${APPROVAL_INTRO}`,
    ...sec.roots.map((r) => `! ${r}`),
    `! ${APPROVAL_CLOSING}`,
  ]
  for (const g of sec.groups) {
    lines.push('', g.label)
    for (const it of g.items) {
      lines.push(`  ${it.field}  (${it.source})`)
      for (const v of it.values) lines.push(`      ${v}`)
    }
  }
  lines.push(
    '',
    'To approve, run "Drydock: Approve Host Access…" from this document (or its title bar) and type the repository\'s full name.',
    'To cancel the request, run "Drydock: Cancel Host-Access Request". The workspace stays stopped; Start asks again.',
    '',
  )
  return lines.join('\n')
}

/**
 * The open review documents' snapshots. A snapshot changes only when the
 * entity's request does (another hash, or none); then the document is told
 * to re-render and an approval being typed for it is cancelled.
 */
export class Reviews {
  readonly #snaps = new Map<string, Snapshot | null>()
  readonly #cancels = new Map<string, Set<() => void>>()

  constructor(private readonly d: FlowDeps & { documents: DocumentsPort }) {
    d.live.onChange(() => this.#refresh())
  }

  /** The snapshot a document shows (taken now, the first time it is asked for). */
  snapshot(id: string): Snapshot | null {
    if (!this.#snaps.has(id)) this.#snaps.set(id, snapshotOf(this.d.live.entities, id))
    return this.#snaps.get(id) ?? null
  }

  /** The text of the document for workspace `id`. */
  text(id: string): string {
    return renderReview(this.snapshot(id), id)
  }

  /** Re-takes the snapshot of `id` from the entities now, as after a refetch. */
  retake(id: string): void {
    this.#set(id, snapshotOf(this.d.live.entities, id))
  }

  /** A Cancel that fires when `id`'s snapshot changes. */
  cancelOnChange(id: string): Cancel & { dispose(): void } {
    const fns = new Set<() => void>()
    let set = this.#cancels.get(id)
    if (set === undefined) this.#cancels.set(id, (set = new Set()))
    const own = () => {
      for (const f of fns) f()
    }
    set.add(own)
    return { onCancel: (f) => void fns.add(f), dispose: () => set!.delete(own) }
  }

  #refresh(): void {
    for (const id of this.#snaps.keys()) {
      const next = snapshotOf(this.d.live.entities, id)
      const cur = this.#snaps.get(id) ?? null
      if (cur?.hash !== next?.hash) this.#set(id, next)
    }
  }

  #set(id: string, next: Snapshot | null): void {
    const cur = this.#snaps.get(id) ?? null
    this.#snaps.set(id, next)
    if (cur?.hash !== next?.hash) {
      for (const c of this.#cancels.get(id) ?? []) c()
      this.d.documents.changed(uriOf(id))
    }
  }
}

/** Shows and reads the virtual documents. */
export interface DocumentsPort {
  /** Opens `uri` beside the active editor, read-only, and focuses it. */
  show(uri: string): Promise<void>
  /** The URI of the active editor's document, if any. */
  activeUri(): string | undefined
  /** `uri`'s content changed: VS Code asks for it again. */
  changed(uri: string): void
}

export interface ReviewDeps extends FlowDeps {
  documents: DocumentsPort
  reviews: Reviews
}

/** drydock.reviewApproval: open the review document for the tree item's workspace, or pick one waiting. */
export async function reviewApproval(d: ReviewDeps, target?: string): Promise<void> {
  const release = await d.live.hold()
  try {
    let id = target
    if (id === undefined) {
      const waiting = Object.keys(d.live.entities.workspaces).flatMap((wid) => {
        const s = snapshotOf(d.live.entities, wid)
        return s === null ? [] : [{ label: escapeFormat(s.fullName ?? wid), description: 'Needs host-access approval', value: wid }]
      })
      if (waiting.length === 0) {
        await d.prompter.message('info', 'No workspace is waiting for a host-access approval.')
        return
      }
      id = await d.prompter.pick(waiting, { placeHolder: 'Review a host-access request' })
      if (id === undefined) return
    }
    d.reviews.retake(id)
    await d.documents.show(uriOf(id))
  } finally {
    release()
  }
}

/**
 * drydock.approve. Only on the active review document's snapshot: with none
 * active it opens the pick and the document, never the input box. The
 * typed full name, compared exactly, sends that snapshot's hash.
 */
export async function approve(d: ReviewDeps): Promise<void> {
  const id = idOfURI(d.documents.activeUri())
  if (id === null) return reviewApproval(d)
  const release = await d.live.hold()
  try {
    const s = d.reviews.snapshot(id)
    if (s === null) {
      await d.prompter.message('info', 'Nothing is waiting for approval in this document.')
      return
    }
    if (!(d.live.meta?.credential.scopes ?? []).includes('approve')) {
      if ((await d.prompter.message('warning', NO_APPROVE_SCOPE, [REVIEW_IN_DRYDOCK])) === REVIEW_IN_DRYDOCK) {
        await openInDrydock(d, id)
      }
      return
    }
    if (s.fullName === null) {
      await d.prompter.message('warning', 'Drydock has not said which repository this is, so it cannot be approved here.')
      return
    }
    const roots = approvalRoots(s)
    const cancel = d.reviews.cancelOnChange(id)
    let typed: string | undefined
    try {
      typed = await d.prompter.input({
        prompt: `To approve host access for ${escapeFormat(s.fullName)}, type its full name.${roots}`,
        ignoreFocusOut: false,
        cancel,
      })
    } finally {
      cancel.dispose()
    }
    // Escape, focus loss, or the request changed under the box.
    if (typed === undefined) return
    if (typed !== s.fullName) {
      await d.prompter.message('warning', NOT_APPROVED_NAME)
      return
    }
    if (d.reviews.snapshot(id)?.hash !== s.hash) return
    const key = `workspace:${id}:approve`
    if (d.live.marks.has(key)) return
    d.live.marks.begin(key, settlesJobOr(id, 'approve', d.live.entities.lastEventId, settlesApprove(id)), overIn(id, OVER.build))
    try {
      await d.client.send('POST', pathFor('workspaces.approve', { id }), { hash: s.hash })
      d.live.marks.accepted(key)
    } catch (e) {
      d.live.marks.end(key)
      if (e instanceof ApiError && e.code === 'approval_stale') {
        await d.live.loadWorkspace(id).catch(() => {})
        d.reviews.retake(id)
        await d.documents.show(uriOf(id))
        await d.prompter.message('warning', STALE)
        return
      }
      if (e instanceof ApiError && e.code === 'forbidden_scope') {
        if ((await d.prompter.message('warning', NO_APPROVE_SCOPE, [REVIEW_IN_DRYDOCK])) === REVIEW_IN_DRYDOCK) await openInDrydock(d, id)
        return
      }
      throw e
    }
    d.log.info(`approved host access for workspace ${id} (${s.hash})`)
    // The approve job resumes the run: followed to running, then Connect.
    await follow(d, id, s.hash)
  } finally {
    release()
  }
}

/** drydock.decline: on the active review document, a modal, then the request is cancelled. */
export async function decline(d: ReviewDeps): Promise<void> {
  const id = idOfURI(d.documents.activeUri())
  if (id === null) return reviewApproval(d)
  const release = await d.live.hold()
  try {
    const s = d.reviews.snapshot(id)
    if (s === null) return
    const ok = await d.prompter.confirm(
      `Cancel the host-access request for ${escapeFormat(s.fullName ?? id)}? The workspace stays stopped, and Start asks again.`, CANCEL_REQUEST)
    if (!ok) return
    const key = `workspace:${id}:decline`
    if (d.live.marks.has(key)) return
    d.live.marks.begin(key, settlesDecline(id), overIn(id, OVER.decline))
    try {
      await d.client.send('DELETE', pathFor('workspaces.decline', { id }))
      d.live.marks.accepted(key)
    } catch (e) {
      d.live.marks.end(key)
      throw e
    }
  } finally {
    release()
  }
}

/** The root sentences that apply, for the confirm prompt to repeat. */
function approvalRoots(s: Snapshot): string {
  const r = approvalSections(s.view).roots
  return r.length === 0 ? '' : ` ${r.join(' ')}`
}
