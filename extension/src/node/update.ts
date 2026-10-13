// §9.2's update path: a release's .vsix, downloaded from GitHub and checked
// against that release's SHA256SUMS before anything installs it. The URLs
// are built only from a version that is exactly vX.Y.Z (handshake.ts
// vsixURL's rule): `dev`, or anything else a server says, downloads nothing.
// Each download is bounded in time and in size, and the file lives in a
// directory of its own that `removeDownload` deletes once VS Code has it.
//
// The sums come from the same place as the .vsix, so this guards against a
// corrupted or truncated download, not against a compromised release, and
// claims no more than that. No Drydock credential is involved: this is not
// the client, and sends nothing but two GETs.

import { createHash } from 'node:crypto'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

export const RELEASES = 'https://github.com/krelinga/drydock/releases/download'
export const ASSET = 'drydock-vscode.vsix'
/** Each request's bound. */
export const DOWNLOAD_TIMEOUT_MS = 60_000
/** The largest SHA256SUMS and .vsix accepted: a few lines, and far more than the bundle's budget. */
export const SUMS_MAX_BYTES = 64 * 1024
export const VSIX_MAX_BYTES = 20 * 1024 * 1024
const RELEASE = /^v\d+\.\d+\.\d+$/
const PREFIX = 'drydock-vsix-'

/** `resp`'s body, refused past `max` bytes whether or not it says its length. */
async function capped(resp: Response, max: number, what: string): Promise<Buffer> {
  const said = Number(resp.headers.get('Content-Length') ?? NaN)
  if (Number.isFinite(said) && said > max) throw new Error(`${what} is ${said} bytes, more than the ${max} accepted`)
  if (resp.body === null) return Buffer.alloc(0)
  const parts: Buffer[] = []
  let n = 0
  for await (const chunk of resp.body as AsyncIterable<Uint8Array>) {
    n += chunk.byteLength
    if (n > max) throw new Error(`${what} is more than the ${max} bytes accepted`)
    parts.push(Buffer.from(chunk))
  }
  return Buffer.concat(parts)
}

/** Downloads and checks release `version`'s .vsix; resolves to the file's path (see removeDownload). */
export async function downloadRelease(version: string, base = RELEASES, timeoutMs = DOWNLOAD_TIMEOUT_MS): Promise<string> {
  if (!RELEASE.test(version)) throw new Error(`not a release version: ${JSON.stringify(version)}`)
  const get = async (name: string, max: number) => {
    const r = await fetch(`${base}/${version}/${name}`, { redirect: 'follow', signal: AbortSignal.timeout(timeoutMs) })
    if (!r.ok) throw new Error(`downloading ${name} for ${version}: ${r.status}`)
    return capped(r, max, `${version}'s ${name}`)
  }
  const sums = (await get('SHA256SUMS', SUMS_MAX_BYTES)).toString('utf8')
  const want = sums.split('\n').map((l) => /^([0-9a-f]{64}) [ *](.+)$/.exec(l.trim())).find((m) => m?.[2] === ASSET)?.[1]
  if (want === undefined) throw new Error(`${version}'s SHA256SUMS lists no ${ASSET}`)
  const vsix = await get(ASSET, VSIX_MAX_BYTES)
  const got = createHash('sha256').update(vsix).digest('hex')
  if (got !== want) throw new Error(`${version}'s ${ASSET} has SHA-256 ${got}, its SHA256SUMS says ${want}: not installed`)
  const file = path.join(mkdtempSync(path.join(tmpdir(), PREFIX)), `drydock-vscode-${version}.vsix`)
  writeFileSync(file, vsix, { mode: 0o600 })
  return file
}

/** Deletes a download and the directory downloadRelease made for it; anything else is left alone. */
export function removeDownload(file: string): void {
  const dir = path.dirname(file)
  if (path.dirname(dir) !== tmpdir() || !path.basename(dir).startsWith(PREFIX)) return
  rmSync(dir, { recursive: true, force: true })
}
