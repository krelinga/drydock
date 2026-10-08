<script setup lang="ts">
// Design §12, *Disk full*, as frontend §9 words it: "Banner with per-workspace
// disk, list sortable by it". The fault is the host's, not any one card's —
// every create, start and rebuild is refused while it stands — so it is one
// banner, on every screen, with the one action that fixes it: delete a
// workspace. It lists the largest first, which is the sort that matters, each
// linking to its detail view, where Delete is.
//
// Rendered from the reducer's `hostDisk` and `resources` alone, which the
// stream's `resources` frames and the workspace list write; nothing here
// fetches or measures.
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { formatBytes, hostPercent } from '../lib/resources'
import { useStreamStore } from '../stores/stream'

const stream = useStreamStore()
const host = computed(() => stream.entities.hostDisk)

/** The three largest workspaces by disk, measured ones only. */
const largest = computed(() => {
  const e = stream.entities
  return Object.entries(e.resources)
    .flatMap(([id, r]) => {
      const w = e.workspaces[id]
      if (w === undefined || r.disk === null || w.state === 'deleting') return []
      return [{ id, name: w.fullName ?? id, bytes: r.disk.bytes, partial: r.disk.partial }]
    })
    .sort((a, b) => b.bytes - a.bytes)
    .slice(0, 3)
})
</script>

<template>
  <div v-if="host?.over" class="msg bad banner" role="alert" data-test="disk-banner">
    <span class="glyph" aria-hidden="true">!</span>
    <div class="body">
      <strong>The workspace disk is {{ hostPercent(host) }}% full.</strong>
      Drydock refuses to clone, start or rebuild a workspace at {{ host.limitPercent }}%, so nothing new can start.
      Delete a workspace you no longer need; pushed work is safe on GitHub.
      <ul v-if="largest.length > 0" class="largest" data-test="disk-largest">
        <li v-for="w in largest" :key="w.id">
          <RouterLink :to="{ name: 'workspace', params: { id: w.id } }" class="mono">{{ w.name }}</RouterLink>
          {{ w.partial ? '≥ ' : '' }}{{ formatBytes(w.bytes) }}
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.banner { align-items: flex-start; }
.body { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.largest { margin: 2px 0 0; padding-left: 18px; font-size: 12.5px; }
.mono { font-family: var(--mono); overflow-wrap: anywhere; }
</style>
