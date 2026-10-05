<script setup lang="ts">
// What the shared Claude login does to one workspace's card (frontend §6.6):
// a warning dot while it is expiring, "Waiting on Claude sign-in." once a
// session cannot run. Never a button — the fleet banner holds the one Sign in
// to Claude there is, and ten cards with ten buttons is the failure §6.6 is
// about. lib/identity.ts decides; this only renders.
import { computed } from 'vue'
import type { WorkspaceState } from '../api/types'
import { cardOverlay } from '../lib/identity'
import { useStreamStore } from '../stores/stream'

// `part` places the two halves: the dot beside the status line, the sentence
// under it, where the session area will be.
const props = defineProps<{ state: WorkspaceState | null; part: 'dot' | 'waiting' }>()
const stream = useStreamStore()
const overlay = computed(() => {
  const o = cardOverlay(stream.entities.identity, props.state)
  return o !== null && o.kind === props.part ? o : null
})
</script>

<template>
  <span
    v-if="overlay?.kind === 'dot'" class="dot" role="img" :aria-label="overlay.text" :title="overlay.text"
    data-test="identity-dot"
  />
  <p v-else-if="overlay?.kind === 'waiting'" class="waiting" data-test="identity-waiting">{{ overlay.text }}</p>
</template>

<style scoped>
.dot {
  display: inline-block; width: 8px; height: 8px; border-radius: 50%;
  background: var(--warn); flex: none; align-self: center;
}
.waiting { font-size: 12.5px; color: var(--ink-3); }
</style>
