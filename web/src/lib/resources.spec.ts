// The card's resource line (frontend §6.1): unknown is "—", stale says so, a
// partial walk is a lower bound, and a stopped workspace has no memory figure.

import { describe, expect, it } from 'vitest'
import type { Resources } from '../stores/resources'
import { diskBreakdown, formatBytes, hostPercent, resourceLine } from './resources'

const at = '2026-10-08T12:00:00Z'
function r(mem: number | null, disk: number | null, o: { stale?: boolean; partial?: boolean; layer?: number | null; container?: string } = {}): Resources {
  return {
    boot: 'b', round: 1,
    memory: mem === null ? null : { bytes: mem, at, stale: o.stale === true, containerId: o.container ?? null },
    disk: disk === null ? null : {
      bytes: disk, directoryBytes: disk - (o.layer ?? 0), containerBytes: o.layer ?? null,
      partial: o.partial === true, at, stale: o.stale === true,
    },
  }
}

describe('formatBytes', () => {
  it.each([
    [0, '0 B'], [999, '999 B'], [1000, '1.0 kB'], [1_200_000_000, '1.2 GB'], [3_400_000_000, '3.4 GB'],
    [12_400_000_000, '12 GB'], [999_600_000, '1.0 GB'], [512_000_000, '512 MB'], [1.5e12, '1.5 TB'],
  ])('%d → %s', (n, want) => expect(formatBytes(n)).toBe(want))
})

describe('resourceLine', () => {
  it('a running workspace shows memory and disk', () => {
    expect(resourceLine('running', r(1_200_000_000, 3_400_000_000))?.text).toBe('mem 1.2 GB · disk 3.4 GB')
  })

  it('unknown is a dash, never zero', () => {
    const line = resourceLine('running', undefined)
    expect(line?.text).toBe('mem — · disk —')
    expect(line?.text).not.toMatch(/\b0\b/)
    expect(line?.label).toContain('not measured yet')
    expect(resourceLine('running', r(null, 5_000_000_000))?.text).toBe('mem — · disk 5.0 GB')
  })

  it('a stopped workspace has no memory figure, and keeps its disk', () => {
    expect(resourceLine('stopped', r(1_200_000_000, 3_400_000_000))?.text).toBe('disk 3.4 GB')
    expect(resourceLine('failed', undefined)?.text).toBe('disk —')
  })

  it('stale and partial are said, not hidden', () => {
    expect(resourceLine('running', r(1_200_000_000, 3_400_000_000, { stale: true }))?.text)
      .toBe('mem 1.2 GB (stale) · disk 3.4 GB (stale)')
    const p = resourceLine('stopped', r(null, 3_400_000_000, { partial: true }))
    expect(p?.text).toBe('disk ≥ 3.4 GB')
    expect(p?.label).toContain('at least')
  })

  it('a reading of another container is not this workspace\'s: a stop and a start inside one round', () => {
    expect(resourceLine('running', r(1_200_000_000, 1e9, { container: 'old' }), 'new')?.text).toBe('mem — · disk 1.0 GB')
    // Controls: its own container, or no container named on either side, is shown.
    expect(resourceLine('running', r(1_200_000_000, 1e9, { container: 'new' }), 'new')?.text).toBe('mem 1.2 GB · disk 1.0 GB')
    expect(resourceLine('running', r(1_200_000_000, 1e9), 'new')?.text).toBe('mem 1.2 GB · disk 1.0 GB')
  })

  it('a workspace before its directory exists says nothing', () => {
    expect(resourceLine('pending', undefined)).toBeNull()
    expect(resourceLine('cloning', undefined)?.text).toBe('disk —') // control: once cloning, the dash
  })
})

describe('diskBreakdown', () => {
  it('names the clone and the container layer, which is what a delete frees', () => {
    expect(diskBreakdown(r(null, 3_400_000_000, { layer: 300_000_000 })))
      .toBe("the clone and Drydock's files 3.1 GB, the container's own changes 300 MB")
    expect(diskBreakdown(r(null, 1_000_000_000))).toBe("the clone and Drydock's files 1.0 GB")
    expect(diskBreakdown(undefined)).toBeNull()
  })
})

describe('hostPercent', () => {
  it('rounds down, so a disk refused at 90 never reads 89', () => {
    expect(hostPercent({ usedBytes: 905, totalBytes: 1000, limitPercent: 90, over: true, boot: 'b', round: 1 })).toBe(90)
    expect(hostPercent({ usedBytes: 1, totalBytes: 0, limitPercent: 90, over: false, boot: 'b', round: 1 })).toBe(0)
  })
})
