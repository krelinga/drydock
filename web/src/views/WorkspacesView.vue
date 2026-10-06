<script setup lang="ts">
// Home (frontend §5): `Running` above `All repositories`, because the home
// screen's job is "what is wrong" and a catalog of fifty repos answers a
// different question.
//
// Everything rendered here is read from the stream store's entities through
// the catalog read model. This view fetches GET /api/repos and GET
// /api/workspaces, each handed to the reducer as a snapshot, and registers
// both as its backstop: refetched when the stream reopens, on `resync`, and —
// the catalog — on any repo.* event. The workspace list is what makes the
// `Running` section whole: the catalog joins only each repository's newest
// workspace, so an older one still holding a container would otherwise be
// missing from it.
//
// Every card's status line and action come from lib/workspaceCard.ts (§6.1);
// every button's lifecycle from ActionButton (§4.2), and a workspace's
// action from WorkspaceAction, so a card and a row cannot send different
// things for the same word.
import { computed, onMounted } from 'vue'
import { RouterLink } from 'vue-router'
import { describeError } from '../api/messages'
import ActionButton from '../components/ActionButton.vue'
import MakeRoom from '../components/MakeRoom.vue'
import WorkspaceAction from '../components/WorkspaceAction.vue'
import WorkspaceIdentityNote from '../components/WorkspaceIdentityNote.vue'
import { catalogEvent, useCatalogStore, type CatalogRow } from '../stores/catalog'
import type { Workspace } from '../stores/reducer'
import { cloneKey, useWorkspacesStore } from '../stores/workspaces'
import { useStreamRefetch } from '../lib/refetch'
import { relativeTime } from '../lib/time'
import { cardStatus, rowAction, withRoom, type CardAction } from '../lib/workspaceCard'

const catalog = useCatalogStore()
const workspaces = useWorkspacesStore()

onMounted(() => {
  void catalog.load()
  void workspaces.loadList()
})
useStreamRefetch({ refetch: () => catalog.load(), when: catalogEvent })
useStreamRefetch({ refetch: () => workspaces.loadList() })

const status = (w: Workspace) => cardStatus(w)
// A row's action at the cap: Clone (or a Start) becomes the pointer to Stop.
const shownRowAction = (r: CatalogRow) => {
  const a = rowAction(r)
  return a === 'clone' ? withRoom('clone', null, catalog.capacity.full) : a
}
const wsLink = (w: Workspace) => ({ name: 'workspace', params: { id: w.id } })

const searching = computed(() => catalog.query.trim() !== '')
const firstLoad = computed(() => catalog.status === 'loading' || catalog.status === 'idle')
const awaitingFirstRefresh = computed(() => catalog.status === 'ready' && catalog.refreshedAt === null)

function rowNote(r: CatalogRow): string | null {
  if (r.repo.removed) return 'Removed from the GitHub App. Unpushed work in the working tree survives.'
  return null
}
</script>

