// The status bar item (vscode-extension §6.3): `Drydock: 2 running`, with
// the stream's phase — the web UI's quiet marker after five seconds without
// a live stream, and *idle* once the stream has closed (§5.5), so a stale
// count never looks live — and, ahead of the count, the one thing the
// operator must act on: a server outside this extension's api range, or a
// token that expires within seven days. One item, one command.
//
// Pure: the adapter renders it on every change and every few seconds.

import type { Live } from './live'

/** web/src/stores/stream.ts's: nothing shown for this long after a drop (frontend §4.3); status.spec.ts reads it from there. */
export const MARKER_AFTER_MS = 5_000

export interface StatusView {
  text: string
  tooltip: string
  /** The command a click runs. */
  command: 'drydock.go' | 'drydock.signIn' | 'drydock.openInBrowser'
  warning: boolean
}

/** A token this close to its expiry says so (§4.2). */
export const EXPIRY_WARN_MS = 7 * 24 * 60 * 60_000

export function statusView(live: Pick<Live, 'entities' | 'meta' | 'phase' | 'phaseSince' | 'streaming' | 'skew'>, now: number, signedIn: boolean): StatusView {
  if (!signedIn) {
    return { text: '$(server) Drydock: sign in', tooltip: 'Sign in to Drydock with an API token from its Settings.', command: 'drydock.signIn', warning: false }
  }
  const skew = live.skew()
  if (skew !== null && skew.kind !== 'ok') {
    return { text: '$(warning) Drydock: version mismatch', tooltip: skew.sentence, command: 'drydock.openInBrowser', warning: true }
  }
  const expires = live.meta === null ? NaN : Date.parse(live.meta.credential.expires_at)
  if (Number.isFinite(expires) && expires - now <= EXPIRY_WARN_MS) {
    const days = Math.max(0, Math.ceil((expires - now) / 86_400_000))
    const when = expires <= now ? 'has expired' : days <= 1 ? 'expires within a day' : `expires in ${days} days`
    return {
      text: `$(warning) Drydock token ${when}`,
      tooltip: 'Create a new token in Drydock\'s Settings and sign in with it.',
      command: 'drydock.signIn', warning: true,
    }
  }
  const running = Object.values(live.entities.workspaces).filter((w) => w.state === 'running' && live.entities.gone[w.id] === undefined).length
  let text = `$(server) Drydock: ${running} running`
  let tooltip = 'Go to a workspace'
  if (!live.streaming) {
    text += ' · idle'
    tooltip = 'Not following Drydock right now: the count is as it was. Go to a workspace to look again.'
  } else if (live.phase !== 'live' && now - live.phaseSince >= MARKER_AFTER_MS) {
    text += ' · reconnecting'
    tooltip = 'The connection to Drydock dropped; reconnecting.'
  }
  return { text, tooltip, command: 'drydock.go', warning: false }
}
