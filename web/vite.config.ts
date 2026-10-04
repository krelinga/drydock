/// <reference types="vitest/config" />
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { msw } from 'msw/vite'

// The build writes straight into the Go package that embeds it, and that
// directory is committed, so `go build` never needs Node (frontend §3). Rebuild
// with `npm run build`; `npm run check` fails if the committed copy drifts.
const outDir = fileURLToPath(new URL('../internal/web/dist', import.meta.url))

export default defineConfig(({ command, mode }) => ({
  plugins: [
    vue(),
    // The MSW harness (frontend §10) exists only under `vite --mode mock`. It
    // is not merely tree-shaken out of a production build — the plugin that
    // serves the service worker is never loaded, so the worker script cannot
    // be emitted into dist/ (§7: no service worker, deliberately).
    command === 'serve' && mode === 'mock' ? msw({ mode: 'worker-only' }) : null,
  ],
  build: {
    outDir,
    emptyOutDir: true,
    assetsDir: 'assets',
    // Every target browser (§1) supports modulepreload natively.
    modulePreload: { polyfill: false },
    // No inline anything: CSP is script-src 'self' (frontend §8), and an
    // asset inlined as a data: URL is a second way for content to arrive.
    assetsInlineLimit: 0,
    sourcemap: false,
    reportCompressedSize: false,
  },
  server: {
    // `npm run dev` against a real `drydock serve` would proxy here; the
    // usual development loop is `npm run dev:mock`, which needs no backend.
    strictPort: true,
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.spec.ts'],
    environmentOptions: { jsdom: { url: 'https://drydock.test/' } },
    restoreMocks: true,
  },
}))
