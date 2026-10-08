// Resources (design §6 *Resources*, frontend §6.1): each workspace's memory
// and disk, and the workspace filesystem against the disk limit. Pure helpers
// the reducer calls — it stays the one writer of these entities.
//
// They arrive three ways: the workspace list's and detail's `resources`
// (and the list's `disk`), and the stream's named `resources` frame, one per
// sampling round. A frame has no event id — a measurement is never
// persisted — so these are versioned by the server's own round counter and
// its boot id instead: a copy from the same boot replaces one only if its
// round is at least as new, and a copy from another boot (Drydock restarted)
// always does. Never by wall-clock time, which can step backwards and would
// then freeze every card. A measurement is never invented here: a missing or
// malformed field is "no reading", which the card shows as unknown, never as
// zero.

import type { DiskSampleView, HostDiskView, MemorySampleView, ResourcesFrame, ResourcesView } from '../api/types'

export interface MemorySample {
  bytes: number
  at: string
  /** The container measured, when one was: shown only while it is the workspace's. */
  containerId: string | null
  stale: boolean
}

export interface DiskSample {
  bytes: number
  directoryBytes: number
  containerBytes: number | null
  partial: boolean
  at: string
  stale: boolean
}

/** The server's version for a copy: its process, and its round counter there. */
export interface Version {
  boot: string
  round: number
}

export interface Resources extends Version {
  memory: MemorySample | null
  disk: DiskSample | null
}

export interface HostDisk extends Version {
  usedBytes: number
  totalBytes: number
  limitPercent: number
  over: boolean
}

function bytes(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null
}

function validTime(v: unknown): v is string {
  return typeof v === 'string' && Number.isFinite(Date.parse(v))
}

function version(v: { boot?: unknown; round?: unknown }): Version | null {
  const round = bytes(v.round)
  if (typeof v.boot !== 'string' || v.boot === '' || round === null) return null
  return { boot: v.boot, round }
}

/** Whether `next` may replace `held`: another boot always, the same boot only at the same round or later. */
export function newer(held: Version | null | undefined, next: Version): boolean {
  return held === null || held === undefined || held.boot !== next.boot || next.round >= held.round
}

function toMemory(v: MemorySampleView | null | undefined): MemorySample | null {
  if (v === null || typeof v !== 'object') return null
  const b = bytes(v.bytes)
  if (b === null || !validTime(v.at)) return null
  return {
    bytes: b, at: v.at, stale: v.stale === true,
    containerId: typeof v.container_id === 'string' && v.container_id !== '' ? v.container_id : null,
  }
}

function toDisk(v: DiskSampleView | null | undefined): DiskSample | null {
  if (v === null || typeof v !== 'object') return null
  const b = bytes(v.bytes)
  const dir = bytes(v.directory_bytes)
  if (b === null || dir === null || !validTime(v.at)) return null
  return {
    bytes: b, directoryBytes: dir, containerBytes: bytes(v.container_bytes),
    partial: v.partial === true, at: v.at, stale: v.stale === true,
  }
}

export function toResources(v: ResourcesView | null | undefined): Resources | null {
  if (v === null || v === undefined || typeof v !== 'object') return null
  const ver = version(v)
  if (ver === null) return null
  return { ...ver, memory: toMemory(v.memory), disk: toDisk(v.disk) }
}

export function toHostDisk(v: HostDiskView | null | undefined): HostDisk | null {
  if (v === null || v === undefined || typeof v !== 'object') return null
  const ver = version(v)
  const used = bytes(v.used_bytes)
  const total = bytes(v.total_bytes)
  const limit = bytes(v.limit_percent)
  if (ver === null || used === null || total === null || limit === null) return null
  return { ...ver, usedBytes: used, totalBytes: total, limitPercent: limit, over: v.over === true }
}

/**
 * Merges measurements into the map: each replaces the one held only if it is
 * newer by `newer`. `known` says which ids may be held — a workspace the
 * entities do not have, or one deleted, gets nothing.
 */
export function mergeResources(
  held: Record<string, Resources>,
  incoming: Record<string, ResourcesView | null | undefined>,
  known: (id: string) => boolean,
): Record<string, Resources> {
  let out = held
  for (const [id, view] of Object.entries(incoming)) {
    if (!known(id)) continue
    const r = toResources(view)
    if (r === null || !newer(held[id], r)) continue
    if (out === held) out = { ...held }
    out[id] = r
  }
  return out
}

/** The host disk, if `incoming` is newer by `newer`. */
export function mergeHostDisk(held: HostDisk | null, incoming: HostDiskView | null | undefined): HostDisk | null {
  const h = toHostDisk(incoming)
  if (h === null || !newer(held, h)) return held
  return h
}

/** A frame's parts, read defensively: a frame that is not one changes nothing. */
export function frameParts(f: unknown): { workspaces: Record<string, ResourcesView>; host: HostDiskView | null } | null {
  if (f === null || typeof f !== 'object') return null
  const x = f as Partial<ResourcesFrame>
  const workspaces = x.workspaces !== null && typeof x.workspaces === 'object' ? x.workspaces : {}
  return { workspaces, host: x.host ?? null }
}
