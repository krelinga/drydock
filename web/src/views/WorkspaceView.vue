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
// Phase 6 puts the lifecycle here: the card's one action (WorkspaceAction,
// the same button the home list shows), Rebuild where it is not already that
// action, a stop's or a delete's sub-steps while it runs or where it stuck,
// and Delete behind §6.5's confirm — the repository's full name, typed, and
// nothing else.
//
// Event messages are rendered as text, never HTML (§8): `message` is the
// server's prose about things that can carry repository content.
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { WORKSPACE_STEPS, type StreamEvent } from '../api/types'
import { describeError } from '../api/messages'
import ActionButton from '../components/ActionButton.vue'
import WorkspaceAction from '../components/WorkspaceAction.vue'
import WorkspaceIdentityNote from '../components/WorkspaceIdentityNote.vue'
import { useStreamRefetch } from '../lib/refetch'
import { relativeTime } from '../lib/time'
import MakeRoom from '../components/MakeRoom.vue'
import ResourceLine from '../components/ResourceLine.vue'
import { diskBreakdown } from '../lib/resources'
import { capacity } from '../lib/capacity'
import { actionStepTitle, cardStatus, stepTitle, withRoom } from '../lib/workspaceCard'
import { ACTION_STEPS, failedStep, liveAction, runSteps, stopFailed } from '../stores/reducer'
import { useStreamStore } from '../stores/stream'
import { deleteKey, rebuildKey, stopKey, useWorkspacesStore } from '../stores/workspaces'

const route = useRoute()
const stream = useStreamStore()
const workspaces = useWorkspacesStore()

const id = computed(() => String(route.params.id ?? ''))
const ws = computed(() => stream.entities.workspaces[id.value] ?? null)
const deleted = computed(() => stream.entities.gone[id.value] !== undefined)
const load = computed(() => workspaces.detail[id.value] ?? { status: 'idle', error: null })
const status = computed(() => (ws.value !== null ? cardStatus(ws.value, stream.entities.identity?.state ?? null) : null))

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

const GLYPH: Record<string, string> = { done: '✓', started: '…', failed: '×', needs_approval: '!' }
const WORD: Record<string, string> = { done: 'done', started: 'running', failed: 'failed', needs_approval: 'needs approval' }

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

// What the disk figure counts (design §6 *Resources*): what a delete frees.
const breakdown = computed(() => diskBreakdown(stream.entities.resources[id.value]))

// A stop's or a delete's sub-steps: while one runs, where a stop failed, and
// for as long as a delete has not finished — a stuck one names its sub-step.
// A failed stop and a stuck delete are each ended by the server's annotation,
// so neither is live by then; each keeps its run here until something moves
// the workspace on or a retry clears the annotation.
const run = computed(() => {
  const w = ws.value
  if (w === null) return null
  const live = liveAction(w)
  if (live !== null) return live
  if (w.state === 'deleting' && w.action?.name === 'delete') return w.action
  return stopFailed(w) && w.action?.name === 'stop' ? w.action : null
})
const runRows = computed(() => {
  const r = run.value
  if (r === null) return []
  const names = [...(ACTION_STEPS[r.name] ?? [])]
  for (const n of Object.keys(r.steps)) if (!names.includes(n)) names.push(n)
  return names.map((step) => ({ step, title: actionStepTitle(step), rec: r.steps[step] ?? null }))
})
const RUN_TITLE: Record<string, string> = { stop: 'Stop', delete: 'Delete' }

// Rebuild beside the card's action, where it can work and is not already it:
// from running or stopped, with no stop in progress (design §5).
const canRebuild = computed(() => {
  const w = ws.value
  // Not while a host-access request waits: a rebuild would only ask again.
  if (w === null || status.value === null || status.value.action === 'rebuild' || w.approval !== null) return false
  return (w.state === 'running' || w.state === 'stopped') && liveAction(w) === null
})

