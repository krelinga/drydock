<script setup lang="ts">
// A workspace's "mem 1.2 GB · disk 3.4 GB" (frontend §6.1), read from the
// entities the reducer writes from the stream's `resources` frames and the
// views. Unknown reads "—" and stale says so; lib/resources.ts has the rules.
import { computed } from 'vue'
import type { Workspace } from '../stores/reducer'
import { useStreamStore } from '../stores/stream'
import { resourceLine } from '../lib/resources'

const props = defineProps<{ workspace: Workspace }>()
const stream = useStreamStore()
const line = computed(() => resourceLine(props.workspace.state, stream.entities.resources[props.workspace.id]))
</script>

<template>
  <p v-if="line" class="resources" :title="line.label" data-test="resources">
    <span aria-hidden="true">{{ line.text }}</span>
    <span class="vis-sr">{{ line.label }}</span>
  </p>
</template>

<style scoped>
.resources { font-family: var(--mono); font-size: 11px; color: var(--ink-3); }
</style>
