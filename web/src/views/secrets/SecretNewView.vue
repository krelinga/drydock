<script setup lang="ts">
// /secrets/new (frontend §5, §6.4). The form, then the secret's own page —
// where its grants start empty and say so, because storing a secret grants
// it to nothing (design §10.1).
import { onMounted } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { useStreamRefetch } from '../../lib/refetch'
import { useSecretsStore } from '../../stores/secrets'
import SecretForm from './SecretForm.vue'

const router = useRouter()
const secrets = useSecretsStore()

// The form refuses a name this list already holds (#29), so it is loaded
// here and kept current, not left to the banner that happens to load it too.
onMounted(() => {
  void secrets.load()
})
useStreamRefetch({ refetch: () => secrets.load() })

function saved(name: string): void {
  void router.replace({ name: 'secret', params: { name } })
}
</script>

<template>
  <section class="view" aria-labelledby="new-h">
    <RouterLink :to="{ name: 'secrets' }" class="back">← Secrets</RouterLink>
    <h1 id="new-h" tabindex="-1">New secret</h1>
    <p class="lede">
      Stored encrypted, delivered only to the repositories you grant it to, and never shown again. It is granted
      to nothing until you choose.
    </p>
    <SecretForm mode="create" @saved="saved" @cancel="router.push({ name: 'secrets' })" />
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 14px; }
.back { font-size: 13px; }
.lede { font-size: 13.5px; color: var(--ink-2); }
</style>
