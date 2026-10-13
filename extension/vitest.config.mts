import { defineConfig } from 'vitest/config'

// The unit and manifest tiers (vscode-extension §12.2, §12.3): plain Node, no
// `vscode` (which does not exist outside the editor, so a pure module that
// reached for it would fail its own spec at import).
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.spec.ts', 'test/**/*.spec.ts'],
    // The client tier needs the stack (vitest.client.config.mts, run.sh).
    exclude: ['test/client/**', 'node_modules/**'],
    restoreMocks: true,
  },
})
