// §9.2's update path: a release's .vsix, downloaded from GitHub and checked
// against that release's SHA256SUMS before anything installs it. The URLs
// are built only from a version that is exactly vX.Y.Z (handshake.ts
// vsixURL's rule): `dev`, or anything else a server says, downloads nothing.
//
// The sums come from the same place as the .vsix, so this guards against a
// corrupted or truncated download, not against a compromised release, and
// claims no more than that. No Drydock credential is involved: this is not
// the client, and sends nothing but two GETs.

import { createHash } from 'node:crypto'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

export const RELEASES = 'https://github.com/krelinga/drydock/releases/download'
export const ASSET = 'drydock-vscode.vsix'
const RELEASE = /^v\d+\.\d+\.\d+$/

/** Downloads and checks release `version`'s .vsix; resolves to the file's path. */
export async function downloadRelease(version: string, base = RELEASES): Promise<string> {
  if (!RELEASE.test(version)) throw new Error(`not a release version: ${JSON.stringify(version)}`)
  const get = async (name: string) => {
    const r = await fetch(`${base}/${version}/${name}`, { redirect: 'follow' })
    if (!r.ok) throw new Error(`downloading ${name} for ${version}: ${r.status}`)
    return Buffer.from(await r.arrayBuffer())
  }
  const sums = (await get('SHA256SUMS')).toString('utf8')
  const want = sums.split('\n').map((l) => /^([0-9a-f]{64}) [ *](.+)$/.exec(l.trim())).find((m) => m?.[2] === ASSET)?.[1]
  if (want === undefined) throw new Error(`${version}'s SHA256SUMS lists no ${ASSET}`)
  const vsix = await get(ASSET)
  const got = createHash('sha256').update(vsix).digest('hex')
  if (got !== want) throw new Error(`${version}'s ${ASSET} has SHA-256 ${got}, its SHA256SUMS says ${want}: not installed`)
  const file = path.join(mkdtempSync(path.join(tmpdir(), 'drydock-vsix-')), `drydock-vscode-${version}.vsix`)
  writeFileSync(file, vsix, { mode: 0o600 })
  return file
}
