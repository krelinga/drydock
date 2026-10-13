// The pinned VS Code (vscode.json), downloaded, checked and unpacked once per
// version into a cache: the archive's SHA-256 must match before tar sees it,
// as CI checks certutil's .deb. @vscode/test-electron's own download is
// never used, since it checks nothing.

import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { createReadStream, existsSync, mkdirSync, renameSync, rmSync } from 'node:fs'
import { homedir } from 'node:os'
import path from 'node:path'
import pin from './vscode.json'

export const PIN = pin as { version: string; platform: string; url: string; sha256: string }

function sha256(file: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const h = createHash('sha256')
    createReadStream(file).on('data', (c) => h.update(c)).on('end', () => resolve(h.digest('hex'))).on('error', reject)
  })
}

/** The `code` executable of the pinned VS Code, downloading and checking it first if it is not cached. */
export async function vscodeExecutable(cache = process.env.DRYDOCK_VSCODE_CACHE ?? path.join(homedir(), '.cache', 'drydock-vscode')): Promise<string> {
  const dir = path.join(cache, `${PIN.version}-${PIN.platform}`)
  const exe = path.join(dir, 'VSCode-linux-x64', 'code')
  if (existsSync(exe)) return exe
  mkdirSync(cache, { recursive: true })
  const archive = path.join(cache, `.download-${process.pid}.tar.gz`)
  const unpack = path.join(cache, `.unpack-${process.pid}`)
  try {
    const resp = await fetch(PIN.url)
    if (!resp.ok || resp.body === null) throw new Error(`VS Code download: ${resp.status}`)
    const { writeFile } = await import('node:fs/promises')
    await writeFile(archive, Buffer.from(await resp.arrayBuffer()))
    const got = await sha256(archive)
    if (got !== PIN.sha256) throw new Error(`VS Code ${PIN.version}: SHA-256 ${got}, pinned ${PIN.sha256}; refusing to unpack it`)
    mkdirSync(unpack, { recursive: true })
    execFileSync('tar', ['-xzf', archive, '-C', unpack])
    rmSync(dir, { recursive: true, force: true })
    renameSync(unpack, dir)
  } finally {
    rmSync(archive, { force: true })
    rmSync(unpack, { recursive: true, force: true })
  }
  if (!existsSync(exe)) throw new Error(`VS Code ${PIN.version}: no ${exe} after unpacking`)
  return exe
}
