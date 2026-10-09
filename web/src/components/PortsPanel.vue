<script setup lang="ts">
// The ports panel (frontend §6.3; port forwarding §6, §8, §13 step 4).
//
// Rendered from the stream store's port entities — the list snapshot and the
// port.* events — and nothing else. Every switch is an ActionButton, in flight
// until the event that ends it. What this component holds locally is the
// legitimate list (§4.2): the add form's fields, which rows' probes were
// asked for and what they answered (a read, never an entity), the remove
// confirm, and the "show hidden" preference.
//
// Off until enabled, always: a declared port is listed, never exposed, and a
// port something in the container listens on (discovery, PF §8.2) appears
// as a row, switched off, the way any row does — no toast, no badge, nothing
// that asks for a click; the panel is where that decision is made (§12). A
// row says what discovery last saw: listening, on which address (loopback
// said plainly, since nothing outside the container can reach it), or not
// listening now. An enabled row shows its full preview host — the address the
// operator is learning to trust is never hidden behind a friendly label
// (§6.3) — and opens it in a new tab, never a frame (§8).
import { computed, onMounted, ref, watch } from 'vue'
import { describeError } from '../api/messages'
import type { ProbeResult } from '../api/types'
import ActionButton from './ActionButton.vue'
import { useStreamRefetch } from '../lib/refetch'
import { portsOf, type Port, type Workspace } from '../stores/reducer'
import {
  portAddKey, portEnableKey, portHideKey, portHostKey, portRescanKey, portRetireKey, usePortsStore,
} from '../stores/ports'
import { useStreamStore } from '../stores/stream'

const props = defineProps<{ workspace: Workspace }>()

const stream = useStreamStore()
const ports = usePortsStore()
const wsId = computed(() => props.workspace.id)

onMounted(() => void ports.load(wsId.value))
watch(wsId, (id) => void ports.load(id))
// The backstop: a reopened stream or a resync.
useStreamRefetch({ refetch: () => ports.load(wsId.value) })

const SHOW_HIDDEN = 'drydock.ports.showHidden'
function readPref(): boolean {
  try {
    return localStorage.getItem(SHOW_HIDDEN) === '1'
  } catch {
    return false
  }
}
const showHidden = ref(readPref())
watch(showHidden, (v) => {
  try {
    localStorage.setItem(SHOW_HIDDEN, v ? '1' : '0')
  } catch {
    // A preference that cannot be kept is only forgotten.
  }
})

const all = computed(() => portsOf(stream.entities, wsId.value))
const hiddenCount = computed(() => all.value.filter((p) => p.hidden).length)
const rows = computed(() => all.value.filter((p) => showHidden.value || !p.hidden))
const loaded = computed(() => stream.entities.portsLoaded[wsId.value] !== undefined)
const load = computed(() => ports.status[wsId.value] ?? null)
const previewsOn = computed(() => stream.entities.previews !== false)
const running = computed(() => props.workspace.state === 'running')

function provenance(p: Port): string[] {
  const out: string[] = []
  if (p.declared) out.push('declared')
  if (p.observed) out.push('discovered')
  if (p.manual) out.push('added by hand')
  return out
}

/** What discovery last saw of it, or null when it never has. */
function observation(p: Port): string | null {
  if (p.observedState === 'listening') {
    if (p.bindAddr === null) return 'Listening.'
    return p.loopback
      ? `Listening on ${p.bindAddr} only, which nothing outside the container can reach.`
      : `Listening on ${p.bindAddr}.`
  }
  if (p.observedState === 'gone') return 'Not listening now.'
  return null
}

// Probes: a read for this screen, by port id.
const probes = ref<Record<string, { busy: boolean; result: ProbeResult | null; error: string | null }>>({})
async function probe(p: Port): Promise<void> {
  probes.value = { ...probes.value, [p.id]: { busy: true, result: null, error: null } }
  try {
    const result = await ports.probe(wsId.value, p.id)
    probes.value = { ...probes.value, [p.id]: { busy: false, result, error: null } }
  } catch (e) {
    probes.value = { ...probes.value, [p.id]: { busy: false, result: null, error: describeError(e) } }
  }
}

// The remove confirm: removing retires the address for good.
const removing = ref<string | null>(null)

