import { createApp } from 'vue'
import { createWebHistory } from 'vue-router'
import App from './App.vue'
import { createDrydock } from './app'
import './styles/tokens.css'
import './styles/base.css'

async function boot(): Promise<void> {
  // `vite --mode mock` only. MODE is a build-time constant, so in a production
  // build this branch and the dynamic import behind it are removed entirely.
  if (import.meta.env.MODE === 'mock') {
    const { startMockWorker } = await import('./mocks/browser')
    await startMockWorker()
  }

  const { pinia, router } = createDrydock(createWebHistory())
  const app = createApp(App).use(pinia).use(router)
  // Mount after the first navigation settles — which includes the boot probe
  // (GET /api/auth/session) — so a signed-out visitor never sees the shell
  // flash before /signin.
  await router.isReady()
  app.mount('#app')
}

void boot()
