<script setup lang="ts">
// The fleet-wide banner slot (frontend §6.6). One fault with one cause gets
// one message here, above every card, rather than one on each. Phase 5's
// identity store supplies the five Claude-login states.
//
// Phase 4's one fleet-wide fault: `secret.undeliverable`. When the broker
// finds a stored secret it cannot deliver it fails *every* workspace's fetch
// rather than hand out half an environment (design §10.3), so every
// workspace's commands stop at once and none of them is at fault — §6.6's
// shape exactly. It is an error the operator must see wherever they are, so
// it is here rather than only on /secrets.
//
// Nothing on the stream says it was fixed: there is no "delivered again"
// event. So after a later change to secrets the banner says that the report
// predates it, rather than either vanishing (a guess that the change fixed
// it) or staying as though nothing happened. A reload drops it: GET
// /api/secrets does not carry it (an API gap, frontend §4.5 #12).
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { relativeTime } from '../lib/time'
import { useStreamStore } from '../stores/stream'

interface Banner {
  tone: 'warn' | 'bad'
  title: string
  body: string
  /** The server's sentence, shown as text after ours, never parsed. */
  detail?: string
  link?: { to: string; label: string }
}

const stream = useStreamStore()

const banner = computed<Banner | null>(() => {
  const fault = stream.entities.secretFault
  if (fault === null) return null
  const superseded = stream.entities.secretsWrittenAt > fault.eventId
  return {
    tone: superseded ? 'warn' : 'bad',
    title: 'Stored secrets cannot be delivered.',
    body: superseded
      ? `Reported ${relativeTime(fault.at)}, before the latest change to secrets. If it is still broken, the next command any workspace runs reports it again.`
      : `Every workspace's commands fail until this is fixed. Reported ${relativeTime(fault.at)}.`,
    detail: fault.message,
    link: { to: '/secrets', label: 'Secrets' },
  }
})
</script>

<template>
  <div v-if="banner" class="msg" :class="banner.tone" role="alert" data-test="fleet-banner">
    <span class="glyph" aria-hidden="true">!</span>
    <div>
      <b>{{ banner.title }}</b>
      <p>{{ banner.body }}</p>
      <p v-if="banner.detail" class="detail" data-test="fleet-detail">{{ banner.detail }}</p>
      <RouterLink v-if="banner.link" :to="banner.link.to">{{ banner.link.label }}</RouterLink>
    </div>
  </div>
</template>

<style scoped>
.detail { font-family: var(--mono); font-size: 11.5px; overflow-wrap: anywhere; margin-top: 4px; }
</style>
