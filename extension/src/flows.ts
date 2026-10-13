// E1's flows (vscode-extension §5.4, §6, §7): the hub, clone and follow,
// connect, and the catalog refresh. Each mutation is frontend §4.2 to the
// letter — begin a mark, send, discard the 202, end the mark on its settling
// event or on a snapshot showing it over — and what the operator sees comes
// from the reducer's entities alone.
//
// Nothing shown unprompted takes focus (§6.2): a follow is a progress
// notification, and its end is a notification whose primary action is
// Connect (or Open in Drydock, for a failure or a host-access request).
// `drydock.connectWhenReady` connects without asking.

import { cloneOverIn, settlesClone, settlesRefresh } from '../../web/src/core/settle'
import { capacity } from '../../web/src/core/lib/capacity'
import { escapeFormat } from '../../web/src/core/lib/text'
import { cardStatus, stepTitle } from '../../web/src/core/lib/workspaceCard'
import { currentStep, vscodeConfigured, vscodeLink, workspaceForRepo, type Entities, type Workspace } from '../../web/src/core/reducer'
import { ApiError } from '../../web/src/core/errors'
import { atCap, cloneItems, connectItems, hubItems, type HubAction } from './hub'
import { vsixURL } from './handshake'
import { remoteURI } from './link'
import type { Live } from './live'
import type { Clock, Extensions, Log, Opener, Prompter, Settings, Window } from './ports'
import { pathFor } from './routes'
import { originOf } from './settings'
import { ELSEWHERE, mayMutate } from './windowRule'
import type { DrydockClient } from './client'

export const REMOTE_SSH = 'ms-vscode-remote.remote-ssh'
export const DEV_CONTAINERS = 'ms-vscode-remote.remote-containers'

export const CONNECT = 'Connect'
export const OPEN_IN_DRYDOCK = 'Open in Drydock'
export const TRY_AGAIN = 'Try again'
export const INSTALL_REMOTES = 'Install Remote - SSH and Dev Containers'
export const NO_SSH_HOST =
  'Drydock was started without --vscode-ssh-host, so it cannot say how VS Code reaches this server. Open in Drydock shows how to turn it on.'
export const NO_CONTAINER = 'Drydock could not find this workspace\'s container just now.'
export const NEEDS_REMOTES = 'Connecting needs the Remote - SSH and Dev Containers extensions.'
export const NOTHING_YET = 'Drydock has no workspaces and the GitHub App reaches no repositories yet.'

export interface FlowDeps {
  live: Live
  client: DrydockClient
  prompter: Prompter
  opener: Opener
  extensions: Extensions
  window: Window
  settings: Settings
  clock: Clock
  log: Log
  /** `drydock.connectWhenReady`. */
  connectWhenReady: () => boolean
  /** Workspaces this window followed to running: the hub's *ready* (§6.2). */
  ready: Set<string>
}

const nameOf = (e: Entities, w: Workspace | null | undefined, fallback = 'The workspace') =>
  w === null || w === undefined ? fallback
    : escapeFormat(w.fullName ?? (w.repositoryId !== null ? e.repos[w.repositoryId]?.fullName : undefined) ?? w.id)

/** Runs `fn` with the stream held (§5.5). */
async function held<T>(d: FlowDeps, fn: () => Promise<T>): Promise<T> {
  const release = await d.live.hold()
  try {
    return await fn()
  } finally {
    release()
  }
}

/** Drydock's page for a workspace, or its home. */
export async function openInDrydock(d: Pick<FlowDeps, 'settings' | 'opener' | 'prompter'>, id?: string): Promise<void> {
  const o = originOf(d.settings.url())
  if (!o.ok) {
    await d.prompter.message('warning', o.sentence)
    return
  }
  await d.opener.openExternal(id === undefined ? `${o.origin}/` : `${o.origin}/ws/${encodeURIComponent(id)}`)
}

/** `sentence`, with Open in Drydock; pressing it opens `id`'s page (or the home page). */
async function offerDrydock(d: FlowDeps, level: 'info' | 'warning' | 'error', sentence: string, id?: string): Promise<void> {
  if ((await d.prompter.message(level, sentence, [OPEN_IN_DRYDOCK])) === OPEN_IN_DRYDOCK) await openInDrydock(d, id)
}

/**
 * Whether a mutation may go to this server (§9.1): outside the extension's
 * api range every mutation is replaced by the skew's sentence, with the
 * matching release to install when the server is the newer one.
 */
async function skewAllows(d: FlowDeps): Promise<boolean> {
  const s = d.live.skew()
  if (s === null || s.kind === 'ok') return true
  const url = s.kind === 'server_newer' ? vsixURL(d.live.meta?.version) : null
  if (s.kind !== 'server_newer' || url === null) {
    await d.prompter.message('warning', s.sentence)
    return false
  }
  if ((await d.prompter.message('warning', `${s.sentence} Install the extension from Drydock ${d.live.meta!.version}'s release.`, [s.action])) === s.action) {
    await d.opener.openExternal(url)
  }
  return false
}

