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
//
// At the cap, an action that would take a container slot — Start, or a
// Rebuild of a workspace that is not running — is replaced by MakeRoom
// (`withRoom`): the server would refuse it, and the way forward is a Stop.
import { computed } from 'vue'
import ActionButton from './ActionButton.vue'
import MakeRoom from './MakeRoom.vue'
import { capacity } from '../lib/capacity'
import { withRoom, type CardAction } from '../lib/workspaceCard'
import type { Workspace } from '../stores/reducer'
import { useStreamStore } from '../stores/stream'
import { deleteKey, rebuildKey, startKey, stopKey, useWorkspacesStore } from '../stores/workspaces'

const props = defineProps<{ workspace: Workspace; action: CardAction; primary?: boolean }>()
const workspaces = useWorkspacesStore()
const stream = useStreamStore()
const id = () => props.workspace.id
const shown = computed(() => withRoom(props.action, props.workspace, capacity(stream.entities).full))
</script>

<template>
  <MakeRoom v-if="shown === 'make_room'" />
  <ActionButton
    v-else-if="shown === 'start'" label="Start" :primary="primary"
    :flight-key="startKey(workspace.id)" :run="() => workspaces.start(id())" data-test="start"
  />
  <ActionButton
    v-else-if="shown === 'stop'" label="Stop" :primary="primary"
    :flight-key="stopKey(workspace.id)" :run="() => workspaces.stop(id())" data-test="stop"
  />
  <ActionButton
    v-else-if="shown === 'rebuild'" label="Rebuild" :primary="primary"
    :flight-key="rebuildKey(workspace.id)" :run="() => workspaces.rebuild(id())" data-test="rebuild"
  />
  <ActionButton
    v-else-if="shown === 'delete' && workspace.fullName !== null" label="Delete again" danger
    :flight-key="deleteKey(workspace.id)" :run="() => workspaces.remove(id(), workspace.fullName!)"
    data-test="delete-again"
  />
</template>
