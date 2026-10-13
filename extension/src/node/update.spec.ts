// The release download (node/update.ts) against a real HTTP server standing in
// for GitHub's release downloads: a .vsix whose SHA-256 matches its release's
// SHA256SUMS is written out; anything else is refused, and a version that is
// not a release asks for nothing.

import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { afterEach, describe, expect, it } from 'vitest'
import { ASSET, downloadRelease } from './update'

const servers: Server[] = []
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => new Promise((r) => s.close(r))))
})

async function release(files: Record<string, Buffer | string>) {
  const seen: string[] = []
  const s = createServer((req, res) => {
    seen.push(req.url ?? '')
    const body = files[req.url ?? '']
    if (body === undefined) {
      res.writeHead(404)
      res.end()
      return
    }
    res.writeHead(200)
    res.end(body)
  })
  servers.push(s)
  await new Promise<void>((r) => s.listen(0, '127.0.0.1', r))
  return { base: `http://127.0.0.1:${(s.address() as AddressInfo).port}`, seen }
}

const vsix = Buffer.from('PK\u0003\u0004 a vsix')
const sha = createHash('sha256').update(vsix).digest('hex')

describe('downloadRelease', () => {
  it('writes the .vsix when its SHA-256 is the one SHA256SUMS lists', async () => {
    const r = await release({ '/v1.2.3/SHA256SUMS': `${'0'.repeat(64)}  drydock_linux_amd64.tar.gz\n${sha}  ${ASSET}\n`, [`/v1.2.3/${ASSET}`]: vsix })
    const file = await downloadRelease('v1.2.3', r.base)
    expect(readFileSync(file)).toEqual(vsix)
  })

  it('refuses a .vsix whose SHA-256 differs, and a SHA256SUMS without it', async () => {
    const bad = await release({ '/v1.2.3/SHA256SUMS': `${'f'.repeat(64)}  ${ASSET}\n`, [`/v1.2.3/${ASSET}`]: vsix })
    await expect(downloadRelease('v1.2.3', bad.base)).rejects.toThrow(/SHA-256/)
    const none = await release({ '/v1.2.3/SHA256SUMS': `${sha}  something-else.vsix\n`, [`/v1.2.3/${ASSET}`]: vsix })
    await expect(downloadRelease('v1.2.3', none.base)).rejects.toThrow(/lists no/)
    expect(none.seen).toEqual(['/v1.2.3/SHA256SUMS'])
  })

  it('a version that is not a release asks for nothing', async () => {
    const r = await release({})
    for (const v of ['dev', 'v1.2', '../v1.2.3', 'v1.2.3/../x']) {
      await expect(downloadRelease(v, r.base)).rejects.toThrow(/not a release version/)
    }
    expect(r.seen).toEqual([])
  })
})
