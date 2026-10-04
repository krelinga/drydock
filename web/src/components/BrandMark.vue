<script setup lang="ts">
// The Drydock mark (brand/icons/drydock-mark.svg), inlined so its colours come
// from tokens.css. The shipped SVG switches palette with an internal <style>
// block, which the UI's CSP (style-src 'self') would refuse; inlined, the same
// geometry takes its colours from this component's stylesheet instead.
// Geometry must match the icon file — see brand-design.md on drift.
defineProps<{ size?: number }>()
</script>

<template>
  <svg
    class="mark"
    viewBox="0 0 32 32"
    :width="size ?? 28"
    :height="size ?? 28"
    fill="none"
    aria-hidden="true"
    focusable="false"
  >
    <path class="ink-s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" d="M3.1 8 8.6 24H23.4L28.9 8" />
    <rect class="ink-f" x="12.6" y="20.9" width="2" height="2.2" rx="0.4" />
    <rect class="ink-f" x="17.4" y="20.9" width="2" height="2.2" rx="0.4" />
    <rect class="acc-f" x="11" y="10.9" width="10" height="10" rx="1.6" />
    <path class="bg-s" stroke-width="0.8" stroke-linejoin="round" d="M12.5 18.1V13.7h0.5a2.2 2.2 0 0 1 0 4.4H12.5" />
    <path class="bg-s" stroke-width="0.8" stroke-linejoin="round" d="M16.8 18.1V13.7h0.5a2.2 2.2 0 0 1 0 4.4H16.8" />
  </svg>
</template>

<style scoped>
.mark { flex: none; display: block; }
.ink-s { stroke: var(--mark-ink); }
.ink-f { fill: var(--mark-ink); }
.acc-f { fill: var(--mark-accent); }
.bg-s  { stroke: var(--mark-bg); }
</style>
