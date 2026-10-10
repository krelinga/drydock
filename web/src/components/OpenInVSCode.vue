<script setup lang="ts">
// *Open in VS Code* (design §6, "Opening a workspace in VS Code"): a plain
// link the operator's VS Code opens, attached to the workspace's container
// over Remote-SSH. The URL is the server's, read from Docker as the view was
// served, validated by the reducer (vscodeURL) and hidden when it is older
// than the state it would open (vscodeLink).
//
// When the server has no --vscode-ssh-host there is no button, disabled or
// otherwise: with `note`, the one place that says so (the workspace page of a
// running workspace) says how to turn it on instead. A card never does — one
// fleet-wide setting gets one message, not one per card.
import { computed } from 'vue'
import { vscodeConfigured, vscodeLink, type Workspace } from '../stores/reducer'

const props = defineProps<{ workspace: Workspace; note?: boolean }>()
const url = computed(() => vscodeLink(props.workspace))
const off = computed(() =>
  props.note === true && props.workspace.state === 'running' && vscodeConfigured(props.workspace) === false)
</script>

<template>
  <a v-if="url !== null" :href="url" class="vscode" data-test="open-vscode">Open in VS Code</a>
  <p v-else-if="off" class="vscode-off" data-test="vscode-off">
    Open in VS Code is off: start Drydock with <code>--vscode-ssh-host USER@HOST</code>
    (the installer's <code>--vscode-ssh-host</code>). The SSH user needs Docker access.
  </p>
</template>

<style scoped>
.vscode {
  align-self: flex-start; font-size: 13px; color: var(--ink);
  text-decoration: underline; text-underline-offset: 3px;
}
.vscode-off { font-size: 12.5px; color: var(--ink-3); }
</style>