// The add form.
const addPort = ref('')
const addLabel = ref('')
const addNumber = computed(() => {
  const s = addPort.value.trim()
  if (!/^\d{1,5}$/.test(s)) return null
  const n = Number(s)
  return n >= 1 && n <= 65535 ? n : null
})
const addTaken = computed(() => addNumber.value !== null && all.value.some((p) => p.containerPort === addNumber.value))
async function add(): Promise<void> {
  const n = addNumber.value
  if (n === null || addTaken.value) return
  await ports.add(wsId.value, n, addLabel.value)
  addPort.value = ''
  addLabel.value = ''
}
</script>

<template>
  <div class="block" data-test="ports">
    <div class="sec-label"><span>Ports</span><span v-if="all.length > 0">{{ all.length }}</span></div>
    <p class="sub">
      A port is previewed only once you turn it on, never because something started listening on it. Each
      preview opens on its own address, behind your Drydock sign-in; Drydock asks for its password only here, so
      a sign-in form on a preview is not Drydock's.
    </p>
    <p v-if="!previewsOn" class="sub" data-test="previews-off">
      No preview domain is set up on this Drydock, so ports are listed but cannot be previewed. The installer's
      <code>--preview-domain</code> sets one up.
    </p>
    <p v-else-if="!running && all.some((p) => p.enabled)" class="sub" data-test="ports-not-running">
      The workspace is not running, so its previews will not answer until it is started.
    </p>

    <ul v-if="rows.length > 0" class="ports">
      <li
        v-for="p in rows" :key="p.id" class="port" :class="{ on: p.enabled, hidden: p.hidden }"
        data-test="port" :data-port="p.containerPort"
      >
        <div class="head">
          <span class="num">{{ p.containerPort }}</span>
          <span v-if="p.label" class="label" data-test="port-label">{{ p.label }}</span>
          <span v-for="b in provenance(p)" :key="b" class="badge" data-test="port-badge">{{ b }}</span>
          <span v-if="p.hidden" class="badge">hidden</span>
        </div>
        <p v-if="observation(p)" class="sub" :class="{ loop: p.loopback && p.observedState === 'listening' }" data-test="port-observed">
          {{ observation(p) }}
        </p>

        <template v-if="p.enabled && p.url">
          <a
            class="host" :href="p.url" target="_blank" rel="noopener noreferrer" data-test="port-open"
          >{{ p.host }}</a>
          <ActionButton
            label="Turn off preview" :flight-key="portEnableKey(p.id)"
            :run="() => ports.setEnabled(wsId, p.id, false)" data-test="port-disable"
          />
        </template>
        <template v-else-if="previewsOn">
          <p class="sub" data-test="port-off">Not previewed.</p>
          <ActionButton
            label="Preview this port" primary :flight-key="portEnableKey(p.id)"
            :run="() => ports.setEnabled(wsId, p.id, true)" data-test="port-enable"
          />
        </template>

        <details class="more">
          <summary>More</summary>
          <div class="opts">
            <p class="sub" data-test="port-host-mode">
              <template v-if="p.hostHeader === 'localhost'">
                The dev server is sent <code>Host: localhost:{{ p.containerPort }}</code>, which its host check
                accepts unedited.
              </template>
              <template v-else>
                The dev server is sent the preview's own host name, which its host check has to allow.
              </template>
            </p>
            <ActionButton
              :label="p.hostHeader === 'localhost' ? 'Send the preview host instead' : 'Send localhost again'"
              :flight-key="portHostKey(p.id)"
              :run="() => ports.setHostHeader(wsId, p.id, p.hostHeader === 'localhost' ? 'passthrough' : 'localhost')"
              data-test="port-host"
            />
            <div class="probe">
              <button
                type="button" class="btn" :disabled="probes[p.id]?.busy" data-test="port-probe"
                @click="probe(p)"
              >Check the port</button>
              <p v-if="probes[p.id]?.result" class="sub" :class="probes[p.id]!.result!.outcome" data-test="port-probe-result">
                {{ probes[p.id]!.result!.message }}
              </p>
              <p v-else-if="probes[p.id]?.error" class="sub bad" data-test="port-probe-error">{{ probes[p.id]!.error }}</p>
            </div>
            <ActionButton
              :label="p.hidden ? 'Show in the list' : 'Hide from the list'" :flight-key="portHideKey(p.id)"
              :run="() => ports.setHidden(wsId, p.id, !p.hidden)" data-test="port-hide"
            />
            <button
              v-if="removing !== p.id" type="button" class="btn ghost" data-test="port-remove"
              @click="removing = p.id"
            >Remove…</button>
            <div v-else class="confirm" data-test="port-remove-confirm">
              <p class="sub">
                Removing retires this port's preview address for good: listed again, it gets a new one, and a
                bookmark of this one stops working.
              </p>
              <div class="act">
                <button type="button" class="btn" @click="removing = null">Cancel</button>
                <ActionButton
                  :label="`Remove port ${p.containerPort}`" danger :flight-key="portRetireKey(p.id)"
                  :run="() => ports.retire(wsId, p.id)" data-test="port-retire"
                />
              </div>
            </div>
          </div>
        </details>
      </li>
    </ul>
    <div v-else-if="loaded" class="empty" data-test="ports-empty">
      <span>No ports listed.</span>
      <span class="sub">
        Ports the dev container configuration declares, and ports something in the container is listening on,
        appear here, switched off. Add one by number below.
      </span>
    </div>
    <div v-else-if="load?.status === 'error'" class="msg bad" role="alert" data-test="ports-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ describeError(load.error) }}</span>
    </div>
    <div v-else class="empty" data-test="ports-loading"><span>Loading…</span></div>

    <div v-if="running" class="rescan">
      <ActionButton
        label="Look for listening ports now" :flight-key="portRescanKey(wsId)"
        :run="() => ports.rescan(wsId)" data-test="ports-rescan"
      />
    </div>

    <label v-if="hiddenCount > 0" class="toggle" data-test="ports-show-hidden">
      <input v-model="showHidden" type="checkbox"> Show {{ hiddenCount }} hidden
    </label>

    <div class="add" data-test="port-add-form">
      <div class="field">
        <label for="port-add-number">Port</label>
        <input
          id="port-add-number" v-model="addPort" type="text" inputmode="numeric" autocomplete="off"
          placeholder="5173" data-test="port-add-number"
        >
      </div>
      <div class="field">
        <label for="port-add-label">Label (optional)</label>
        <input id="port-add-label" v-model="addLabel" type="text" autocomplete="off" maxlength="100" data-test="port-add-label">
      </div>
      <p v-if="addPort.trim() !== '' && addNumber === null" class="sub bad" data-test="port-add-invalid">
        A port is a number from 1 to 65535.
      </p>
      <p v-else-if="addTaken" class="sub" data-test="port-add-taken">That port is already listed.</p>
      <ActionButton
        label="Add port" :blocked="addNumber === null || addTaken"
        :flight-key="portAddKey(wsId, addNumber ?? 0)" :run="add" data-test="port-add"
      />
    </div>
  </div>
