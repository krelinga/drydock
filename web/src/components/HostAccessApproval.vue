<script setup lang="ts">
// The host-access request a stopped workspace waits on (design §6, "What a
// configuration may ask of the host"), with what approving it means and the
// two answers. It stands where the card's one action would be, on the card,
// the catalog row and the detail view alike (WorkspaceAction), so the
// request is read where it is approved: never a bare Approve button.
//
// Everything shown is the entity's — the request the server sent on the
// workspace.state event or the view — and nothing here is kept: Approve and
// continue sends the request's own hash, and the server refuses it
// (`approval_stale`) if the configuration changed since. Both buttons are
// ActionButton, settled by the stream (stores/workspaces settlesApprove,
// settlesDecline), never by the 202.
//
// Values are the settings' JSON, rendered as text: the container wrote
// them, so they are shown exactly and never interpreted.
import { computed } from 'vue'
import type { HostSettingChange, HostSettingView } from '../api/types'
import type { Workspace } from '../stores/reducer'
import { approveKey, declineKey, useWorkspacesStore } from '../stores/workspaces'
import ActionButton from './ActionButton.vue'

const props = defineProps<{ workspace: Workspace }>()
const workspaces = useWorkspacesStore()
const req = computed(() => props.workspace.approval)

const SOURCE: Record<string, string> = {
  repository: "the repository's devcontainer.json",
  feature_or_image: 'a Feature or the image',
}
const source = (s: string) => SOURCE[s] ?? s
const json = (v: unknown) => JSON.stringify(v)
const privileged = computed(() => {
  const r = req.value
  if (r === null) return false
  return [...r.added, ...r.changed].some((s: HostSettingView | HostSettingChange) => s.field === 'privileged')
})
</script>

<template>
  <section v-if="req" class="approval" aria-label="Host access approval" data-test="approval">
    <p class="title"><strong>Needs approval:</strong> this configuration asks for host access.</p>
    <div class="msg warn" role="note" data-test="approval-warning">
      <span class="glyph" aria-hidden="true">!</span>
      <span>
        Approving lets this repository's dev container reach this server, as the drydock user or as root.
        Anything running in the container — an agent included — can then use it, without changing the configuration again.
        <strong v-if="privileged" data-test="approval-privileged">
          privileged is root on the host: Drydock's keys and every other workspace are in reach.
        </strong>
        Approve only what you trust for this repository. It is recorded, and asked again if it changes.
      </span>
    </div>

    <template v-if="req.added.length > 0">
      <p class="group">New</p>
      <ul class="settings" data-test="approval-added">
        <li v-for="s in req.added" :key="s.field + s.source" data-test="approval-setting">
          <code class="field">{{ s.field }}</code> <span class="src">from {{ source(s.source) }}</span>
          <pre class="value">{{ json(s.value) }}</pre>
        </li>
      </ul>
    </template>
    <template v-if="req.changed.length > 0">
      <p class="group">Changed since the last approval</p>
      <ul class="settings" data-test="approval-changed">
        <li v-for="s in req.changed" :key="s.field + s.source" data-test="approval-setting">
          <code class="field">{{ s.field }}</code> <span class="src">from {{ source(s.source) }}</span>
          <pre class="value">was {{ json(s.from) }}</pre>
          <pre class="value">now {{ json(s.to) }}</pre>
        </li>
      </ul>
    </template>
    <template v-if="req.removed.length > 0">
      <p class="group">No longer asked for</p>
      <ul class="settings" data-test="approval-removed">
        <li v-for="s in req.removed" :key="s.field + s.source">
          <code class="field">{{ s.field }}</code> <span class="src">from {{ source(s.source) }}</span>
        </li>
      </ul>
    </template>

    <div class="buttons">
      <ActionButton
        label="Approve and continue" danger :flight-key="approveKey(workspace.id)"
        :run="() => workspaces.approve(workspace.id, req!.hash)" data-test="approve"
      />
      <ActionButton
        label="Cancel" :flight-key="declineKey(workspace.id)"
        :run="() => workspaces.decline(workspace.id)" data-test="decline"
      />
    </div>
  </section>
</template>

<style scoped>
.approval { display: flex; flex-direction: column; gap: 8px; }
.title { font-size: 13.5px; }
.group { font-size: 12px; color: var(--ink-3); text-transform: uppercase; letter-spacing: .04em; }
.settings { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 6px; }
.field { font-family: var(--mono); font-weight: 700; }
.src { font-size: 12px; color: var(--ink-3); }
.value {
  font-family: var(--mono); font-size: 12px; margin: 2px 0 0; padding: 4px 6px;
  background: var(--bg-2, transparent); border-radius: var(--r);
  white-space: pre-wrap; overflow-wrap: anywhere;
}
.buttons { display: flex; gap: 8px; flex-wrap: wrap; }
</style>
