<script setup lang="ts">
// Design §12, *Repo removed from the installation*, on the workspace itself:
// the card and the detail view, not only the catalog row (frontend §9). The
// workspace keeps its container and its clone — the working tree may hold
// unpushed work — but GitHub access is gone, so it is badged read-only with
// the one fix: the installation's settings, to add the repository back.
//
// Joined from the catalog the reducer holds (repo.removed), never inferred.
import { computed } from 'vue'
import type { Workspace } from '../stores/reducer'
import { useStreamStore } from '../stores/stream'

const props = defineProps<{ workspace: Workspace }>()
const stream = useStreamStore()
const repo = computed(() => {
  const id = props.workspace.repositoryId
  return id !== null ? stream.entities.repos[id] ?? null : null
})
const settings = computed(() => {
  const r = repo.value
  return r !== null ? stream.entities.installations[r.installationId]?.settings_url ?? null : null
})
</script>

<template>
  <p v-if="repo?.removed" class="read-only" data-test="read-only">
    <span class="badge warn">read-only</span>
    Removed from the GitHub App, so git and gh have no access. Unpushed work in the working tree survives.
    <a v-if="settings" :href="settings" target="_blank" rel="noopener noreferrer">Installation settings</a>
  </p>
</template>

<style scoped>
.read-only { font-size: 12.5px; color: var(--ink-2); }
.badge {
  font-family: var(--mono); font-size: 10px; letter-spacing: .05em; text-transform: uppercase;
  padding: 2px 6px; border-radius: 3px; background: var(--warn-bg); color: var(--warn); margin-right: 4px;
}
</style>