</template>

<style scoped>
.block { display: flex; flex-direction: column; gap: 8px; }
.sub { font-size: 13px; color: var(--ink-2); }
.sub.bad, .sub.refused, .sub.timed_out, .sub.lookup_failed { color: var(--bad); }
.sub.answering { color: var(--ok); }
.ports {
  list-style: none; margin: 0; padding: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.port { padding: 10px 12px; border-bottom: 1px solid var(--line-soft); display: flex; flex-direction: column; gap: 6px; }
.port:last-child { border-bottom: 0; }
.port.hidden { opacity: .7; }
.head { display: flex; flex-wrap: wrap; align-items: baseline; gap: 6px 8px; }
.num { font-family: var(--mono); font-weight: 700; font-size: 14px; }
.label { font-size: 13.5px; color: var(--ink-2); overflow-wrap: anywhere; }
.badge { font-size: 11px; color: var(--ink-3); border: 1px solid var(--line); border-radius: 999px; padding: 0 6px; }
.host { font-family: var(--mono); font-size: 13px; overflow-wrap: anywhere; }
.more summary { font-size: 12.5px; color: var(--ink-3); cursor: pointer; }
.opts { display: flex; flex-direction: column; gap: 8px; padding-top: 6px; }
.probe { display: flex; flex-direction: column; gap: 4px; align-items: flex-start; }
.confirm { display: flex; flex-direction: column; gap: 6px; }
.act { display: flex; gap: 8px; align-items: flex-start; }
.rescan { display: flex; }
.toggle { font-size: 13px; color: var(--ink-2); display: flex; gap: 6px; align-items: center; }
.add { display: flex; flex-direction: column; gap: 6px; }
.field { display: flex; flex-direction: column; gap: 3px; font-size: 13px; }
.field input { max-width: 16rem; }
</style>