/** Resolves on the next change to the live state, or after `ms`. */
function nextChange(d: FlowDeps, ms: number): Promise<void> {
  return new Promise((resolve) => {
    let off = () => {}
    const cancel = d.clock.after(ms, () => {
      off()
      resolve()
    })
    off = d.live.onChange(() => {
      off()
      cancel()
      resolve()
    })
  })
}

// ---- the hub ---------------------------------------------------------------

/** drydock.go: the hub. */
export async function go(d: FlowDeps): Promise<void> {
  const choice = await held(d, async () => {
    const items = hubItems(d.live.entities, d.ready)
    if (items.length === 0) {
      await offerDrydock(d, 'info', NOTHING_YET)
      return undefined
    }
    return d.prompter.pick(items, { placeHolder: 'Go to a workspace, or clone a repository' })
  })
  if (choice !== undefined) await act(d, choice)
}

async function act(d: FlowDeps, a: HubAction): Promise<void> {
  switch (a.kind) {
    case 'connect':
      return connect(d, a.id)
    case 'open':
      return openInDrydock(d, a.id)
    case 'clone':
      return clone(d, a.repoId)
    case 'make_room':
      return offerDrydock(d, 'warning', 'Drydock is at its cap. Stop a workspace in Drydock to make room: its clone survives.')
  }
}

// ---- clone and follow --------------------------------------------------------

/** drydock.clone: pick a repository, clone it, follow it. */
export async function cloneCommand(d: FlowDeps): Promise<void> {
  const repoId = await held(d, async () => {
    const e = d.live.entities
    const cap = capacity(e)
    if (cap.full && cap.cap !== null) {
      await offerDrydock(d, 'warning', `${atCap(cap.cap)}. Drydock shows which ones can be stopped.`)
      return undefined
    }
    const items = cloneItems(e)
    if (items.length === 0) {
      await offerDrydock(d, 'info', 'Every repository the GitHub App reaches already has a workspace.')
      return undefined
    }
    return d.prompter.pick(items, { placeHolder: 'Clone a repository into a new workspace' })
  })
  if (repoId !== undefined) await clone(d, repoId)
}

/**
 * Clones repository `repoId` and follows the new workspace. Rule 1 holds
 * here as in the dispatcher, since the hub, which reads, can reach it.
 */
export async function clone(d: FlowDeps, repoId: number): Promise<void> {
  if (!mayMutate(d.window.remoteName())) {
    await offerDrydock(d, 'warning', ELSEWHERE)
    return
  }
  const release = await d.live.hold()
  try {
    if (!(await skewAllows(d))) return
    const key = `repo:${repoId}:clone`
    const name = escapeFormat(d.live.entities.repos[repoId]?.fullName ?? `repository ${repoId}`)
    if (d.live.marks.has(key)) {
      await d.prompter.message('info', `${name} is already being cloned.`)
      return
    }
    d.live.marks.begin(key, settlesClone(repoId), cloneOverIn(repoId))
    try {
      await d.client.send('POST', pathFor('workspaces.create'), { repository_id: repoId })
      d.live.marks.accepted(key)
    } catch (e) {
      d.live.marks.end(key)
      throw e
    }
    // The mark ends when the row exists (settlesClone): the workspace id
    // comes from that event, never from the 202's body (§5.4).
    while (d.live.marks.has(key)) await nextChange(d, 1_000)
    const w = workspaceForRepo(d.live.entities, repoId)
    if (w === null) return
    d.log.info(`cloning ${name}: workspace ${w.id}`)
    await follow(d, w.id)
  } finally {
    release()
  }
}

export type FollowEnd = 'running' | 'failed' | 'approval' | 'stopped' | 'gone' | 'dismissed'

/** Where a followed workspace is: an end, or the line to show while it moves. */
export function followState(e: Entities, id: string): { end: Exclude<FollowEnd, 'dismissed'> | null; line: string } {
  const w = e.workspaces[id]
  if (w === undefined || e.gone[id] !== undefined) return { end: 'gone', line: 'Deleted.' }
  switch (w.state) {
    case 'running':
      return { end: 'running', line: 'Running.' }
    case 'failed':
      return { end: 'failed', line: cardStatus(w).line }
    case 'stopped':
      return w.approval !== null ? { end: 'approval', line: 'Needs host-access approval.' } : { end: 'stopped', line: 'Stopped.' }
  }
  const step = currentStep(w)
  return { end: null, line: step !== null ? `${stepTitle(step.name)}…` : `${cardStatus(w).line}…` }
}

/**
 * Follows workspace `id` to running, failed, or stopped for an approval, in
 * a progress notification, holding the stream throughout (§5.5); the
 * operator dismissing it ends the follow. Then says how it ended, with the
 * action that fits.
 */
