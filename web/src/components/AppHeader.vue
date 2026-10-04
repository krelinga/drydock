<script setup lang="ts">
import { RouterLink } from 'vue-router'
import BrandMark from './BrandMark.vue'
</script>

<template>
  <header class="top">
    <!-- No wordmark exists yet (brand-design.md), so the name is plain text. -->
    <RouterLink to="/" class="brand" aria-label="Drydock, home">
      <BrandMark :size="26" />
      <span class="name">Drydock</span>
    </RouterLink>
    <!-- Phase 2: the stream's "reconnecting" marker lives here (§4.3), never a toast. -->
    <span class="status" />
  </header>
</template>

<style scoped>
.top {
  display: flex; align-items: center; justify-content: space-between; gap: 12px;
  padding: calc(8px + env(safe-area-inset-top)) 14px 8px;
  min-height: 52px;
  border-bottom: 1px solid var(--line);
  background: var(--surface);
}
.brand {
  display: inline-flex; align-items: center; gap: 9px;
  color: var(--ink); text-decoration: none;
  min-height: var(--tap);
}
.brand:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }
.name { font-size: 16px; font-weight: 600; letter-spacing: -.01em; }
.status { font-family: var(--mono); font-size: 10.5px; color: var(--ink-3); }
</style>
