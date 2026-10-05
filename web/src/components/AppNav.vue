<script setup lang="ts">
// Three destinations, no nesting (frontend §5): a bottom tab bar on a phone,
// a left rail at ≥ 900 px. One element, two layouts — not two navs to keep in
// sync.
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

const route = useRoute()
const items = [
  { to: '/', name: 'workspaces', label: 'Workspaces', glyph: '▤' },
  { to: '/secrets', name: 'secrets', label: 'Secrets', glyph: '◆' },
  { to: '/settings', name: 'settings', label: 'Settings', glyph: '◎' },
] as const
// A workspace's detail is reached from Workspaces, and a secret's from
// Secrets, so those tabs stay current.
const active = computed(() => {
  if (route.name === 'workspace') return 'workspaces'
  if (route.name === 'secret' || route.name === 'secret-new') return 'secrets'
  return route.name
})
</script>

<template>
  <nav class="nav" aria-label="Main">
    <RouterLink
      v-for="it in items"
      :key="it.name"
      :to="it.to"
      class="item"
      :aria-current="active === it.name ? 'page' : undefined"
    >
      <span class="glyph" aria-hidden="true">{{ it.glyph }}</span>
      <span>{{ it.label }}</span>
    </RouterLink>
  </nav>
</template>

<style scoped>
.nav {
  display: flex;
  border-top: 1px solid var(--line);
  background: var(--surface);
  padding-bottom: env(safe-area-inset-bottom);
}
.item {
  flex: 1;
  min-height: 56px;
  display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 2px;
  font-size: 11.5px; font-weight: 500;
  color: var(--ink-3); text-decoration: none;
  touch-action: manipulation;
}
.item .glyph { font-family: var(--mono); font-size: 14px; }
.item[aria-current="page"] { color: var(--accent); }
.item:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }

@media (min-width: 900px) {
  .nav {
    flex-direction: column; gap: 3px;
    border-top: 0; border-right: 1px solid var(--line);
    padding: 16px 12px;
  }
  .item {
    flex: none; min-height: 40px;
    flex-direction: row; justify-content: flex-start; gap: 10px;
    padding: 0 10px; border-radius: 4px;
    font-size: 14px; font-weight: 400; color: var(--ink-2);
  }
  .item .glyph { font-size: 12px; width: 14px; text-align: center; }
  .item[aria-current="page"] { background: var(--surface-2); color: var(--accent); font-weight: 500; }
}
</style>