export async function follow(d: FlowDeps, id: string): Promise<FollowEnd> {
  const release = await d.live.hold()
  let end: FollowEnd = 'dismissed'
  try {
    const name = nameOf(d.live.entities, d.live.entities.workspaces[id])
    await d.prompter.progress(`Drydock: ${name}`, async (report, dismissed) => {
      let last = ''
      for (;;) {
        const s = followState(d.live.entities, id)
        if (s.end !== null) {
          end = s.end
          return
        }
        if (s.line !== last) report((last = s.line))
        if (dismissed()) return
        await nextChange(d, 1_000)
      }
    })
    await ending(d, id, name, end)
    return end
  } finally {
    release()
  }
}

async function ending(d: FlowDeps, id: string, name: string, end: FollowEnd): Promise<void> {
  switch (end) {
    case 'running': {
      d.ready.add(id)
      if (d.connectWhenReady()) return connect(d, id)
      if ((await d.prompter.message('info', `${name} is running.`, [CONNECT])) === CONNECT) await connect(d, id)
      return
    }
    case 'failed': {
      const w = d.live.entities.workspaces[id]
      const s = w !== undefined ? cardStatus(w) : null
      return offerDrydock(d, 'error', `${name}: ${s?.line ?? 'Failed'}.${s?.note ? ` ${s.note}` : ''}`, id)
    }
    case 'approval':
      return offerDrydock(d, 'warning',
        `${name} needs host-access approval: its configuration asks for access to this server. Review it in Drydock.`, id)
    case 'stopped':
      return offerDrydock(d, 'info', `${name} stopped.`, id)
    case 'gone':
      return void (await d.prompter.message('info', `${name} was deleted.`))
    case 'dismissed':
      return
  }
}

// ---- connect --------------------------------------------------------------

/** drydock.connect: pick a running workspace and connect to it. */
export async function connectCommand(d: FlowDeps): Promise<void> {
  const id = await held(d, async () => {
    const items = connectItems(d.live.entities, d.ready)
    if (items.length === 0) {
      await offerDrydock(d, 'info', 'No workspace is running.')
      return undefined
    }
    return d.prompter.pick(items, { placeHolder: 'Connect to a running workspace' })
  })
  if (id !== undefined) await connect(d, id)
}

/**
 * Opens workspace `id`'s container in a new window (§7): the server's own
 * link, read from a view fetched now (a rebuild renames the container, and
 * no event carries the link), checked by core's vscodeURL and rewritten only
 * in its scheme. Remote - SSH and Dev Containers are checked here, never
 * declared as dependencies.
 */
export async function connect(d: FlowDeps, id: string): Promise<void> {
  await held(d, async () => {
    try {
      await d.live.loadWorkspace(id)
    } catch (e) {
      if (e instanceof ApiError && e.code === 'not_found') {
        await d.prompter.message('info', 'That workspace no longer exists.')
        return
      }
      throw e
    }
    const w = d.live.entities.workspaces[id]
    const name = nameOf(d.live.entities, w)
    if (w === undefined || w.state !== 'running') {
      await offerDrydock(d, 'info', `${name} is not running. Start it in Drydock.`, id)
      return
    }
    if (vscodeConfigured(w) === false) {
      await offerDrydock(d, 'warning', NO_SSH_HOST, id)
      return
    }
    const uri = remoteURI(vscodeLink(w))
    if (uri === null) {
      if ((await d.prompter.message('warning', NO_CONTAINER, [TRY_AGAIN])) === TRY_AGAIN) await connect(d, id)
      return
    }
    const missing = [REMOTE_SSH, DEV_CONTAINERS].filter((x) => !d.extensions.installed(x))
    if (missing.length > 0) {
      // Installing an extension is a mutation of the laptop (§8 rule 1).
      if (!mayMutate(d.window.remoteName())) {
        await d.prompter.message('warning', `${NEEDS_REMOTES} ${ELSEWHERE}`)
        return
      }
      if ((await d.prompter.message('warning', NEEDS_REMOTES, [INSTALL_REMOTES])) !== INSTALL_REMOTES) return
      for (const x of missing) await d.extensions.install(x)
    }
    d.log.info(`connecting to workspace ${id}`)
    await d.opener.openFolder(uri)
  })
}

// ---- the catalog refresh ------------------------------------------------------

export const REFRESH_KEY = 'catalog:refresh'

/** drydock.refreshRepositories: POST /api/repos/refresh, settled by its outcome event. */
export async function refreshRepositories(d: FlowDeps): Promise<void> {
  await held(d, async () => {
    if (!(await skewAllows(d)) || d.live.marks.has(REFRESH_KEY)) return
    const since = d.live.entities.lastRefresh?.eventId ?? 0
    d.live.marks.begin(REFRESH_KEY, settlesRefresh)
    try {
      await d.client.send('POST', pathFor('repos.refresh'))
      d.live.marks.accepted(REFRESH_KEY)
    } catch (e) {
      d.live.marks.end(REFRESH_KEY)
      throw e
    }
    await d.prompter.progress('Drydock: refreshing repositories', async (_report, dismissed) => {
      while (d.live.marks.has(REFRESH_KEY) && !dismissed()) await nextChange(d, 1_000)
    })
    const r = d.live.entities.lastRefresh
    if (r === null || r.eventId <= since) return
    await d.prompter.message(r.ok ? 'info' : 'error', r.message)
  })
}
