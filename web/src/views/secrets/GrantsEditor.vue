<script setup lang="ts">
// Which repositories a secret reaches (design §10.1, frontend §6.4).
//
// Default deny is the visible default: a secret with no grants says, in
// words, that nothing receives it. Repositories are picked from the catalog.
// `all_repos` is a decision, not a checkbox — it has its own button and its
// own confirm, which names the count and says the part that is easy to miss:
// it covers repositories added to the installation *later*, too.
//
// A grant change is not a rotation. The broker resolves the grant set on
// every call, so the next command sees it with nothing to restart and nothing
// stale, and this component says so rather than implying otherwise (§6.4).
//
// The checkboxes are a draft (§4.2: a form being edited). What the secret is
// granted to is always read from the entity, which only events and the list
// write — the 200 the grants PUT returns is not applied.
import { computed, ref } from 'vue'
import { describeError } from '../../api/messages'
import { useCatalogStore } from '../../stores/catalog'
import type { Secret } from '../../stores/reducer'
import { grantsKey, useSecretsStore } from '../../stores/secrets'
import { useSessionStore } from '../../stores/session'
import { useStreamStore } from '../../stores/stream'

const props = defineProps<{ secret: Secret }>()

const catalog = useCatalogStore()
const secrets = useSecretsStore()
const stream = useStreamStore()
const session = useSessionStore()

const flight = computed(() => stream.inFlight[grantsKey(props.secret.name)] ?? null)
const editing = ref(false)
const confirmingAll = ref(false)
const draft = ref<Set<number>>(new Set())
const error = ref<string | null>(null)

const granted = computed(() => props.secret.grants.map((g) => g.repositoryId))
const nothing = computed(() => !props.secret.allRepos && props.secret.grants.length === 0)

/** The catalog, plus any granted repository it no longer lists, so a grant can always be removed. */
const choices = computed(() => {
  const rows = catalog.rows.map((r) => ({ id: r.repo.id, fullName: r.repo.fullName, note: r.repo.removed ? 'removed from the App' : r.repo.archived ? 'archived' : null }))
  for (const g of props.secret.grants) {
    if (!rows.some((r) => r.id === g.repositoryId)) rows.push({ id: g.repositoryId, fullName: g.fullName, note: 'not in the catalog' })
  }
  return rows
})
const repoCount = computed(() => catalog.rows.length)

function edit(): void {
  draft.value = new Set(granted.value)
  error.value = null
  editing.value = true
}

function toggle(id: number, on: boolean): void {
  const next = new Set(draft.value)
  if (on) next.add(id)
  else next.delete(id)
  draft.value = next
}

async function send(ids: number[], allRepos: boolean): Promise<boolean> {
  error.value = null
  try {
    await secrets.setGrants(props.secret.name, ids, allRepos)
    return true
  } catch (e) {
    if (session.status === 'signed-in') error.value = describeError(e)
    return false
  }
}

async function saveDraft(): Promise<void> {
  if (await send([...draft.value].sort((a, b) => a - b), props.secret.allRepos)) editing.value = false
}

async function grantAll(): Promise<void> {
  if (await send(granted.value, true)) confirmingAll.value = false
}

async function limit(): Promise<void> {
  await send(granted.value, false)
}
</script>

