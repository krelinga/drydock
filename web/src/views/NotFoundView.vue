<script setup lang="ts">
import { RouterLink } from 'vue-router'
</script>

<template>
  <section class="view" aria-labelledby="nf-h">
    <h1 id="nf-h" tabindex="-1">Nothing here</h1>
    <p class="sub">This address is not a page in Drydock.</p>
    <p><RouterLink to="/">Back to workspaces</RouterLink></p>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 12px; }
.sub { color: var(--ink-2); }
</style>
