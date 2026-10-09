// Why a port's preview cannot answer, from what the entities already hold
// (port forwarding §11, §13 step 6): the row's bind address and observed
// state from discovery, the workspace's state, and its discovery's state. A
// pure function of structured fields — never of a message — so the panel says
// the same thing whichever input wrote them.
//
// The proxy says the same on the preview's own page (internal/preview
// LoopbackSentence and OutcomeSentence); portDiagnosis.spec.ts reads the Go
// so the loopback sentence cannot drift.

import type { DiscoveryState } from '../api/types'
import type { Port } from '../stores/reducer'

export type PortDiagnosis =
  /** Listening on loopback only: never previewable, never dialled (§11's first row). */
  | { kind: 'loopback'; where: string }
  /** Enabled, and discovery saw it stop listening (§11's second row). */
  | { kind: 'not_listening'; lastSeenAt: string | null }
  /** Enabled, and discovery, working, has never seen it listen. */
  | { kind: 'never_listened' }

/**
 * `127.0.0.1:5173`, `[::1]:5173` — the bind address and port as the dev
 * server's own banner prints them, an IPv4-mapped address unmapped (Go's
 * netip.AddrPort, which LoopbackSentence formats with).
 */
export function bindWhere(bind: string | null, port: number): string {
  if (bind === null || bind === '') return String(port)
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(bind)
  if (mapped) return `${mapped[1]}:${port}`
  return bind.includes(':') ? `[${bind}]:${port}` : `${bind}:${port}`
}

/** The loopback sentence's words before the flag; the flag is set as code beside it. */
export function loopbackLead(where: string): string {
  return `Listening on ${where}, which is only reachable from inside the container. Start it with`
}

/** internal/preview LoopbackSentence, whole. */
export function loopbackSentence(where: string): string {
  return `${loopbackLead(where)} --host 0.0.0.0.`
}

/**
 * The row's diagnosis, or null when there is nothing to say beyond what the
 * row already shows. `running` is the workspace's state (a stopped
 * workspace's rows are the panel's one sentence, not each row's); `discovery`
 * its discovery's, so a row is never said to be silent by a scanner that
 * cannot read it.
 */
export function diagnose(p: Port, running: boolean, discovery: DiscoveryState | null): PortDiagnosis | null {
  if (p.observedState === 'listening' && p.loopback) return { kind: 'loopback', where: bindWhere(p.bindAddr, p.containerPort) }
  if (!p.enabled || !running) return null
  if (p.observedState === 'gone') return { kind: 'not_listening', lastSeenAt: p.lastSeenAt }
  if (p.observedState === null && discovery === 'ok') return { kind: 'never_listened' }
  return null
}
