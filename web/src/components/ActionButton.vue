<script setup lang="ts">
// One mutating button, §4.2's lifecycle around it (frontend §4.2, §9).
//
// The button reads the stream store's in-flight set and nothing else: it
// disables and spins while its key is in flight, says "no response yet" once
// the stream store marks the key slow, and never turns that into a failure —
// the server accepted the request, so the one thing certainly true is that it
// is running. What the action *did* is not this component's business; it
// arrives as events and renders wherever the entity renders.
//
// A refusal is the only outcome shown here: by code, never by the server's
// prose (api/messages.ts). `in_progress` is a note, not an error — the state
// machine beat you to it (§9).
import { computed, ref } from 'vue'
import { ApiError } from '../api/client'
import { describeError } from '../api/messages'
import { useSessionStore } from '../stores/session'
import { useStreamStore } from '../stores/stream'

const props = defineProps<{
  label: string
  flightKey: string
  run: () => Promise<void>
  primary?: boolean
}>()

const stream = useStreamStore()
const session = useSessionStore()
const flight = computed(() => stream.inFlight[props.flightKey] ?? null)
// Local, and legitimately so (§4.2): the outcome of this device's request.
const refusal = ref<{ text: string; note: boolean } | null>(null)

async function click(): Promise<void> {
  if (flight.value !== null) return
  refusal.value = null
  try {
    await props.run()
  } catch (e) {
    // A 401 has gone down the sign-out path; there is no screen to tell.
    if (session.status !== 'signed-in') return
    const note = e instanceof ApiError && e.code === 'in_progress'
    refusal.value = { text: describeError(e), note }
  }
}
</script>

<template>
  <div class="action">
    <button
      type="button" class="btn" :class="{ primary }" data-test="action"
      :disabled="flight !== null" :aria-busy="flight !== null" @click="click"
    >
      <span v-if="flight" class="spinner" aria-hidden="true" />
      {{ label }}
    </button>
    <p v-if="flight?.slow" class="note" aria-live="polite" data-test="action-slow">
      No response yet. It was accepted and is still running.
    </p>
    <p v-if="refusal?.note" class="note" role="status" data-test="action-note">{{ refusal.text }}</p>
    <div v-else-if="refusal" class="msg bad" role="alert" data-test="action-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ refusal.text }}</span>
    </div>
  </div>
</template>

<style scoped>
.action { display: flex; flex-direction: column; gap: 6px; align-items: flex-start; }
.note { font-size: 12.5px; color: var(--ink-3); }
.spinner {
  width: 12px; height: 12px; border-radius: 50%;
  border: 2px solid var(--line); border-top-color: var(--ink-2);
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
