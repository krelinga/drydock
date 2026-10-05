<script setup lang="ts">
// /ws/:id — the workspace detail (frontend §5, §6.1).
//
// Rendered entirely from the stream store's entities: the workspace, its
// step timeline and its feed. This view's own job is the fetch — GET
// /api/workspaces/:id on entry, handed to the reducer as a snapshot — and
// registering that fetch as its backstop for a reopened stream or a `resync`.
// After that it lives off the stream, so a clone started on another device
// moves here without a reload.
//
// Event messages are rendered as text, never HTML (§8): `message` is the
// server's prose about things that can carry repository content.
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { WORKSPACE_STEPS, type StreamEvent } from '../api/types'
import { describeError } from '../api/messages'
import ActionButton from '../components/ActionButton.vue'
import { useStreamRefetch } from '../lib/refetch'
import { relativeTime } from '../lib/time'
import { cardStatus, stepTitle } from '../lib/workspaceCard'
import { failedStep, runSteps } from '../stores/reducer'
import { useStreamStore } from '../stores/stream'
import { startKey, useWorkspacesStore } from '../stores/workspaces'

const route = useRoute()
const stream = useStreamStore()
const workspaces = useWorkspacesStore()

const id = computed(() => String(route.params.id ?? ''))
const ws = computed(() => stream.entities.workspaces[id.value] ?? null)
const deleted = computed(() => stream.entities.gone[id.value] !== undefined)
const load = computed(() => workspaces.detail[id.value] ?? { status: 'idle', error: null })
const status = computed(() => (ws.value !== null ? cardStatus(ws.value) : null))

const name = computed(() => {
  const w = ws.value
  if (w === null) return id.value
  const repo = w.repositoryId !== null ? stream.entities.repos[w.repositoryId] : undefined
  return w.fullName ?? repo?.fullName ?? w.id
})

onMounted(() => void workspaces.loadOne(id.value))
watch(id, (now) => void workspaces.loadOne(now))
// The backstop only: a reopened stream or a `resync`. Nothing else here needs
// a refetch — the move to running and `workspace.adopted` both carry the
// container id, so the reducer has it from the event (design §6).
useStreamRefetch({ refetch: () => workspaces.loadOne(id.value) })

const failed = computed(() => (ws.value?.state === 'failed' ? failedStep(ws.value) : null))

// Only the current run's steps (reducer.ts runSteps): after a start, a step
// the earlier run reached and this one has not is "not run", not its old
// status.
const current = computed(() => (ws.value !== null ? runSteps(ws.value) : {}))
const timeline = computed(() => WORKSPACE_STEPS.map((step) => {
  const rec = current.value[step] ?? null
  return { step, title: stepTitle(step), rec, isFailed: failed.value === step }
}))

const GLYPH: Record<string, string> = { done: '✓', started: '…', failed: '×' }
const WORD: Record<string, string> = { done: 'done', started: 'running', failed: 'failed' }

// §7: newest at the bottom, scrolling inside its own box, and never moved
// under a reader — the box follows new events only while already at the end.
const feed = computed<StreamEvent[]>(() => [...(stream.entities.feeds[id.value] ?? [])].reverse())
const box = ref<HTMLElement | null>(null)
const atEnd = ref(true)
function onScroll(): void {
  const el = box.value
  if (el !== null) atEnd.value = el.scrollTop + el.clientHeight >= el.scrollHeight - 8
}
function jump(): void {
  const el = box.value
  if (el !== null) el.scrollTop = el.scrollHeight
  atEnd.value = true
}
watch(() => feed.value.length, async () => {
  if (!atEnd.value) return
  await nextTick()
  jump()
})

const shortId = (c: string) => c.slice(0, 12)
</script>

