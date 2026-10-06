<script setup lang="ts">
// /ws/:id/logs — the session server's log (frontend §5, §4.5 #4; design §8).
//
// The minimal viewer: GET /api/workspaces/:id/logs, as text, with a refresh.
// The lines are redacted on the server as they are written and live only in
// its memory, so this is a read of something that will not be there after a
// restart, and says so. It is a fetch, not an entity: nothing here is
// workspace state, so it does not go through the reducer. Lines are rendered
// as text, never HTML (§8) — they are a terminal's output.
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import * as api from '../api/client'
import { describeError } from '../api/messages'
import type { LogTail } from '../api/types'
import { useStreamStore } from '../stores/stream'

const route = useRoute()
const stream = useStreamStore()
const id = computed(() => String(route.params.id ?? ''))
const name = computed(() => stream.entities.workspaces[id.value]?.fullName ?? id.value)

const tail = ref<LogTail | null>(null)
const error = ref<api.ApiError | null>(null)
const loading = ref(false)

async function load(): Promise<void> {
  loading.value = true
  try {
    tail.value = await api.get<LogTail>(`/api/workspaces/${encodeURIComponent(id.value)}/logs?tail=500`)
    error.value = null
  } catch (e) {
    error.value = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
  } finally {
    loading.value = false
  }
}
onMounted(() => void load())
watch(id, () => void load())
</script>

<template>
  <section class="view" aria-labelledby="logs-h">
    <p class="back"><RouterLink :to="{ name: 'workspace', params: { id } }">← {{ name }}</RouterLink></p>
    <h1 id="logs-h" tabindex="-1">Session server log</h1>
    <p class="sub">
      Redacted as it was written, kept only in Drydock's memory, and gone after a restart. The newest line is last.
    </p>
    <div class="act">
      <button type="button" class="btn" :disabled="loading" data-test="logs-refresh" @click="load">
        {{ loading ? 'Loading…' : 'Refresh' }}
      </button>
    </div>
    <div v-if="error" class="msg bad" role="alert" data-test="logs-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ describeError(error) }}</span>
    </div>
    <template v-else-if="tail">
      <div v-if="!tail.held" class="empty" data-test="logs-none">
        <span>Drydock holds no log for this workspace.</span>
        <span class="sub">A log exists once Drydock has started its session server, and lasts until Drydock restarts.</span>
      </div>
      <template v-else>
        <p v-if="tail.truncated" class="sub" data-test="logs-truncated">Older lines are no longer held.</p>
        <pre class="log" data-test="logs" tabindex="0" aria-label="Log lines"><template
          v-for="l in tail.lines" :key="l.n"
        ><span class="at">{{ l.at.slice(11, 19) }}</span> {{ l.text }}
</template></pre>
      </template>
    </template>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 12px; }
.back { font-size: 13px; }
.back a { color: var(--ink-2); text-decoration: none; }
.sub { font-size: 13px; color: var(--ink-2); }
.log {
  margin: 0; padding: 8px 12px; max-height: 70vh; overflow: auto; overscroll-behavior: contain;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
  font-family: var(--mono); font-size: 12px; white-space: pre-wrap; overflow-wrap: anywhere;
}
.at { color: var(--ink-3); }
</style>
