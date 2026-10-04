<script setup lang="ts">
// The boot probe failed for a reason other than "signed out": Drydock is
// unreachable or erroring. Say so, and offer the one action that can help,
// rather than sending a signed-in operator to a password prompt.
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError } from '../api/client'
import { describeError } from '../api/messages'
import { signInLocation } from '../routes'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()
const router = useRouter()
const busy = ref(false)

async function retry(): Promise<void> {
  busy.value = true
  const here = router.currentRoute.value.fullPath
  try {
    await session.load()
    session.probeError = null
  } catch (e) {
    if (session.status === 'signed-out') {
      // The probe got through and said 401: an ordinary signed-out boot.
      session.probeError = null
      await router.replace(signInLocation(here))
    } else {
      session.probeError = e instanceof ApiError ? e : new ApiError(0, 'network')
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="boot" aria-labelledby="boot-h">
    <h1 id="boot-h" tabindex="-1">Drydock did not answer</h1>
    <div class="msg bad" role="alert">
      <span class="glyph" aria-hidden="true">×</span>
      <span>{{ describeError(session.probeError) }}</span>
    </div>
    <div>
      <button type="button" class="btn primary" :disabled="busy" @click="retry">Try again</button>
    </div>
  </section>
</template>

<style scoped>
.boot { display: flex; flex-direction: column; gap: 14px; }
</style>
