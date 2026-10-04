// Shared spec plumbing: one MSW server over one pretend backend, reset between
// tests, failing loudly on any request no handler claims.

import { afterAll, afterEach, beforeAll } from 'vitest'
import { setupServer } from 'msw/node'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory } from 'vue-router'
import App from '../App.vue'
import { createDrydock } from '../app'
import { onUnauthorized } from '../api/client'
import { handlersFor, newBackend, type MockBackend } from '../mocks/backend'
import { FakeEventSource, installFakeEventSource } from './fakeEventSource'

export let backend: MockBackend = newBackend()
export const server = setupServer()

installFakeEventSource()

export function useMockApi(): void {
  beforeAll(() => server.listen({ onUnhandledFrame: 'error' }))
  afterEach(() => {
    server.resetHandlers()
    onUnauthorized(null)
    for (const es of FakeEventSource.instances) es.close()
    FakeEventSource.instances = []
    document.body.innerHTML = ''
  })
  afterAll(() => server.close())
}

/** A fresh backend for one test, installed as the server's handlers. */
export function freshBackend(overrides: Partial<MockBackend> = {}): MockBackend {
  backend = newBackend(overrides)
  server.use(...handlersFor(backend))
  return backend
}

/** Mounts the whole app — the shipping router, stores and 401 path — at `path`. */
export async function mountApp(path: string) {
  const { pinia, router } = createDrydock(createMemoryHistory())
  await router.push(path)
  await router.isReady()
  const host = document.createElement('div')
  document.body.appendChild(host)
  const wrapper = mount(App, { global: { plugins: [pinia, router] }, attachTo: host })
  await settle()
  return { wrapper, router, pinia }
}

/**
 * Lets every pending request, store update and navigation finish. MSW answers
 * on a macrotask, so a microtask flush alone can observe a sign-in halfway.
 */
export async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) {
    await new Promise((r) => setTimeout(r, 0))
    await flushPromises()
  }
}
