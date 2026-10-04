// The bundle budget (frontend §1): initial JS under 100 KB gzipped. "A budget
// without a gate is a wish," so this exits non-zero when it is exceeded.
//
// "Initial" is exactly what index.html makes the browser fetch before the app
// can run: its module scripts and their modulepreloads. Lazy chunks (secrets,
// later logs) are not counted, which is the point of making them lazy.
//
// It also checks two things about the shipped bundle that the CSP and §7 rely
// on and nothing else would notice: index.html carries no inline script or
// style, and no part of the mock harness (an MSW service worker) was emitted.
//
// Usage: node scripts/budget.mjs [distDir]

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { gzipSync } from 'node:zlib'

const BUDGET = 100 * 1024
const dist = process.argv[2] ?? fileURLToPath(new URL('../../internal/web/dist', import.meta.url))
const failures = []

const html = readFileSync(join(dist, 'index.html'), 'utf8')

// Every <script> must have a src, and there may be no <style> element or
// style attribute: the CSP is script-src 'self'; style-src 'self'.
for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
  if (!/\bsrc=/.test(m[1]) || m[2].trim() !== '') failures.push(`inline <script> in index.html: ${m[0].slice(0, 80)}`)
}
if (/<style\b/i.test(html)) failures.push('inline <style> in index.html')
if (/\sstyle=/i.test(html)) failures.push('style attribute in index.html')

const initial = new Set()
for (const m of html.matchAll(/<script\b[^>]*\bsrc="([^"]+)"/gi)) initial.add(m[1])
for (const m of html.matchAll(/<link\b[^>]*\brel="modulepreload"[^>]*\bhref="([^"]+)"/gi)) initial.add(m[1])
for (const m of html.matchAll(/<link\b[^>]*\bhref="([^"]+)"[^>]*\brel="modulepreload"/gi)) initial.add(m[1])

// A budget that found nothing to measure would pass forever.
if (initial.size === 0) failures.push('found no initial JS in index.html: the budget would be vacuous')

let total = 0
for (const src of initial) {
  if (!src.startsWith('/')) {
    failures.push(`initial script is not same-origin absolute: ${src}`)
    continue
  }
  const bytes = readFileSync(join(dist, src))
  const gz = gzipSync(bytes, { level: 9 }).length
  total += gz
  console.log(`  ${src}  ${(bytes.length / 1024).toFixed(1)} KB raw, ${(gz / 1024).toFixed(1)} KB gzip`)
}
console.log(`initial JS: ${(total / 1024).toFixed(1)} KB gzipped of a ${BUDGET / 1024} KB budget`)
if (total >= BUDGET) failures.push(`initial JS is ${total} bytes gzipped; the budget is ${BUDGET}`)

// The mock harness never ships (§7: no service worker; §10: dev only).
function walk(dir) {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n)
    return statSync(p).isDirectory() ? walk(p) : [p]
  })
}
for (const f of walk(dist)) {
  const rel = relative(dist, f)
  if (/mockServiceWorker/i.test(rel)) failures.push(`mock service worker shipped: ${rel}`)
  if (/\.(js|html)$/.test(rel)) {
    const text = readFileSync(f, 'utf8')
    if (text.includes('drydockMock') || text.includes('serviceWorker.register')) {
      failures.push(`mock harness code shipped in ${rel}`)
    }
  }
}

if (failures.length > 0) {
  for (const f of failures) console.error(`FAIL: ${f}`)
  process.exit(1)
}
