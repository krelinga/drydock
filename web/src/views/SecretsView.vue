<script setup lang="ts">
// /secrets — a lazy chunk (frontend §3.1). Phase 4 builds the list, the
// write-only form with `reach`, and grants.
</script>

<template>
  <section class="view" aria-labelledby="sec-h">
    <h1 id="sec-h" tabindex="-1">Secrets</h1>
    <div class="empty">
      <span>No secrets yet.</span>
      <span class="sub">
        Secrets are stored encrypted and reach only the repositories you grant them to. A value is
        never shown again once saved.
      </span>
    </div>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 18px; }
</style>
