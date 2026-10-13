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
// What it says is core's (core/lib/approval.ts), which the VS Code
// extension's review document renders too, so the two cannot drift. Values
// are the settings' JSON, rendered as text: the container wrote them, so they
// are shown exactly and never interpreted, with control and format
// characters escaped (core/lib/text.ts) so none can reorder what is read.
import { computed } from 'vue'
import type { Workspace } from '../core/reducer'
import {
  APPROVAL_CLOSING, APPROVAL_INTRO, APPROVAL_TITLE, INITIALIZE_COMMAND_SENTENCE, PRIVILEGED_SENTENCE, approvalSections,
} from '../core/lib/approval'
import { approveKey, declineKey, useWorkspacesStore } from '../stores/workspaces'
import ActionButton from './ActionButton.vue'

const props = defineProps<{ workspace: Workspace }>()
const workspaces = useWorkspacesStore()
const req = computed(() => props.workspace.approval)
const sections = computed(() => (req.value === null ? null : approvalSections(req.value)))
const GROUP_TEST: Record<string, string> = { added: 'approval-added', changed: 'approval-changed', removed: 'approval-removed' }
</script>

<template>
  <section v-if="req" class="approval" aria-label="Host access approval" data-test="approval">
    <p class="title"><strong>{{ APPROVAL_TITLE.lead }}</strong> {{ APPROVAL_TITLE.rest }}</p>
    <div class="msg warn" role="note" data-test="approval-warning">
      <span class="glyph" aria-hidden="true">!</span>
      <span>
        {{ APPROVAL_INTRO }}
        <strong v-if="sections?.privileged" data-test="approval-privileged">{{ PRIVILEGED_SENTENCE }}</strong>
        <strong v-if="sections?.initializeCommand" data-test="approval-initialize-command">{{ INITIALIZE_COMMAND_SENTENCE }}</strong>
        {{ APPROVAL_CLOSING }}
      </span>
    </div>

    <template v-for="g in sections?.groups ?? []" :key="g.kind">
      <p class="group">{{ g.label }}</p>
      <ul class="settings" :data-test="GROUP_TEST[g.kind]">
        <li v-for="s in g.items" :key="s.key" :data-test="g.kind === 'removed' ? undefined : 'approval-setting'">
          <code class="field">{{ s.field }}</code> <span class="src">{{ s.source }}</span>
          <pre v-for="(v, i) in s.values" :key="i" class="value">{{ v }}</pre>
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
