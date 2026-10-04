<script setup lang="ts">
// Home (frontend §5). Phase 2 fills it — the `Running` cards above the
// repository catalog — from the event stream. Until then it says, honestly,
// that there is nothing to show and why.
</script>

<template>
  <section class="view" aria-labelledby="ws-h">
    <h1 id="ws-h" tabindex="-1">Workspaces</h1>

    <div class="block">
      <div class="sec-label"><span>Running</span><span>0</span></div>
      <div class="empty">
        <span>Nothing is running.</span>
        <span class="sub">Workspaces appear here once a repository has been cloned.</span>
      </div>
    </div>

    <div class="block">
      <div class="sec-label"><span>All repositories</span></div>
      <div class="empty">
        <span>The repository catalog is not built yet.</span>
        <span class="sub">Repositories appear here when the GitHub App is installed on them.</span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 18px; }
.block { display: flex; flex-direction: column; gap: 8px; }
</style>
