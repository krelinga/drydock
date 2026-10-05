<script setup lang="ts">
// The fleet-wide banner slot (frontend §6.6). One fault with one cause gets
// one message here, above every card, rather than one on each. Phase 5's
// identity store supplies the five Claude-login states.
//
// Phase 4's one fleet-wide fault: stored secrets that cannot be delivered.
// When the broker finds a stored secret it cannot deliver it fails *every*
// workspace's fetch rather than hand out half an environment (design §10.3),
// so every workspace's commands stop at once and none of them is at fault —
// §6.6's shape exactly. It is an error the operator must see wherever they
// are, so it is here rather than only on /secrets.
//
// The fault is entity state like any other (frontend §4.5 #12): GET
// /api/secrets carries it, so a reload shows a standing fault, and
// `secret.deliverable` clears it, so the banner goes when the fault does and
// not before. This component loads the list for that reason — on every
// screen, since the banner is on every screen — and refetches it when the
// stream reopens. It renders only what the reducer holds.
import { computed, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useStreamRefetch } from '../lib/refetch'
import { relativeTime } from '../lib/time'
import { useSecretsStore } from '../stores/secrets'
import { useSessionStore } from '../stores/session'
import { useStreamStore } from '../stores/stream'

interface Banner {
  tone: 'warn' | 'bad'
  title: string
  body: string
  /** One line per secret at fault: its name and what repairs it. */
  items: Array<{ name: string; fix: string }>
  link?: { to: string; label: string }
}

const stream = useStreamStore()
const secrets = useSecretsStore()
const session = useSessionStore()

watch(() => session.status, (s) => {
  if (s === 'signed-in') void secrets.load()
}, { immediate: true })
useStreamRefetch({ refetch: () => secrets.load() })

/** What repairs each reason (internal/secrets ReasonDoesNotOpen, ReasonBreaksRules). */
function fixFor(reason: string): string {
  switch (reason) {
    case 'does_not_open':
      return "Drydock's secrets key cannot open it. Store its value again."
    case 'breaks_write_rules':
      return 'It breaks the rules every write is held to, so it was stored around them. Delete it.'
  }
  return 'It cannot be delivered.'
}

const banner = computed<Banner | null>(() => {
  const fault = stream.entities.secretFault
  if (fault === null) return null
  const since = fault.since !== null ? ` Since ${relativeTime(fault.since)}.` : ''
  return {
    tone: 'bad',
    title: 'Stored secrets cannot be delivered.',
    body: `Every workspace's commands fail until this is fixed.${since}`,
    items: fault.secrets.map((s) => ({ name: s.name, fix: fixFor(s.reason) })),
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
      <ul v-if="banner.items.length > 0" class="items" data-test="fleet-items">
        <li v-for="i in banner.items" :key="i.name" data-test="fleet-item">
          <span class="mono">{{ i.name }}</span>: {{ i.fix }}
        </li>
      </ul>
      <RouterLink v-if="banner.link" :to="banner.link.to">{{ banner.link.label }}</RouterLink>
    </div>
  </div>
</template>

<style scoped>
.items { margin: 4px 0; padding-left: 18px; font-size: 13px; }
.mono { font-family: var(--mono); font-size: 12px; overflow-wrap: anywhere; }
</style>
