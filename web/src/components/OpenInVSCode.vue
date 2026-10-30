<script setup lang="ts">
// *Open in VS Code* (design §6, "Opening a workspace in VS Code"): a link,
// styled as *Open in Claude* is (a secondary `.btn`), that the operator's VS
// Code opens, attached to the workspace's container over Remote-SSH. The URL is the server's, read from Docker as the view was
// served, validated by the reducer (vscodeURL) and hidden when it is older
// than the state it would open (vscodeLink).
//
// The href is that URL with `?windowId=_blank`, which VS Code reads as "open
// a new window" (src/vs/code/electron-main/app.ts: handleProtocolUrl when VS
// Code is running; a link that launches it opens as a window of its own,
// through resolveInitialProtocolUrls). getWindowOpenableFromProtocolUrl
// deletes the parameter on both paths, so the vscode-remote:// folder URI
// carries no query. Without it the workspace replaced whichever window had
// focus last; a folder already open is still focused rather than opened twice.
// The query is added here, not by the server: the view's url is also the VS
// Code extension's, which opens the folder in a new window itself and, in
// releases already shipped, refuses any URL with a query (vscodeURL), so a
// server that sent one would break Connect for an older extension
// (internal/vscode, "A new window, and why the link has no query").
//
// When the server has no --vscode-ssh-host there is no button, disabled or
// otherwise: with `note`, the one place that says so (the workspace page of a
// running workspace) says how to turn it on instead. A card never does — one
// fleet-wide setting gets one message, not one per card.
import { computed } from 'vue'
import { vscodeConfigured, vscodeLink, type Workspace } from '../core/reducer'

const props = defineProps<{ workspace: Workspace; note?: boolean }>()
/** VS Code's own "open in a new window" parameter (see above). */
const NEW_WINDOW = '?windowId=_blank'
const url = computed(() => {
  const u = vscodeLink(props.workspace)
  return u === null ? null : u + NEW_WINDOW
})
const off = computed(() =>
  props.note === true && props.workspace.state === 'running' && vscodeConfigured(props.workspace) === false)
</script>

<template>
  <a v-if="url !== null" :href="url" class="btn vscode" data-test="open-vscode">Open in VS Code</a>
  <p v-else-if="off" class="vscode-off" data-test="vscode-off">
    Open in VS Code is off: start Drydock with <code>--vscode-ssh-host USER@HOST</code>
    (the installer's <code>--vscode-ssh-host</code>). The SSH user needs Docker access.
  </p>
</template>

<style scoped>
.vscode {
  align-self: flex-start; text-decoration: none;
}
.vscode-off { font-size: 12.5px; color: var(--ink-3); }
</style>
