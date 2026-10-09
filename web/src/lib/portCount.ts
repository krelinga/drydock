// The card's ambient port count, "3 listening · 1 previewed" (port forwarding
// §8.2, frontend §6.1), as a pure function of a workspace's port entities.
//
// Discovery is ambient, not interruptive: this is a count, in the card's
// quiet type, and never a call to action — no button, no badge, no colour
// that asks to be looked at, and nothing that moves when a port appears. The
// ports panel is where decisions are made, at a moment the operator chose.
//
// Counted, not stored: the rows are the reducer's, so another device's
// enable or a port the scan found changes the count by the same event that
// changes the panel.
//   - listening: rows discovery says are listening now, hidden ones left out
//     (a hidden row is one the operator asked not to see);
//   - previewed: rows switched on, whatever is listening.

import type { Port } from '../stores/reducer'

export interface PortCount {
  listening: number
  previewed: number
}

export function portCount(ports: readonly Port[]): PortCount {
  let listening = 0
  let previewed = 0
  for (const p of ports) {
    if (p.observedState === 'listening' && !p.hidden) listening++
    if (p.enabled) previewed++
  }
  return { listening, previewed }
}

/** The card's text; null when there is nothing to say. */
export function portCountText(c: PortCount): string | null {
  const parts: string[] = []
  if (c.listening > 0) parts.push(`${c.listening} listening`)
  if (c.previewed > 0) parts.push(`${c.previewed} previewed`)
  return parts.length === 0 ? null : parts.join(' · ')
}

/** The longer reading, for a title and for screen readers. */
export function portCountLabel(c: PortCount): string {
  const n = (k: number, one: string, many: string) => `${k} ${k === 1 ? one : many}`
  return `${n(c.listening, 'port', 'ports')} listening in the container, ${n(c.previewed, 'port', 'ports')} previewed`
}
