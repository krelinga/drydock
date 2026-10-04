<script setup lang="ts">
// /signin — the only route reachable without a cookie (frontend §5).
//
// Three details that are security rather than polish:
//  - The form is never natively submitted (§8). `@submit.prevent` sends it
//    through the API client; and the password input has no `name`, so even
//    if the script failed to load and the browser submitted the form itself,
//    the password would not be in the request — and certainly not in a URL.
//  - `return` is validated before it is followed (lib/returnPath.ts).
//  - Messages come from the error envelope's code, never its prose.
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import BrandMark from '../components/BrandMark.vue'
import { ApiError } from '../api/client'
import { describeError } from '../api/messages'
import { safeReturnPath } from '../lib/returnPath'
import { useSessionStore } from '../stores/session'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()

const password = ref('')
const busy = ref(false)
const error = ref<string | null>(null)
const lockedOut = ref(false)
const input = ref<HTMLInputElement | null>(null)

const destination = computed(() => safeReturnPath(route.query.return))

onMounted(() => input.value?.focus())

async function submit(): Promise<void> {
  if (busy.value || password.value === '') return
  busy.value = true
  error.value = null
  lockedOut.value = false
  try {
    await session.signIn(password.value)
    password.value = ''
    await router.replace(destination.value)
  } catch (e) {
    error.value = describeError(e)
    lockedOut.value = e instanceof ApiError && e.code === 'locked_out'
    // Keep the field for a retry only when the server never judged it; a
    // judged-wrong password is cleared so the next attempt is a fresh one.
    if (e instanceof ApiError && (e.code === 'bad_password' || e.code === 'locked_out')) {
      password.value = ''
    }
    input.value?.focus()
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="page">
    <section class="card" aria-labelledby="signin-h">
      <div class="brand">
        <BrandMark :size="40" />
        <span class="name">Drydock</span>
      </div>
      <h1 id="signin-h" tabindex="-1">Sign in</h1>

      <form class="form" novalidate @submit.prevent="submit">
        <div class="field" :class="{ err: error }">
          <label for="signin-password">Password</label>
          <input
            id="signin-password"
            ref="input"
            v-model="password"
            type="password"
            autocomplete="current-password"
            autocapitalize="off"
            spellcheck="false"
            required
            :aria-invalid="error ? 'true' : undefined"
            :aria-describedby="error ? 'signin-error' : undefined"
          >
        </div>

        <div v-if="error" id="signin-error" class="msg" :class="lockedOut ? 'warn' : 'bad'" role="alert">
          <span class="glyph" aria-hidden="true">{{ lockedOut ? '!' : '×' }}</span>
          <span>{{ error }}</span>
        </div>

        <button type="submit" class="btn primary" :disabled="busy || password === ''">
          {{ busy ? 'Signing in…' : 'Sign in' }}
        </button>
      </form>

      <p class="hint">The password is set on the host with <code>drydock passwd</code>.</p>
    </section>
  </div>
</template>

<style scoped>
.page {
  min-height: 100dvh;
  display: flex; align-items: flex-start; justify-content: center;
  padding: calc(48px + env(safe-area-inset-top)) 16px 32px;
}
.card {
  width: 100%; max-width: 380px;
  background: var(--surface); border: 1px solid var(--line); border-radius: 8px;
  padding: 24px 20px;
  display: flex; flex-direction: column; gap: 18px;
}
.brand { display: flex; align-items: center; gap: 10px; }
.name { font-size: 18px; font-weight: 600; letter-spacing: -.01em; }
.form { display: flex; flex-direction: column; gap: 14px; }
.hint { font-size: 12.5px; color: var(--ink-3); }
</style>
