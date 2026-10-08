<script setup lang="ts">
// The login handshake view (frontend §6.2, design §7.2), in Settings' Claude
// section — where the fleet banner's one Sign in to Claude leads.
//
// Its state is the server's: the reducer's one `login` field, written by GET
// /api/auth/claude and auth.login. A reload, or a phone that discarded the
// page while the operator was in their browser, finds the login where it was
// (§2.4). Every button is ActionButton, in flight until the event that ends
// what it asked (§4.2).
//
// The code is the one thing typed here that must not outlive its use. It is
// one ref in this component — not the store, not storage, not the URL — in a
// field inside no <form> with autofill, capitalisation and spelling off, and
// it is taken out of the field and the field cleared before the request is
// sent. A wrong code is a loop: the field opens empty again beside the error,
// with the same link and the countdown still running (Spike 01).
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import ActionButton from './ActionButton.vue'
import { acceptsCode, checkCodeShape, codeRuleSentence, countdown, loginLive } from '../lib/login'
import { LOGIN_BEGIN_KEY, LOGIN_CANCEL_KEY, LOGIN_CODE_KEY, useIdentityStore } from '../stores/identity'
import { loginEnding } from '../lib/identity'
import { useStreamStore } from '../stores/stream'

const stream = useStreamStore()
const identity = useIdentityStore()
const login = computed(() => stream.entities.login)
const id = computed(() => stream.entities.identity)
const live = computed(() => login.value !== null && loginLive(login.value.phase))
// Only what no session server can fix by itself needs a sign-in: expired is
// the access token, which the next server renews (design §7.3).
const needsSignIn = computed(() => id.value?.state === 'blanked' || id.value?.state === 'absent')

// Local, and legitimately so (§4.2): the field being typed into, a shape
// complaint about it, "Copied", and a clock for the countdown.
const code = ref('')
const shape = ref<string | null>(null)
const copied = ref(false)
const now = ref(Date.now())
let tick: ReturnType<typeof setInterval> | null = null
onMounted(() => { tick = setInterval(() => { now.value = Date.now() }, 1000) })
onBeforeUnmount(() => {
  if (tick !== null) clearInterval(tick)
  code.value = ''
})

const left = computed(() => (login.value?.deadline ? countdown(login.value.deadline, now.value) : null))

const startLabel = computed(() => {
  const l = login.value
  if (l !== null && (l.phase === 'timed_out' || l.phase === 'failed' || l.phase === 'cancelled')) return 'Start over'
  return needsSignIn.value || loginEnding(id.value) !== null ? 'Sign in to Claude' : 'Sign in again'
})

async function begin(): Promise<void> {
  await identity.beginLogin()
}

async function submit(): Promise<void> {
  const l = login.value
  if (l === null) return
  const rule = checkCodeShape(code.value)
  if (rule !== null) {
    // Not sent: said here, at once, and the field keeps what was pasted so
    // the operator can see what is missing.
    shape.value = codeRuleSentence(rule)
    return
  }
  shape.value = null
  const value = code.value.trim()
  code.value = ''
  await identity.submitCode(l.id, value)
}

async function cancel(): Promise<void> {
  if (login.value !== null) await identity.cancelLogin(login.value.id)
}

async function copy(): Promise<void> {
  const url = login.value?.url
  if (!url) return
  try {
    await navigator.clipboard.writeText(url)
    copied.value = true
    setTimeout(() => { copied.value = false }, 2000)
  } catch {
    copied.value = false
  }
}
</script>

<template>
  <div class="login" data-test="claude-login" :data-phase="login?.phase ?? 'none'">
    <template v-if="live && login">
      <p v-if="login.phase === 'starting'" class="state" role="status" data-test="login-starting">
        <span class="spinner" aria-hidden="true" />
        Starting a login container… The first time, Drydock builds the image Claude Code runs in, which can take a few minutes.
      </p>

      <template v-else>
        <p class="note">
          Open the sign-in page, sign in to Claude, and paste the code it shows you below.
          This signs in <b>every workspace</b>: they all share one login.
        </p>
        <div class="link-row">
          <a
            v-if="login.url" :href="login.url" target="_blank" rel="noopener noreferrer"
            class="btn primary url" data-test="login-url"
          >Open the Claude sign-in page</a>
          <button type="button" class="btn" data-test="login-copy" @click="copy">
            {{ copied ? 'Copied' : 'Copy link' }}
          </button>
        </div>
        <p v-if="left" class="deadline" role="timer" data-test="login-deadline">
          Time left: <span class="mono">{{ left }}</span>
        </p>

        <div v-if="login.phase === 'invalid_code'" class="msg bad" role="alert" data-test="login-invalid">
          <span class="glyph" aria-hidden="true">×</span>
          <span>Claude did not accept that code. Paste it again: the link is still valid.</span>
        </div>

        <!-- No <form>: nothing here may be offered to a password manager or
             autofill, and Enter must not submit a native form. -->
        <div v-if="acceptsCode(login.phase) || login.phase === 'submitting'" class="code">
          <label for="claude-code">Code from Claude</label>
          <input
            id="claude-code" v-model="code" data-test="login-code"
            type="text" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false"
            :disabled="login.phase === 'submitting'"
          >
          <p v-if="shape" class="msg warn" role="alert" data-test="login-shape">
            <span class="glyph" aria-hidden="true">!</span><span>{{ shape }}</span>
          </p>
          <p v-if="login.phase === 'submitting'" class="note" role="status" data-test="login-checking">
            Checking the code with Claude…
          </p>
          <ActionButton
            label="Submit code" :flight-key="LOGIN_CODE_KEY" :run="submit" primary
            :blocked="code.trim() === '' || login.phase === 'submitting'"
          />
        </div>
      </template>

      <ActionButton label="Cancel sign-in" :flight-key="LOGIN_CANCEL_KEY" :run="cancel" />
    </template>

    <template v-else>
      <div
        v-if="login && login.phase !== 'succeeded'" class="msg warn" role="status" data-test="login-ended"
      >
        <span class="glyph" aria-hidden="true">!</span><span>{{ login.message }}</span>
      </div>
      <p v-else-if="login && login.phase === 'succeeded'" class="msg ok" role="status" data-test="login-succeeded">
        <span class="glyph" aria-hidden="true">✓</span><span>Signed in. Every workspace uses this login.</span>
      </p>
      <ActionButton :label="startLabel" :flight-key="LOGIN_BEGIN_KEY" :run="begin" :primary="needsSignIn" />
      <p class="note">One sign-in covers every workspace: they all share this login.</p>
    </template>
  </div>
</template>

<style scoped>
.login { display: flex; flex-direction: column; gap: 10px; align-items: flex-start; }
.note { font-size: 12.5px; color: var(--ink-3); }
.state { display: flex; gap: 8px; align-items: center; font-size: 13px; }
.link-row { display: flex; flex-wrap: wrap; gap: 8px; }
.url { overflow-wrap: anywhere; }
.deadline { font-size: 12.5px; color: var(--ink-2); }
.mono { font-family: var(--mono); }
.code { display: flex; flex-direction: column; gap: 6px; align-self: stretch; }
.code label { font-size: 12.5px; color: var(--ink-2); }
.code input {
  font-family: var(--mono); font-size: 14px; padding: 8px 10px;
  border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); color: var(--ink);
  min-width: 0; width: 100%;
}
.spinner {
  width: 12px; height: 12px; border-radius: 50%; flex: none;
  border: 2px solid var(--line); border-top-color: var(--ink-2);
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