// Stop, now that the session half has the card (§6.1): here, beside
// Rebuild, and it asks first when sessions are live — design §12: the count
// makes stop an informed action, and "Capacity N" counts the pre-created one.
const canStop = computed(() => {
  const w = ws.value
  return w !== null && w.state === 'running' && liveAction(w) === null && status.value?.action !== 'stop'
})
const liveSessions = computed(() => {
  const s = ws.value?.session
  return ws.value?.supervisor?.state === 'serving' && s != null && s.capacityUsed !== null ? s.capacityUsed : 0
})
const stopAsked = ref(false)
watch(id, () => { stopAsked.value = false })

// §6.5: the destructive confirm. The typed text is the one piece of local
// state a form may hold (§4.2), compared to the full name exactly — no trim,
// no case folding, as the server compares it — and sent as typed, so the
// server stays the authority. No re-auth prompt (design §13.5).
const confirming = ref(false)
const typed = ref('')
const confirmInput = ref<HTMLInputElement | null>(null)
const deleteFlight = computed(() => stream.inFlight[deleteKey(id.value)] ?? null)
const matches = computed(() => ws.value?.fullName != null && typed.value === ws.value.fullName)
const canDelete = computed(() => ws.value !== null && ws.value.state !== 'deleting' && ws.value.fullName !== null)
const showSheet = computed(() => confirming.value && (canDelete.value || deleteFlight.value !== null))