<template>
  <section class="view" aria-labelledby="ws-h">
    <h1 id="ws-h" tabindex="-1">Workspaces</h1>

    <div id="running" class="block" data-test="running">
      <div class="sec-label">
        <span>Running</span>
        <!-- The cap beside the section the slots are in (frontend §9, §4.5 #17). -->
        <span v-if="catalog.capacity.cap !== null" data-test="capacity">
          {{ catalog.capacity.occupied }} of {{ catalog.capacity.cap }} slots
        </span>
        <span v-else>{{ catalog.running.length }}</span>
      </div>
      <ul v-if="catalog.running.length > 0" class="list">
        <li v-for="r in catalog.running" :key="r.workspace.id" class="row" data-test="running-row">
          <div class="head">
            <RouterLink :to="wsLink(r.workspace)" class="name" data-test="ws-link">
              {{ r.repo?.fullName ?? r.workspace.fullName ?? r.workspace.id }}
            </RouterLink>
            <span class="state" :class="status(r.workspace).tone" data-test="running-state">
              <WorkspaceIdentityNote :state="r.workspace.state" part="dot" />
              {{ status(r.workspace).line }}
            </span>
          </div>
          <p v-if="status(r.workspace).note" class="detail">{{ status(r.workspace).note }}</p>
          <WorkspaceIdentityNote :state="r.workspace.state" part="waiting" />
          <WorkspaceAction :workspace="r.workspace" :action="status(r.workspace).action" />
        </li>
      </ul>
      <div v-else class="empty">
        <span>Nothing is running.</span>
        <span class="sub">Workspaces appear here once a repository has been cloned.</span>
      </div>
    </div>

    <div class="block" data-test="catalog">
      <div class="sec-label">
        <span>All repositories</span>
        <span v-if="catalog.status === 'ready'">{{ catalog.rows.length }}</span>
      </div>

      <!-- No App: say so, and say what makes it non-empty (§9). -->
      <div v-if="catalog.status === 'not_configured'" class="empty" data-test="app-not-configured">
        <span>No GitHub App is set up yet, so there are no repositories to show.</span>
        <span class="sub">
          Start <code>drydock serve</code> with <code>--github-app-id</code> and <code>--github-app-key</code>,
          then install the App on the repositories Drydock should see.
        </span>
      </div>

      <div v-else-if="catalog.status === 'error'" class="msg bad" role="alert" data-test="catalog-error">
        <span class="glyph" aria-hidden="true">×</span>
        <span>{{ describeError(catalog.error) }}</span>
      </div>

      <div v-else-if="firstLoad" class="empty" data-test="catalog-loading">
        <span>Loading repositories…</span>
      </div>

      <template v-else>
        <div v-if="catalog.error" class="msg warn" role="status" data-test="catalog-stale">
          <span class="glyph" aria-hidden="true">!</span>
          <span>Could not update the list: {{ describeError(catalog.error) }} Showing what was last loaded.</span>
        </div>

        <div class="field search">
          <label for="repo-search" class="vis-sr">Search repositories</label>
          <input
            id="repo-search" v-model="catalog.query" type="search" placeholder="Search repositories"
            autocomplete="off" autocapitalize="off" spellcheck="false" data-test="search"
          >
        </div>

        <div v-if="awaitingFirstRefresh" class="empty" data-test="catalog-first-refresh">
          <span>Reading the repository list from GitHub…</span>
          <span class="sub">It appears here as soon as the first refresh finishes.</span>
        </div>

        <ul v-else-if="catalog.filtered.length > 0" class="list" data-test="repos">
          <li
            v-for="r in catalog.filtered" :key="r.repo.id" class="row"
            :class="{ removed: r.repo.removed }" data-test="repo"
          >
            <div class="head">
              <RouterLink v-if="r.workspace" :to="wsLink(r.workspace)" class="name">{{ r.repo.fullName }}</RouterLink>
              <span v-else class="name">{{ r.repo.fullName }}</span>
              <span v-if="r.workspace" class="state" :class="status(r.workspace).tone" data-test="repo-state">
                {{ status(r.workspace).line }}
              </span>
            </div>
            <div class="badges">
              <span v-if="r.repo.removed" class="badge warn" data-test="badge-removed">read-only</span>
              <!-- null is unknown: no badge at all, never "no dev container". -->
              <span v-if="r.repo.hasDevcontainer === true" class="badge ok" data-test="badge-devcontainer">dev container</span>
              <span v-else-if="r.repo.hasDevcontainer === false" class="badge" data-test="badge-no-devcontainer">no dev container</span>
              <span v-if="r.repo.archived" class="badge" data-test="badge-archived">archived</span>
              <span v-if="r.repo.private" class="badge">private</span>
              <span v-if="r.repo.pushedAt" class="pushed">pushed {{ relativeTime(r.repo.pushedAt) }}</span>
            </div>
            <!--
              One action per row (§6.1), from lib/workspaceCard.ts rowAction:
              Clone only when no workspace holds the repo in any state;
              otherwise its workspace's own card action. At the cap, Clone is
              replaced by the pointer to Stop, never shown disabled (§9).
            -->
            <ActionButton
              v-if="shownRowAction(r) === 'clone'" label="Clone"
              :flight-key="cloneKey(r.repo.id)" :run="() => workspaces.create(r.repo.id)"
              data-test="clone"
            />
            <MakeRoom v-else-if="shownRowAction(r) === 'make_room'" />
            <WorkspaceAction v-else-if="r.workspace" :workspace="r.workspace" :action="rowAction(r) as CardAction" />
            <p v-if="rowNote(r)" class="detail" data-test="removed-note">
              {{ rowNote(r) }}
              <a
                v-if="r.installation" :href="r.installation.settings_url"
                target="_blank" rel="noopener noreferrer"
              >Installation settings</a>
            </p>
          </li>
        </ul>

        <p v-if="searching && catalog.filtered.length === 0 && !awaitingFirstRefresh" class="none" data-test="no-match">
          No repository matches “{{ catalog.query.trim() }}”.
        </p>

        <!-- §9: the line under the search that answers "where is my repo?". -->
        <div
          v-if="!awaitingFirstRefresh && (catalog.filtered.length === 0 || catalog.rows.length === 0)"
          class="empty" data-test="catalog-empty"
        >
          <span>Repos appear here when the GitHub App is installed on them.</span>
          <span v-if="catalog.installations.length > 0" class="sub">
            <template v-for="(i, n) in catalog.installations" :key="i.id">
              <template v-if="n > 0"> · </template>
              <a :href="i.settings_url" target="_blank" rel="noopener noreferrer" data-test="settings-link">
                Installation settings for {{ i.account }}
              </a>
            </template>
          </span>
          <span v-else class="sub" data-test="no-installations">The App is not installed on any account yet.</span>
        </div>

        <p v-if="catalog.refreshError" class="foot refresh-error" role="status" data-test="refresh-error">
          The last refresh failed {{ relativeTime(catalog.refreshError.at) }}: {{ catalog.refreshError.message }}
          The list above is from the last one that worked.
        </p>
        <p class="foot">
          <template v-if="catalog.refreshedAt">Refreshed {{ relativeTime(catalog.refreshedAt) }}. </template>
          <RouterLink to="/settings">Refresh in Settings</RouterLink>
        </p>
      </template>
    </div>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 18px; }
