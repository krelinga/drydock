<script setup lang="ts">
// A workspace's "3 listening · 1 previewed" (port forwarding §8.2): passive
// text on the card, from the reducer's port entities. The count is ambient —
// nothing here asks for a click, and nothing changes but the number when a
// port appears (lib/portCount.ts). A running workspace's ports are loaded
// here when nothing has loaded them, so the home view's cards can count; the
// stream keeps them current, and a reopened stream refetches them.
import { computed, onMounted, watch } from 'vue'
import { portCount, portCountLabel, portCountText } from '../lib/portCount'
import { useStreamRefetch } from '../lib/refetch'
import { usePortsStore } from '../stores/ports'
import { portsOf, type Workspace } from '../stores/reducer'
import { useStreamStore } from '../stores/stream'

// panel: the page's ports panel loads them, so the count only reads them.
const props = defineProps<{ workspace: Workspace; panel?: boolean }>()
const stream = useStreamStore()
const ports = usePortsStore()

const running = computed(() => props.workspace.state === 'running')
function loadIfNeeded(): void {
  const id = props.workspace.id
  if (props.panel || !running.value || stream.entities.portsLoaded[id] !== undefined || ports.status[id]?.status === 'loading') return
  void ports.load(id)
}
onMounted(loadIfNeeded)
watch(() => [props.workspace.id, props.workspace.state], loadIfNeeded)
useStreamRefetch({ refetch: () => (running.value && !props.panel ? ports.load(props.workspace.id) : Promise.resolve()) })

const count = computed(() => portCount(portsOf(stream.entities, props.workspace.id)))
const text = computed(() => (running.value ? portCountText(count.value) : null))
</script>

<template>
  <p v-if="text" class="ports" :title="portCountLabel(count)" data-test="port-count">
    <span aria-hidden="true">{{ text }}</span>
    <span class="vis-sr">{{ portCountLabel(count) }}</span>
  </p>
</template>

<style scoped>
.ports { font-family: var(--mono); font-size: 11px; color: var(--ink-3); }
</style>
