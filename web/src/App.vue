<script setup lang="ts">
// The shell: header, the three-destination nav, the fleet-banner slot, and
// the routed view (frontend §3, §5). /signin renders bare — a signed-out
// visitor gets no navigation to destinations they cannot reach.
import { computed, nextTick, ref, watch } from 'vue'
import { RouterView, useRoute } from 'vue-router'
import AppHeader from './components/AppHeader.vue'
import AppNav from './components/AppNav.vue'
import FleetBanner from './components/FleetBanner.vue'
import DiskBanner from './components/DiskBanner.vue'
import BootFailure from './components/BootFailure.vue'
import { useSessionStore } from './stores/session'
import { useStreamStore } from './stores/stream'

const route = useRoute()
const session = useSessionStore()
const stream = useStreamStore()
const bare = computed(() => route.meta.public === true)

// One stream for the whole signed-in app (frontend §4.1). Signing out closes
// it through clearEntityState; this opens it again on the next sign-in.
watch(
  () => session.status,
  (s) => {
    if (s === 'signed-in') stream.connect()
  },
  { immediate: true },
)

// §7: on route change, focus moves to the view's <h1>, and one polite live
// region says where you are. Not on first load, where the sign-in field's
// own autofocus is the better target.
const announcement = ref('')
watch(
  () => route.fullPath,
  async (_to, from) => {
    if (from === undefined) return
    announcement.value = route.meta.title
    await nextTick()
    document.querySelector<HTMLElement>('#main h1, #bare h1')?.focus()
  },
)
</script>

<template>
  <div v-if="bare" id="bare" class="bare">
    <RouterView />
  </div>
  <div v-else class="shell">
    <AppHeader class="shell-header" />
    <AppNav class="shell-nav" />
    <main id="main" class="shell-main">
      <FleetBanner />
      <DiskBanner />
      <BootFailure v-if="session.probeError" />
      <RouterView v-else />
    </main>
  </div>
  <div class="vis-sr" aria-live="polite">{{ announcement }}</div>
</template>

<style scoped>
.bare { min-height: 100dvh; }

/* Mobile first: header, content, and a bottom tab bar (frontend §5, §7). */
.shell {
  min-height: 100dvh;
  display: grid;
  grid-template-rows: auto 1fr auto;
  grid-template-areas: "header" "main" "nav";
}
.shell-header { grid-area: header; }
.shell-main {
  grid-area: main;
  padding: 14px 14px 20px;
  display: flex; flex-direction: column; gap: 16px;
  min-width: 0;
}
.shell-nav { grid-area: nav; position: sticky; bottom: 0; }

/* ≥ 900 px: the same three destinations become a left rail. */
@media (min-width: 900px) {
  .shell {
    grid-template-columns: var(--rail) minmax(0, 1fr);
    grid-template-rows: auto 1fr;
    grid-template-areas: "header header" "nav main";
  }
  .shell-nav { position: static; }
  .shell-main { padding: 22px 28px 32px; max-width: 880px; }
}
</style>
