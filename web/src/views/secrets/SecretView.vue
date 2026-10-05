<script setup lang="ts">
// /secrets/:name (frontend §5, §6.4, §6.5): one secret — what it reaches,
// where it is granted, who fetched it — and the three things to do with it:
// change its grants, replace its value, delete it.
//
// Everything about the secret itself is read from the entity. The replace
// form and the delete sheet are local and transient; the save's result (the
// two kinds of stale) is held here only until it is dismissed or the view
// unmounts, because it is what *that* save did, not what the secret is.
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { describeError } from '../../api/messages'
import { useStreamRefetch } from '../../lib/refetch'
import { relativeTime } from '../../lib/time'
import { useCatalogStore } from '../../stores/catalog'
import { deleteKey, useSecretsStore, type PutOutcome } from '../../stores/secrets'
import { useSessionStore } from '../../stores/session'
import { useStreamStore } from '../../stores/stream'
import { useWorkspacesStore } from '../../stores/workspaces'
import GrantsEditor from './GrantsEditor.vue'
import SaveResult from './SaveResult.vue'
import SecretForm from './SecretForm.vue'
import { workspaceName } from './names'

const route = useRoute()
const router = useRouter()
const secrets = useSecretsStore()
const catalog = useCatalogStore()
const workspaces = useWorkspacesStore()
const stream = useStreamStore()
const session = useSessionStore()

const name = computed(() => String(route.params.name ?? ''))
const secret = computed(() => secrets.byName(name.value))

onMounted(() => {
  void secrets.load()
  void catalog.load() // the grants editor picks from it
  void workspaces.loadList()
})
useStreamRefetch({ refetch: () => secrets.load() })

const replacing = ref(false)
const result = ref<PutOutcome | null>(null)
const confirmingDelete = ref(false)
const deleteError = ref<string | null>(null)
const deleteFlight = computed(() => stream.inFlight[deleteKey(name.value)] ?? null)

function saved(_name: string, outcome: PutOutcome): void {
  replacing.value = false
  result.value = outcome
}

function startReplace(): void {
  result.value = null
  replacing.value = true
}

async function remove(): Promise<void> {
  deleteError.value = null
  try {
    await secrets.remove(name.value)
    await router.replace({ name: 'secrets' })
  } catch (e) {
    if (session.status === 'signed-in') deleteError.value = describeError(e)
  }
}

const fetchedBy = computed(() => (secret.value?.accessedBy ?? []).map((id) => ({ id, label: workspaceName(stream.entities, id), known: stream.entities.workspaces[id] !== undefined })))
</script>