.block { display: flex; flex-direction: column; gap: 8px; }

.list {
  list-style: none; margin: 0; padding: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.row { padding: 11px 12px; border-bottom: 1px solid var(--line-soft); display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.row:last-child { border-bottom: 0; }
.row.removed { border-left: 3px solid var(--warn); padding-left: 9px; }
/* A long status line ("Failed while starting the container") wraps under the
 * name rather than squeezing it at 360 px. */
.head { display: flex; flex-wrap: wrap; align-items: baseline; justify-content: space-between; gap: 2px 8px; min-width: 0; }
.head .name { flex: 0 1 auto; min-width: 0; }
.name { font-family: var(--mono); font-size: 13.5px; overflow-wrap: anywhere; }
a.name { color: var(--ink); text-decoration: underline; text-decoration-color: var(--line); text-underline-offset: 3px; }
.state { font-size: 12.5px; color: var(--ink-2); flex: none; text-align: right; }
.state.ok { color: var(--ok); }
.state.bad { color: var(--bad); }
.state.busy { color: var(--warn); }
.state.idle { color: var(--ink-3); }
.detail { font-size: 12.5px; color: var(--ink-3); }
.badges { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 6px; }
.badge {
  font-family: var(--mono); font-size: 10px; letter-spacing: .05em; text-transform: uppercase;
  padding: 2px 6px; border-radius: 3px; background: var(--surface-2); color: var(--ink-2);
}
.badge.ok { background: var(--ok-bg); color: var(--ok); }
.badge.warn { background: var(--warn-bg); color: var(--warn); }
.pushed { font-family: var(--mono); font-size: 11px; color: var(--ink-3); }
.none { font-size: 13px; color: var(--ink-2); }
.foot { font-size: 12.5px; color: var(--ink-3); }
.search input { font-size: 16px; }
</style>
