import { createRouter, type RouteLocationRaw, type Router, type RouterHistory } from 'vue-router'
import WorkspacesView from './views/WorkspacesView.vue'
import WorkspaceView from './views/WorkspaceView.vue'
import SettingsView from './views/SettingsView.vue'
import SignInView from './views/SignInView.vue'
import NotFoundView from './views/NotFoundView.vue'

declare module 'vue-router' {
  interface RouteMeta {
    /** Reachable without a session. Exactly one route sets it (frontend §5). */
    public?: boolean
    /** The document title, and what the focus announcement reads. */
    title: string
  }
}

export function createAppRouter(history: RouterHistory): Router {
  return createRouter({
    history,
    // A link to a section — MakeRoom's "Stop one under Running" — scrolls to
    // it. Nothing else scrolls on navigation, as before.
    scrollBehavior: (to) => (to.hash ? { el: to.hash } : false),
    routes: [
      { path: '/signin', name: 'signin', component: SignInView, meta: { public: true, title: 'Sign in' } },
      { path: '/', name: 'workspaces', component: WorkspacesView, meta: { title: 'Workspaces' } },
      // §5: a route on a phone. (The ≥ 900 px column beside the list is a
      // layout still to come; the route is what both would render.)
      { path: '/ws/:id', name: 'workspace', component: WorkspaceView, meta: { title: 'Workspace' } },
      // A lazy chunk, per frontend §3.1: secrets and logs are the two. All
      // three secret routes import one module, so they are one chunk.
      { path: '/secrets', name: 'secrets', component: () => import('./views/secrets').then((m) => m.SecretsListView), meta: { title: 'Secrets' } },
      { path: '/secrets/new', name: 'secret-new', component: () => import('./views/secrets').then((m) => m.SecretNewView), meta: { title: 'New secret' } },
      { path: '/secrets/:name', name: 'secret', component: () => import('./views/secrets').then((m) => m.SecretView), meta: { title: 'Secret' } },
      { path: '/settings', name: 'settings', component: SettingsView, meta: { title: 'Settings' } },
      // Not public: an unknown path tells a signed-out caller nothing, and the
      // server's SPA fallback already answered it with index.html.
      { path: '/:rest(.*)*', name: 'not-found', component: NotFoundView, meta: { title: 'Not found' } },
    ],
  })
}

/** Where a signed-out navigation goes, carrying where it was headed. */
export function signInLocation(returnTo: string): RouteLocationRaw {
  return returnTo === '/' || returnTo === ''
    ? { name: 'signin' }
    : { name: 'signin', query: { return: returnTo } }
}
