// internal/web/dist is committed so `go build` never needs Node (frontend §3).
// The cost of committing build output is that it can drift from its source,
// silently: the binary would ship a UI nobody's sources describe. This closes
// that gap by rebuilding into a scratch directory and requiring the committed
// copy to match it byte for byte.
//
// Usage: node scripts/check-dist.mjs     (exit 1 on any difference)

import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const web = fileURLToPath(new URL('..', import.meta.url))
const committed = fileURLToPath(new URL('../../internal/web/dist', import.meta.url))
const scratch = mkdtempSync(join(tmpdir(), 'drydock-dist-'))

try {
  const vite = join(web, 'node_modules', '.bin', 'vite')
  const r = spawnSync(vite, ['build', '--outDir', scratch, '--emptyOutDir', '--logLevel', 'warn'], {
    cwd: web,
    stdio: 'inherit',
  })
  if (r.status !== 0) {
    console.error('FAIL: the scratch build itself failed')
    process.exit(1)
  }

  const list = (dir) =>
    readdirSync(dir, { recursive: true })
      .map(String)
      .filter((p) => statSync(join(dir, p)).isFile())
      .sort()
  const want = list(scratch)
  const have = list(committed)
  const problems = []
  for (const p of want) if (!have.includes(p)) problems.push(`missing from the committed dist: ${p}`)
  for (const p of have) if (!want.includes(p)) problems.push(`stale in the committed dist: ${p}`)
  for (const p of want) {
    if (have.includes(p) && !readFileSync(join(scratch, p)).equals(readFileSync(join(committed, p)))) {
      problems.push(`differs from a fresh build: ${p}`)
    }
  }
  if (problems.length > 0) {
    for (const p of problems) console.error(`FAIL: ${p}`)
    console.error(`internal/web/dist does not match the sources. Run \`npm run build\` in web/ and commit the result.`)
    process.exit(1)
  }
  console.log(`internal/web/dist matches a fresh build (${want.length} files, ${relative(web, committed)})`)
} finally {
  rmSync(scratch, { recursive: true, force: true })
}
