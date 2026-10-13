// The version handshake (vscode-extension §9.1): GET /api/meta, read.
//
// `api` is an integer the server bumps when something this extension uses
// changes incompatibly; the extension declares the range it speaks in its
// own package.json (`drydock.apiRange`). Within it, everything works. Outside
// it, reads still work (the lists, follow and connect use routes unchanged
// since Phase 2) and every mutation is replaced by the sentence below.
//
// `version` is shown as text, and becomes part of a download URL only when it
// is exactly `vX.Y.Z` (§9.2): `dev`, or anything else a server says, builds
// none.

import pkg from '../package.json'

export interface Meta {
  version: string
  api: number
  credential: { kind: string; label: string; scopes: string[]; expires_at: string }
}

export type Skew =
  | { kind: 'ok' }
  | { kind: 'server_older'; sentence: string }
  | { kind: 'server_newer'; sentence: string; action: string }

export const API_RANGE: readonly [number, number] = pkg.drydock.apiRange as [number, number]

export const SERVER_OLDER =
  'This Drydock is older than this extension supports. Upgrade Drydock, or install the extension from its release.'
export const SERVER_NEWER = 'This extension is older than the Drydock it is talking to.'
export const INSTALL_MATCHING = 'Install the matching extension'

/** What the extension may do with a server answering `api`, given the range it speaks. */
export function skew(api: number, range: readonly [number, number] = API_RANGE): Skew {
  const [min, max] = range
  if (!Number.isInteger(api) || api < min) return { kind: 'server_older', sentence: SERVER_OLDER }
  if (api > max) return { kind: 'server_newer', sentence: SERVER_NEWER, action: INSTALL_MATCHING }
  return { kind: 'ok' }
}

const RELEASE = /^v\d+\.\d+\.\d+$/

/** The release's `.vsix` for a server's `version`, or null when the version is not a release's. */
export function vsixURL(version: unknown): string | null {
  if (typeof version !== 'string' || !RELEASE.test(version)) return null
  return `https://github.com/krelinga/drydock/releases/download/${version}/drydock-vscode.vsix`
}

/** A meta answer as the server sends it, or null for a body that is not one. */
export function parseMeta(v: unknown): Meta | null {
  if (v === null || typeof v !== 'object') return null
  const o = v as Record<string, unknown>
  const c = o.credential as Record<string, unknown> | null | undefined
  if (typeof o.version !== 'string' || typeof o.api !== 'number' || c === null || typeof c !== 'object') return null
  if (typeof c.kind !== 'string' || typeof c.label !== 'string' || typeof c.expires_at !== 'string' ||
    !Array.isArray(c.scopes) || !c.scopes.every((s) => typeof s === 'string')) return null
  return {
    version: o.version, api: o.api,
    credential: { kind: c.kind, label: c.label, scopes: c.scopes as string[], expires_at: c.expires_at },
  }
}
