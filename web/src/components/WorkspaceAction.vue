<script setup lang="ts">
// A workspace's one card action (frontend §6.1), as a button: which label,
// which in-flight key, which request. lib/workspaceCard.ts decides *which*
// action; this only renders it, so the running card, the catalog row and the
// detail view cannot disagree about what Stop sends or what settles it.
//
// `delete` here is only the resume of a delete that stuck. It sends the full
// name the operator typed when the delete began — the workspace is already
// `deleting`, which is the record of that confirm (internal/provision
// ResumeDelete) — so asking again is one tap, as the server's own sentence
// ("Delete again to retry.") promises. The first delete is the detail view's
// sheet, and never this.
import ActionButton from './ActionButton.vue'
import type { CardAction } from '../lib/workspaceCard'
import type { Workspace } from '../stores/reducer'
import { deleteKey, rebuildKey, startKey, stopKey, useWorkspacesStore } from '../stores/workspaces'

const props = defineProps<{ workspace: Workspace; action: CardAction; primary?: boolean }>()
const workspaces = useWorkspacesStore()
const id = () => props.workspace.id
</script>

<template>
  <ActionButton
    v-if="action === 'start'" label="Start" :primary="primary"
    :flight-key="startKey(workspace.id)" :run="() => workspaces.start(id())" data-test="start"
  />
  <ActionButton
    v-else-if="action === 'stop'" label="Stop" :primary="primary"
    :flight-key="stopKey(workspace.id)" :run="() => workspaces.stop(id())" data-test="stop"
  />
  <ActionButton
    v-else-if="action === 'rebuild'" label="Rebuild" :primary="primary"
    :flight-key="rebuildKey(workspace.id)" :run="() => workspaces.rebuild(id())" data-test="rebuild"
  />
  <ActionButton
    v-else-if="action === 'delete' && workspace.fullName !== null" label="Delete again" danger
    :flight-key="deleteKey(workspace.id)" :run="() => workspaces.remove(id(), workspace.fullName!)"
    data-test="delete-again"
  />
</template>