async function openConfirm(): Promise<void> {
  typed.value = ''
  confirming.value = true
  await nextTick()
  confirmInput.value?.focus()
}
// The delete stuck, or the workspace is gone: the sheet has nothing left to
// confirm. (A delete in flight keeps it, spinner and all, until it settles.)
watch(showSheet, (shown) => {
  if (!shown) confirming.value = false
})
watch(id, () => {
  confirming.value = false
  typed.value = ''
})
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
        <ResourceLine :workspace="ws" />
        <WorkspaceIdentityNote :state="ws.state" part="waiting" />
        <p v-if="status.since" class="note" data-test="waiting-since">Waiting since {{ relativeTime(status.since) }}.</p>
        <WorkspaceAction :workspace="ws" :action="status.action" :link="status.link" primary />
        <dl class="facts">
          <div><dt>Workspace</dt><dd class="mono">{{ ws.id }}</dd></div>
          <div v-if="ws.containerId"><dt>Container</dt><dd class="mono" :title="ws.containerId">{{ shortId(ws.containerId) }}</dd></div>
          <div v-if="ws.createdAt"><dt>Created</dt><dd :title="ws.createdAt">{{ relativeTime(ws.createdAt) }}</dd></div>
          <div v-if="ws.adopted"><dt>Adopted</dt><dd>Found running after a restart</dd></div>
          <div v-if="breakdown" data-test="disk-breakdown">
            <dt>Disk</dt>
            <dd>{{ breakdown }}. Deleting the workspace frees it; images and the shared Claude login are not counted.</dd>
          </div>
          <div v-if="ws.session?.url"><dt>Environment</dt><dd class="mono">{{ ws.session.environmentId }}</dd></div>
          <div v-if="ws.session && ws.session.sessions > 0"><dt>Sessions seen</dt><dd>{{ ws.session.sessions }}</dd></div>
          <div v-if="ws.supervisor && ws.supervisor.restartCount > 0">
            <dt>Restarts</dt><dd>{{ ws.supervisor.restartCount }}</dd>
          </div>
          <div v-if="ws.supervisor">
            <dt>Session log</dt>
            <dd><RouterLink :to="{ name: 'workspace-logs', params: { id: ws.id } }" data-test="logs-link">Open the log</RouterLink></dd>
          </div>
        </dl>
      </div>

      <div v-if="run" class="block" data-test="action-run" :data-action="run.name">
        <div class="sec-label"><span>{{ RUN_TITLE[run.name] ?? run.name }}</span></div>
        <ol class="steps">
          <li
            v-for="s in runRows" :key="s.step" class="step" :class="s.rec?.status ?? 'none'"
            data-test="action-step" :data-step="s.step"
          >
            <span class="glyph" aria-hidden="true">{{ s.rec ? GLYPH[s.rec.status] : '·' }}</span>
            <span class="step-name">{{ s.title }}</span>
            <span class="step-status" data-test="step-status">{{ s.rec ? WORD[s.rec.status] : 'not run' }}</span>
            <time v-if="s.rec" class="step-at" :datetime="s.rec.at" :title="s.rec.at">{{ relativeTime(s.rec.at) }}</time>
            <p v-if="s.rec?.detail" class="step-detail" data-test="step-detail">{{ s.rec.detail }}</p>
          </li>
        </ol>
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

      <div v-if="canStop || canRebuild || canDelete || showSheet" class="block" data-test="more-actions">
        <div class="sec-label"><span>Actions</span></div>
        <div v-if="canStop" class="more" data-test="stop-block">
          <p class="sub">Stop ends the session server and stops the container. The clone survives, and Start brings it back.</p>
          <template v-if="liveSessions > 0 && !stopAsked">
            <button type="button" class="btn" data-test="stop-ask" @click="stopAsked = true">Stop…</button>
          </template>
          <template v-else>
            <p v-if="liveSessions > 0" class="sub" data-test="stop-sessions">
              {{ liveSessions === 1 ? 'One session is' : `${liveSessions} sessions are` }} live. Stopping ends
              {{ liveSessions === 1 ? 'it' : 'them' }}; unpushed work in the clone and its worktrees survives.
            </p>
            <ActionButton
              :label="liveSessions > 0 ? 'Stop and end the sessions' : 'Stop'" :flight-key="stopKey(ws.id)"
              :run="() => workspaces.stop(ws!.id)" data-test="stop"
            />
          </template>
        </div>
        <div v-if="canRebuild" class="more" data-test="rebuild-block">
          <p class="sub">Rebuild replaces the container with a new one from the dev container configuration. The clone, and everything in it, stays.</p>
          <MakeRoom v-if="withRoom('rebuild', ws, capacity(stream.entities).full) === 'make_room'" />
          <ActionButton
            v-else label="Rebuild" :flight-key="rebuildKey(ws.id)" :run="() => workspaces.rebuild(ws!.id)"
            data-test="rebuild"
          />
        </div>
        <div v-if="canDelete && !showSheet">
          <button type="button" class="btn ghost danger-text" data-test="delete" @click="openConfirm">Delete workspace…</button>
        </div>
        <!-- §6.5's sheet: in place, not a modal (§7), with the input at its top. -->
        <div v-if="showSheet" class="sheet" role="group" aria-labelledby="del-h" data-test="confirm-delete">
          <h2 id="del-h">Delete this workspace?</h2>
          <p data-test="delete-goes">
            This removes its container, its clone, and any unpushed work in that clone: commits not yet pushed,
            uncommitted changes, and every worktree.
          </p>
          <p data-test="delete-survives">
            Nothing of the workspace survives. The repository on GitHub is untouched, and whatever was pushed is
            safe there.
          </p>
          <div class="field">
            <label for="del-confirm">Type <code class="name">{{ ws.fullName }}</code> to confirm</label>
            <input
              id="del-confirm" ref="confirmInput" v-model="typed" type="text" data-test="delete-input"
              autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false"
              :disabled="deleteFlight !== null"
            >
          </div>
          <div class="act">
            <button
              type="button" class="btn" data-test="delete-cancel" :disabled="deleteFlight !== null"
              @click="confirming = false"
            >Cancel</button>
            <ActionButton
              label="Delete workspace" danger :blocked="!matches"
              :flight-key="deleteKey(ws.id)" :run="() => workspaces.remove(ws!.id, typed)"
              data-test="delete-confirm"
            />
          </div>
        </div>
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
.step.needs_approval .glyph, .step.needs_approval .step-status { color: var(--warn); }
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

.more { display: flex; flex-direction: column; gap: 6px; }
.sub { font-size: 13px; color: var(--ink-2); }
.danger-text { color: var(--bad); border-color: var(--bad); }
.sheet {
  background: var(--surface); border: 1px solid var(--line); border-top: 2px solid var(--bad);
  border-radius: var(--r); padding: 14px; display: flex; flex-direction: column; gap: 10px; font-size: 14px;
}
.sheet .name { overflow-wrap: anywhere; }
.act { display: flex; gap: 8px; align-items: flex-start; }
.act > * { flex: 1; }
.act :deep(.btn) { width: 100%; }
</style>