<template>
  <section class="view" aria-labelledby="wsd-h">
    <p class="back"><RouterLink to="/">← Workspaces</RouterLink></p>
    <h1 id="wsd-h" tabindex="-1" class="title" data-test="ws-name">{{ name }}</h1>

    <template v-if="ws && status">
      <div class="card" data-test="ws-card">
        <div class="head">
          <span class="state" :class="status.tone" data-test="ws-state">{{ status.line }}</span>
          <span v-if="ws.branch" class="branch">{{ ws.branch }}</span>
        </div>
        <p v-if="status.note" class="note" data-test="ws-note">{{ status.note }}</p>
        <ActionButton
          v-if="status.action === 'start'" label="Start" primary
          :flight-key="startKey(ws.id)" :run="() => workspaces.start(ws!.id)"
          data-test="start"
        />
        <dl class="facts">
          <div><dt>Workspace</dt><dd class="mono">{{ ws.id }}</dd></div>
          <div v-if="ws.containerId"><dt>Container</dt><dd class="mono" :title="ws.containerId">{{ shortId(ws.containerId) }}</dd></div>
          <div v-if="ws.createdAt"><dt>Created</dt><dd :title="ws.createdAt">{{ relativeTime(ws.createdAt) }}</dd></div>
          <div v-if="ws.adopted"><dt>Adopted</dt><dd>Found running after a restart</dd></div>
        </dl>
      </div>

      <div class="block" data-test="steps">
        <div class="sec-label"><span>Steps</span></div>
        <ol class="steps">
          <li
            v-for="s in timeline" :key="s.step" class="step"
            :class="[s.rec?.status ?? 'none', { failed: s.isFailed }]" data-test="step"
            :data-step="s.step"
          >
            <span class="glyph" aria-hidden="true">{{ s.rec ? GLYPH[s.rec.status] : '·' }}</span>
            <span class="step-name">{{ s.title }}</span>
            <span class="step-status" data-test="step-status">{{ s.rec ? WORD[s.rec.status] : 'not run' }}</span>
            <time v-if="s.rec" class="step-at" :datetime="s.rec.at" :title="s.rec.at">{{ relativeTime(s.rec.at) }}</time>
            <p v-if="s.rec?.detail" class="step-detail" data-test="step-detail">{{ s.rec.detail }}</p>
          </li>
        </ol>
      </div>

      <div class="block" data-test="events">
        <div class="sec-label"><span>Recent events</span><span>{{ feed.length }}</span></div>
        <div v-if="feed.length > 0" ref="box" class="feed" tabindex="0" aria-label="Recent events" @scroll="onScroll">
          <ol>
            <li v-for="e in feed" :key="e.id" class="ev" :class="e.level" data-test="event">
              <span class="glyph" aria-hidden="true">{{ e.level === 'error' ? '×' : e.level === 'warn' ? '!' : '·' }}</span>
              <time class="ev-at" :datetime="e.at" :title="e.at">{{ relativeTime(e.at) }}</time>
              <span class="ev-msg" data-test="event-message">{{ e.message }}</span>
            </li>
          </ol>
        </div>
        <div v-else class="empty"><span>No events yet.</span></div>
        <button v-if="feed.length > 0 && !atEnd" type="button" class="btn ghost jump" @click="jump">Jump to latest</button>
      </div>
    </template>

    <div v-else-if="deleted" class="empty" data-test="ws-deleted">
      <span>This workspace was deleted.</span>
      <span class="sub"><RouterLink to="/">Back to Workspaces</RouterLink></span>
    </div>
    <div v-else-if="load.status === 'not_found'" class="empty" data-test="ws-not-found">
      <span>Drydock has no workspace {{ id }}.</span>
      <span class="sub"><RouterLink to="/">Back to Workspaces</RouterLink></span>
    </div>
    <div v-else-if="load.status === 'error'" class="msg bad" role="alert" data-test="ws-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ describeError(load.error) }}</span>
    </div>
    <div v-else class="empty" data-test="ws-loading"><span>Loading…</span></div>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 16px; }
.block { display: flex; flex-direction: column; gap: 8px; }
.back { font-size: 13px; }
.back a { color: var(--ink-2); text-decoration: none; }
.title { font-family: var(--mono); font-size: 17px; overflow-wrap: anywhere; }

.card {
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
  padding: 12px; display: flex; flex-direction: column; gap: 8px;
}
.head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.state { font-size: 14px; font-weight: 500; color: var(--ink-2); }
.state.ok { color: var(--ok); }
.state.bad { color: var(--bad); }
.state.busy { color: var(--warn); }
.state.idle { color: var(--ink-3); }
.branch { font-family: var(--mono); font-size: 12px; color: var(--ink-3); }
.note { font-size: 13px; color: var(--ink-2); }
.facts { margin: 0; display: grid; grid-template-columns: auto 1fr; gap: 3px 12px; font-size: 12.5px; }
.facts > div { display: contents; }
.facts dt { color: var(--ink-3); }
.facts dd { margin: 0; color: var(--ink-2); overflow-wrap: anywhere; }
.mono { font-family: var(--mono); }

.steps {
  list-style: none; margin: 0; padding: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.step {
  display: grid; grid-template-columns: 18px 1fr auto; gap: 2px 8px; align-items: baseline;
  padding: 8px 12px; border-bottom: 1px solid var(--line-soft); font-size: 13.5px;
}
.step:last-child { border-bottom: 0; }
.step .glyph { font-family: var(--mono); font-weight: 700; color: var(--ink-3); }
.step.done .glyph { color: var(--ok); }
.step.started .glyph { color: var(--warn); }
.step.failed .glyph, .step.failed .step-status { color: var(--bad); }
.step.failed { background: var(--bad-bg); }
.step.none .step-name { color: var(--ink-3); }
.step-status { font-size: 12px; color: var(--ink-2); text-align: right; }
.step-at { grid-column: 2; font-family: var(--mono); font-size: 11px; color: var(--ink-3); }
.step-detail { grid-column: 2 / 4; font-size: 12.5px; color: var(--ink-2); }

.feed {
  max-height: 22rem; overflow-y: auto; overscroll-behavior: contain;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.feed ol { list-style: none; margin: 0; padding: 4px 0; }
.ev { display: grid; grid-template-columns: 14px auto 1fr; gap: 0 8px; padding: 4px 12px; font-size: 12.5px; }
.ev .glyph { font-family: var(--mono); font-weight: 700; color: var(--ink-3); }
.ev.warn .glyph { color: var(--warn); }
.ev.error .glyph, .ev.error .ev-msg { color: var(--bad); }
.ev-at { font-family: var(--mono); font-size: 11px; color: var(--ink-3); white-space: nowrap; }
.ev-msg { white-space: pre-wrap; overflow-wrap: anywhere; }
.jump { align-self: flex-start; }
</style>