<template>
  <section class="grants" aria-labelledby="grants-h" data-test="grants">
    <div class="sec-label"><span id="grants-h">Granted to</span></div>

    <div v-if="secret.allRepos" class="msg warn all" data-test="grants-all">
      <span class="glyph" aria-hidden="true">!</span>
      <span>
        <b>All repositories.</b> Every workspace receives it, including ones for repositories added to the
        installation later.
      </span>
    </div>
    <p v-else-if="nothing" class="empty-grant" data-test="grants-none">
      Granted to nothing. No workspace receives this secret until you grant it a repository.
    </p>
    <ul v-if="secret.grants.length > 0" class="repos" data-test="grants-list">
      <li v-for="g in secret.grants" :key="g.repositoryId">{{ g.fullName || `repository ${g.repositoryId}` }}</li>
    </ul>
    <p v-if="secret.allRepos && secret.grants.length > 0" class="help">
      The repositories above are kept for when it is limited again.
    </p>

    <div v-if="error" class="msg bad" role="alert" data-test="grants-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ error }}</span>
    </div>

    <div v-if="editing" class="editor" role="group" aria-labelledby="grants-edit-h" data-test="grants-editor">
      <h3 id="grants-edit-h">Choose repositories</h3>
      <p v-if="catalog.status === 'not_configured'" class="help">No GitHub App is set up yet, so there are no repositories to choose from.</p>
      <p v-else-if="choices.length === 0" class="help">Loading repositories…</p>
      <ul class="choices">
        <li v-for="c in choices" :key="c.id">
          <label>
            <input
              type="checkbox" :checked="draft.has(c.id)" data-test="grant-choice" :data-repo="c.id"
              @change="toggle(c.id, ($event.target as HTMLInputElement).checked)"
            >
            <span class="repo">{{ c.fullName }}</span>
            <span v-if="c.note" class="note">{{ c.note }}</span>
          </label>
        </li>
      </ul>
      <div class="act">
        <button type="button" class="btn primary" data-test="grants-save" :disabled="flight !== null" :aria-busy="flight !== null" @click="saveDraft">
          Save grants
        </button>
        <button type="button" class="btn ghost" data-test="grants-cancel" :disabled="flight !== null" @click="editing = false">Cancel</button>
      </div>
    </div>

    <!-- all_repos: its own confirm, a sheet in place (§6.5, §7: no modals). -->
    <div v-else-if="confirmingAll" class="sheet" role="group" aria-labelledby="all-h" data-test="confirm-all">
      <h3 id="all-h">Grant {{ secret.name }} to every repository?</h3>
      <p data-test="confirm-all-count">
        <template v-if="repoCount > 0">This grants it to all {{ repoCount }} repositories at once</template>
        <template v-else>This grants it to every repository the GitHub App can see</template>,
        <b>and to every repository added to the installation later</b>, without asking again.
      </p>
      <p class="help">
        Every workspace's commands will receive it, so it widens what every repository can reach at once.
        It is meant for the genuinely universal, such as a read-only package registry token.
      </p>
      <div class="act">
        <button type="button" class="btn" data-test="confirm-all-cancel" :disabled="flight !== null" @click="confirmingAll = false">Cancel</button>
        <button type="button" class="btn danger" data-test="confirm-all-yes" :disabled="flight !== null" @click="grantAll">
          Grant to every repository
        </button>
      </div>
    </div>

    <div v-else class="act">
      <button type="button" class="btn" data-test="grants-edit" :disabled="flight !== null" @click="edit">
        {{ secret.grants.length > 0 ? 'Change repositories' : 'Grant repositories' }}
      </button>
      <button
        v-if="!secret.allRepos" type="button" class="btn ghost" data-test="grants-all-ask"
        :disabled="flight !== null" @click="confirmingAll = true"
      >
        Grant to every repository…
      </button>
      <button
        v-else type="button" class="btn ghost" data-test="grants-limit" :disabled="flight !== null" @click="limit"
      >
        Limit to the repositories listed
      </button>
    </div>

    <!-- §6.4: a grant change is not a rotation, and must not read like one. -->
    <p class="help" data-test="grants-no-restart">
      A change here reaches each workspace's next command. Nothing needs restarting.
    </p>
  </section>
</template>

<style scoped>
.grants { display: flex; flex-direction: column; gap: 8px; }
.empty-grant {
  font-size: 14px; color: var(--ink-2);
  border: 1px dashed var(--line); border-radius: var(--r); padding: 10px 12px;
}
.repos { margin: 0; padding-left: 18px; font-family: var(--mono); font-size: 13px; }
.help { font-size: 12.5px; color: var(--ink-3); }
.act { display: flex; gap: 8px; flex-wrap: wrap; }
.editor, .sheet {
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
  padding: 14px; display: flex; flex-direction: column; gap: 10px;
}
.sheet { border-top: 2px solid var(--warn); font-size: 14px; }
h3 { font-size: 14px; font-weight: 600; margin: 0; }
.choices { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; }
.choices label {
  display: flex; align-items: center; gap: 10px; min-height: var(--tap);
  border-bottom: 1px solid var(--line-soft); cursor: pointer; flex-wrap: wrap;
}
.choices input { width: 20px; height: 20px; flex: none; }
.repo { font-family: var(--mono); font-size: 13px; overflow-wrap: anywhere; }
.note { font-size: 11.5px; color: var(--ink-3); }
</style>
