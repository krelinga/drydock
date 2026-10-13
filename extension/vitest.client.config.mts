import { defineConfig } from 'vitest/config'

// The client tier (vscode-extension §12.4): the extension's own client, SSE
// stream and session logic, with its production Node transport, against the
// real Drydock behind real Caddy (test/extension/stack.ts). No VS Code. Run by
// test/extension/run.sh, which checks the prerequisites (go, caddy, openssl,
// drydock.test resolving to 127.0.0.1).
//
// The global setup makes the tier's CA before any test process starts and
// points NODE_EXTRA_CA_CERTS at it, which Node reads once, at a process's
// start: the workers are forked after it, so TLS validation stays fully on
// and trusts this CA too. Files run one at a time, each with its own stack.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/client/**/*.spec.ts'],
    globalSetup: ['test/client/global.ts'],
    pool: 'forks',
    fileParallelism: false,
    testTimeout: 90_000,
    hookTimeout: 300_000,
    restoreMocks: true,
  },
})
