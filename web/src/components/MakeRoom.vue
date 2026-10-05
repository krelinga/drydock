<script setup lang="ts">
// What stands where Clone, Start or a Rebuild from stopped or failed would be
// when Drydock is at its cap (design §1, frontend §9). Not the button
// disabled: an action that cannot work is replaced by the one that can
// (§6.6), and the one that can is Stop, on the cards under Running — which
// sort the stoppable ones first. The numbers are the cap from GET
// /api/workspaces and the count the stream keeps (lib/capacity.ts).
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { capacity } from '../lib/capacity'
import { useStreamStore } from '../stores/stream'

const stream = useStreamStore()
const cap = computed(() => capacity(stream.entities))
</script>

<template>
  <p class="room" role="status" data-test="make-room">
    At the cap: {{ cap.occupied }} of {{ cap.cap }} workspaces are building or running.
    <RouterLink :to="{ name: 'workspaces', hash: '#running' }">Stop one under Running</RouterLink>
    to make room.
  </p>
</template>

<style scoped>
.room { font-size: 12.5px; color: var(--ink-2); }
</style>
