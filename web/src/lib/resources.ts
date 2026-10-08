// The card's resource line, "mem 1.2 GB · disk 3.4 GB" (frontend §6.1), as a
// pure function of a workspace's state and its measurements.
//
// Honesty rules, each a spec:
//   - Unknown is "—", never 0. A running workspace not yet measured says
//     "mem —"; a workspace whose disk was never measured says "disk —".
//   - A stopped (or failed, or building) workspace has no memory figure at
//     all — it is not using any — so the line leaves memory out rather than
//     showing a stale one.
//   - A stale figure (the last measurement failed) carries "(stale)", and a
//     partial disk walk is a lower bound, "≥ 3.4 GB".
// Sizes are decimal (GB = 10⁹ bytes), as the label says.

import type { WorkspaceState } from '../api/types'
import type { HostDisk, Resources } from '../stores/resources'

/** 1234567890 → "1.2 GB"; two significant figures under ten, whole numbers above. */
export function formatBytes(n: number): string {
  const units = ['B', 'kB', 'MB', 'GB', 'TB', 'PB']
  let v = n
  let i = 0
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  const s = i === 0 || v >= 10 ? String(Math.round(v)) : v.toFixed(1)
  // 999.6 MB rounds to "1000 MB": say "1.0 GB" instead.
  if (s === '1000' && i < units.length - 1) return `1.0 ${units[i + 1]}`
  return `${s} ${units[i]}`
}

export interface ResourceLine {
  /** What the card shows. */
  text: string
  /** The longer reading, for a title and for screen readers. */
  label: string
}

/** Null when there is nothing to say: a workspace with no directory yet and no reading. */
export function resourceLine(state: WorkspaceState | null, r: Resources | undefined): ResourceLine | null {
  const parts: string[] = []
  const labels: string[] = []
  if (state === 'running') {
    const m = r?.memory ?? null
    if (m === null) {
      parts.push('mem —')
      labels.push('memory not measured yet')
    } else {
      parts.push(`mem ${formatBytes(m.bytes)}${m.stale ? ' (stale)' : ''}`)
      labels.push(`memory ${formatBytes(m.bytes)}${m.stale ? ', last measured at ' + clock(m.at) + ' (the latest reading failed)' : ''}`)
    }
  }
  const d = r?.disk ?? null
  if (d === null) {
    if (state === 'pending' && parts.length === 0) return null
    parts.push('disk —')
    labels.push('disk not measured yet')
  } else {
    const size = `${d.partial ? '≥ ' : ''}${formatBytes(d.bytes)}`
    parts.push(`disk ${size}${d.stale ? ' (stale)' : ''}`)
    labels.push(`disk ${d.partial ? 'at least ' : ''}${formatBytes(d.bytes)}` +
      (d.partial ? ' (part of the directory could not be read)' : '') +
      (d.stale ? `, last measured at ${clock(d.at)} (the latest reading failed)` : ''))
  }
  return { text: parts.join(' · '), label: labels.join('; ') }
}

/**
 * The detail view's breakdown of the disk figure: what it counts, so the
 * operator knows what a delete frees. Null without a reading.
 */
export function diskBreakdown(r: Resources | undefined): string | null {
  const d = r?.disk ?? null
  if (d === null) return null
  const parts = [`the clone and Drydock's files ${formatBytes(d.directoryBytes)}`]
  if (d.containerBytes !== null) parts.push(`the container's own changes ${formatBytes(d.containerBytes)}`)
  return parts.join(', ')
}

function clock(at: string): string {
  const t = new Date(at)
  return Number.isNaN(t.getTime()) ? at : t.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

/** "93%": the host disk's fill, rounded down so a refusal at 90 never reads 89. */
export function hostPercent(h: HostDisk): number {
  return h.totalBytes > 0 ? Math.floor((h.usedBytes * 100) / h.totalBytes) : 0
}
