<script setup lang="ts">
// /secrets (frontend §5, §6.4): every secret's name, reach, description,
// grants and last access. Never a value — and no affordance that implies one
// exists to be had: no reveal, no copy, no edit-value (§2.5).
//
// Live from the stream (secret.* events through the reducer), and refetched
// on entry, on reopen and on resync. The refetch matters more here than
// elsewhere: a workspace fetching its secrets writes `secret_access` and no
// event, so the last-access column is only as fresh as the last list.
import { computed, onMounted } from 'vue'
import { RouterLink } from 'vue-router'
import { describeError } from '../../api/messages'
import { useStreamRefetch } from '../../lib/refetch'
import { relativeTime } from '../../lib/time'
import type { Secret } from '../../stores/reducer'
import { useSecretsStore } from '../../stores/secrets'
import { useStreamStore } from '../../stores/stream'
import { useWorkspacesStore } from '../../stores/workspaces'
import { workspaceName } from './names'

const secrets = useSecretsStore()
const workspaces = useWorkspacesStore()
const stream = useStreamStore()

onMounted(() => {
  void secrets.load()
  // Only to name the workspaces in "fetched by"; an id is shown otherwise.
  void workspaces.loadList()
})
useStreamRefetch({ refetch: () => secrets.load() })

const firstLoad = computed(() => !secrets.loaded && (secrets.status === 'loading' || secrets.status === 'idle'))
const fetchedBy = (s: Secret) => s.accessedBy.map((id) => workspaceName(stream.entities, id))
</script>

<template>
  <section class="view" aria-labelledby="sec-h">
    <div class="title">
      <h1 id="sec-h" tabindex="-1">Secrets</h1>
      <RouterLink
        v-if="secrets.status !== 'not_configured'" :to="{ name: 'secret-new' }" class="btn primary" data-test="new-secret"
      >New secret</RouterLink>
    </div>

    <div v-if="secrets.status === 'not_configured'" class="empty" data-test="secrets-not-configured">
      <span>Drydock has no secrets key, so secrets cannot be stored.</span>
      <span class="sub">
        Start <code>drydock serve</code> with <code>--secrets-key</code>; the installer creates the key at
        <code>/etc/drydock/secrets.key</code>.
      </span>
    </div>

    <div v-else-if="secrets.status === 'error' && !secrets.loaded" class="msg bad" role="alert" data-test="secrets-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ describeError(secrets.error) }}</span>
    </div>

    <div v-else-if="firstLoad" class="empty" data-test="secrets-loading"><span>Loading secrets…</span></div>

    <template v-else>
      <div v-if="secrets.error" class="msg warn" role="status" data-test="secrets-stale">
        <span class="glyph" aria-hidden="true">!</span>
        <span>Could not update the list: {{ describeError(secrets.error) }} Showing what was last loaded.</span>
      </div>

      <ul v-if="secrets.list.length > 0" class="list" data-test="secrets">
        <li v-for="s in secrets.list" :key="s.name" class="row" data-test="secret">
          <div class="head">
            <RouterLink :to="{ name: 'secret', params: { name: s.name } }" class="name" data-test="secret-name">{{ s.name }}</RouterLink>
            <span v-if="s.allRepos" class="badge warn" data-test="badge-all">all repositories</span>
            <span v-else-if="s.grants.length === 0" class="badge" data-test="badge-none">granted to nothing</span>
          </div>
          <p class="reach"><span class="q">What can someone do with this?</span> <span data-test="secret-reach">{{ s.reach }}</span></p>
          <p v-if="s.description" class="desc" data-test="secret-description">{{ s.description }}</p>
          <p v-if="!s.allRepos && s.grants.length > 0" class="meta" data-test="secret-grants">
            Granted to {{ s.grants.map((g) => g.fullName || `repository ${g.repositoryId}`).join(', ') }}
          </p>
          <p class="meta" data-test="secret-access">
            <template v-if="s.lastAccessAt">
              Last fetched {{ relativeTime(s.lastAccessAt) }} by {{ fetchedBy(s).join(', ') }}
            </template>
            <template v-else>Never fetched by any workspace</template>
          </p>
        </li>
      </ul>

      <div v-else class="empty" data-test="secrets-empty">
        <span>No secrets yet.</span>
        <span class="sub">
          A secret reaches only the repositories you grant it to, and its value is never shown again once
          saved — not here, not anywhere.
        </span>
      </div>
    </template>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 18px; }
.title { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.title .btn { text-decoration: none; }
.list {
  list-style: none; margin: 0; padding: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.row { padding: 11px 12px; border-bottom: 1px solid var(--line-soft); display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.row:last-child { border-bottom: 0; }
.head { display: flex; flex-wrap: wrap; align-items: baseline; justify-content: space-between; gap: 2px 8px; }
.name {
  font-family: var(--mono); font-size: 13.5px; overflow-wrap: anywhere; color: var(--ink);
  text-decoration: underline; text-decoration-color: var(--line); text-underline-offset: 3px;
}
.badge {
  font-family: var(--mono); font-size: 10px; letter-spacing: .05em; text-transform: uppercase;
  padding: 2px 6px; border-radius: 3px; background: var(--surface-2); color: var(--ink-2); flex: none;
}
.badge.warn { background: var(--warn-bg); color: var(--warn); }
.reach { font-size: 13.5px; overflow-wrap: anywhere; white-space: pre-wrap; }
.q { font-size: 12px; color: var(--ink-3); }
.desc { font-size: 12.5px; color: var(--ink-2); overflow-wrap: anywhere; white-space: pre-wrap; }
.meta { font-family: var(--mono); font-size: 11px; color: var(--ink-3); overflow-wrap: anywhere; }
</style>
