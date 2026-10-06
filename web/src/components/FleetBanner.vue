<script setup lang="ts">
// The fleet-wide banner slot (frontend §6.6). One fault with one cause gets
// one message here, above every card, rather than one on each.
//
// Two faults reach it, each from one reducer field:
//
// The Claude identity (Phase 5, design §7.3). The five states, and the
// wording rules that keep them apart, live in lib/identity.ts: blanked is
// "Signed out. Sign in again." — everyone just lost access — and absent is
// "No one has signed in yet." — the first run — though `auth status` says
// loggedIn:false for both. The banner carries the one Sign in to Claude
// there is; cards carry none (WorkspaceIdentityNote). Only the expiring
// countdown can be put away, and only until the countdown changes.
//
// Phase 4's: stored secrets that cannot be delivered. When the broker finds
// a stored secret it cannot deliver it fails *every* workspace's fetch rather
// than hand out half an environment (design §10.3), so every workspace's
// commands stop at once and none of them is at fault — §6.6's shape exactly.
//
// Both are entity state like any other (frontend §4.5 #2, #12): a GET
// carries each, so a reload shows a standing fault, and an event clears it.
// This component loads both for that reason — on every screen, since the
// banner is on every screen — and refetches them when the stream reopens. It
// renders only what the reducer holds.
import { computed, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useStreamRefetch } from '../lib/refetch'
import { identityBanner, SIGN_IN_TARGET } from '../lib/identity'
import { relativeTime } from '../lib/time'
import { useIdentityStore } from '../stores/identity'
import { useSecretsStore } from '../stores/secrets'
import { useSessionStore } from '../stores/session'
import { useStreamStore } from '../stores/stream'

interface Banner {
  key: string
  tone: 'warn' | 'bad'
  title: string
  body: string
  /** One line per secret at fault: its name and what repairs it. */
  items: Array<{ name: string; fix: string }>
  link?: { to: string | { path: string; hash: string }; label: string; button: boolean }
  dismiss?: () => void
}

const stream = useStreamStore()
const secrets = useSecretsStore()
const identity = useIdentityStore()
const session = useSessionStore()
const route = useRoute()
// On Settings the Claude section carries the handshake's own Sign in to
// Claude, so the banner there says what is wrong and offers no second
// button for the same fix (one cause, one button — §6.6).
const onSignInPage = computed(() => route.path === SIGN_IN_TARGET.path)

watch(() => session.status, (s) => {
  if (s === 'signed-in') {
    void secrets.load()
    void identity.load()
  }
}, { immediate: true })
useStreamRefetch({ refetch: () => secrets.load() })
useStreamRefetch({ refetch: () => identity.load() })

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

const banners = computed<Banner[]>(() => {
  const out: Banner[] = []
  const id = stream.entities.identity
  const ib = identityBanner(id)
  if (ib !== null && !(ib.dismissible && identity.dismissedFor === (id?.expiresAt ?? ''))) {
    out.push({
      key: `identity-${ib.state}`, tone: ib.tone, title: ib.title, body: ib.body, items: [],
      link: ib.action !== null && !onSignInPage.value ? { to: ib.action.to, label: ib.action.label, button: true } : undefined,
      dismiss: ib.dismissible ? () => identity.dismiss() : undefined,
    })
  }
  const fault = stream.entities.secretFault
  if (fault !== null) {
    const since = fault.since !== null ? ` Since ${relativeTime(fault.since)}.` : ''
    out.push({
      key: 'secrets', tone: 'bad',
      title: 'Stored secrets cannot be delivered.',
      body: `Every workspace's commands fail until this is fixed.${since}`,
      items: fault.secrets.map((s) => ({ name: s.name, fix: fixFor(s.reason) })),
      link: { to: '/secrets', label: 'Secrets', button: false },
    })
  }
  return out
})
</script>

<template>
  <div v-if="banners.length > 0" class="fleet">
    <div
      v-for="b in banners" :key="b.key" class="msg" :class="b.tone" :role="b.tone === 'bad' ? 'alert' : 'status'"
      data-test="fleet-banner" :data-banner="b.key"
    >
      <span class="glyph" aria-hidden="true">!</span>
      <div class="body">
        <b data-test="fleet-title">{{ b.title }}</b>
        <p>{{ b.body }}</p>
        <ul v-if="b.items.length > 0" class="items" data-test="fleet-items">
          <li v-for="i in b.items" :key="i.name" data-test="fleet-item">
            <span class="mono">{{ i.name }}</span>: {{ i.fix }}
          </li>
        </ul>
        <div v-if="b.link || b.dismiss" class="act">
          <RouterLink v-if="b.link" :to="b.link.to" :class="{ btn: b.link.button }" data-test="fleet-action">
            {{ b.link.label }}
          </RouterLink>
          <button v-if="b.dismiss" type="button" class="btn ghost" data-test="fleet-dismiss" @click="b.dismiss">
            Not now
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.fleet { display: flex; flex-direction: column; gap: 8px; }
.body { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.items { margin: 4px 0; padding-left: 18px; font-size: 13px; }
.mono { font-family: var(--mono); font-size: 12px; overflow-wrap: anywhere; }
.act { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: 4px; }
</style>