<template>
  <section class="view" aria-labelledby="secret-h">
    <RouterLink :to="{ name: 'secrets' }" class="back">← Secrets</RouterLink>
    <h1 id="secret-h" tabindex="-1" class="mono">{{ name }}</h1>

    <div v-if="secrets.status === 'not_configured'" class="empty" data-test="secrets-not-configured">
      <span>Drydock has no secrets key, so secrets cannot be stored.</span>
    </div>

    <div v-else-if="!secret && secrets.loaded" class="empty" data-test="secret-missing">
      <span>There is no secret named {{ name }}.</span>
      <span class="sub">It may have been deleted, here or on another device.</span>
    </div>

    <div v-else-if="!secret && secrets.status === 'error'" class="msg bad" role="alert">
      <span class="glyph" aria-hidden="true">×</span><span>{{ describeError(secrets.error) }}</span>
    </div>

    <div v-else-if="!secret" class="empty"><span>Loading…</span></div>

    <template v-else>
      <section class="block" aria-labelledby="reach-h">
        <div class="sec-label"><span id="reach-h">What can someone do with this?</span></div>
        <p class="prose" data-test="detail-reach">{{ secret.reach }}</p>
        <template v-if="secret.description">
          <div class="sec-label"><span>Where it came from, and how to rotate it</span></div>
          <p class="prose sub" data-test="detail-description">{{ secret.description }}</p>
        </template>
      </section>

      <GrantsEditor :secret="secret" />

      <section class="block" aria-labelledby="value-h">
        <div class="sec-label"><span id="value-h">Value</span></div>
        <p class="meta" data-test="detail-rotated">
          Not shown, ever.
          <template v-if="secret.rotatedAt">Last replaced {{ relativeTime(secret.rotatedAt) }}.</template>
          <template v-else-if="secret.createdAt">Unchanged since it was stored {{ relativeTime(secret.createdAt) }}.</template>
        </p>
        <SaveResult v-if="result" :name="secret.name" :outcome="result" @dismiss="result = null" />
        <SecretForm
          v-if="replacing" mode="rotate"
          :initial="{ name: secret.name, reach: secret.reach, description: secret.description }"
          @saved="saved" @cancel="replacing = false"
        />
        <div v-else-if="!result">
          <button type="button" class="btn" data-test="replace" @click="startReplace">Replace the value…</button>
        </div>
      </section>

      <section class="block" aria-labelledby="access-h">
        <div class="sec-label"><span id="access-h">Fetched by</span></div>
        <p v-if="!secret.lastAccessAt" class="meta" data-test="detail-access">Never fetched by any workspace.</p>
        <template v-else>
          <p class="meta" data-test="detail-access">Last fetched {{ relativeTime(secret.lastAccessAt) }}. Every workspace that ever held it, newest first:</p>
          <ul class="fetched">
            <li v-for="w in fetchedBy" :key="w.id">
              <RouterLink v-if="w.known" :to="{ name: 'workspace', params: { id: w.id } }">{{ w.label }}</RouterLink>
              <span v-else>{{ w.label }}</span>
            </li>
          </ul>
        </template>
      </section>

      <section class="block" aria-labelledby="delete-h">
        <div class="sec-label"><span id="delete-h">Delete</span></div>
        <div v-if="deleteError" class="msg bad" role="alert" data-test="delete-error">
          <span class="glyph" aria-hidden="true">×</span><span>{{ deleteError }}</span>
        </div>
        <div>
          <button
            v-if="!confirmingDelete" type="button" class="btn ghost danger-text" data-test="delete"
            @click="confirmingDelete = true"
          >Delete secret…</button>
        </div>
        <!-- §6.5's sheet, scaled down: it says what goes, and what stays. -->
        <div v-if="confirmingDelete" class="sheet" role="group" aria-labelledby="delete-sheet-h" data-test="confirm-delete">
          <h2 id="delete-sheet-h">Delete {{ secret.name }}?</h2>
          <p data-test="delete-says">
            This removes the stored value and every grant. No workspace receives it from its next command on.
            The record of which workspaces fetched it is kept.
          </p>
          <p class="sub">
            Deleting it here does not revoke it. If it may have leaked, rotate it where it came from as well.
          </p>
          <div class="act">
            <button type="button" class="btn" data-test="delete-cancel" :disabled="deleteFlight !== null" @click="confirmingDelete = false">Cancel</button>
            <button
              type="button" class="btn danger" data-test="delete-confirm"
              :disabled="deleteFlight !== null" :aria-busy="deleteFlight !== null" @click="remove"
            >Delete secret</button>
          </div>
        </div>
      </section>
    </template>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 18px; }
.back { font-size: 13px; }
.mono { font-family: var(--mono); font-size: 18px; overflow-wrap: anywhere; }
.block { display: flex; flex-direction: column; gap: 8px; }
.prose { font-size: 14px; white-space: pre-wrap; overflow-wrap: anywhere; }
.prose.sub, .sub { font-size: 13px; color: var(--ink-2); }
.meta { font-size: 12.5px; color: var(--ink-3); }
.fetched { margin: 0; padding-left: 18px; font-family: var(--mono); font-size: 12.5px; }
.danger-text { color: var(--bad); border-color: var(--bad); }
.sheet {
  background: var(--surface); border: 1px solid var(--line); border-top: 2px solid var(--bad);
  border-radius: var(--r); padding: 14px; display: flex; flex-direction: column; gap: 10px; font-size: 14px;
}
.act { display: flex; gap: 8px; }
.act .btn { flex: 1; }
</style>
