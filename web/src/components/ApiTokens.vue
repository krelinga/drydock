<script setup lang="ts">
// Settings → API tokens (vscode-extension §4.2): create, the once-only
// display, the list, revoke.
//
// The list is entity state: GET /api/tokens and the api_token.* events
// through the reducer, nothing else (frontend §2.1). A mint's 201 carries the
// token — the one response in the API that carries a credential — and the
// token lives in `minted`, a component-local ref, from the response to *Done*
// (or leaving the page), and nowhere else: no store, no storage, no URL, no
// entity. Its row in the list arrives as api_token.created, like a token
// minted on another device.
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { describeError } from '../api/messages'
import { TOKEN_SCOPES, type TokenScope } from '../api/types'
import { relativeTime } from '../lib/time'
import { useStreamRefetch } from '../lib/refetch'
import type { ApiToken } from '../stores/reducer'
import { useSessionStore } from '../stores/session'
import { useStreamStore } from '../stores/stream'
import { CREATE_TOKEN_KEY, DEFAULT_TOKEN_DAYS, TOKEN_LIFETIMES, revokeTokenKey, useTokensStore } from '../stores/tokens'
import ActionButton from './ActionButton.vue'

const tokens = useTokensStore()
const session = useSessionStore()
const stream = useStreamStore()

onMounted(() => void tokens.load())
useStreamRefetch({ refetch: () => tokens.load() })

// What each scope lets the token do, in the words of vscode-extension §4.1.
const SCOPE_TEXT: Record<TokenScope, { name: string; does: string }> = {
  read: { name: 'Read', does: 'Workspaces, repositories, ports, logs and the event stream. Never secrets.' },
  lifecycle: { name: 'Lifecycle', does: 'Clone, start, stop, rebuild and delete workspaces; restart session servers; ports; the Claude login.' },
  approve: { name: 'Approve host access', does: 'Approve or decline what a dev container asks of this server.' },
}

/** "VS Code on Mac", from this device's label ("Mac Firefox"). */
function defaultLabel(): string {
  const cur = session.devices.find((d) => d.is_current)?.label ?? ''
  const platform = cur.split(' ')[0] ?? ''
  return platform === '' || platform === 'Unknown' ? 'VS Code' : `VS Code on ${platform}`
}

// Local state, all of it legitimate per frontend §4.2: a form open, its
// fields, one error, and the one display of a minted token.
const creating = ref(false)
const label = ref('')
const scopes = ref<Record<TokenScope, boolean>>({ read: true, lifecycle: true, approve: false })
const days = ref<number>(DEFAULT_TOKEN_DAYS)
const formError = ref<string | null>(null)
const minted = ref<{ id: string; token: string } | null>(null)
const copied = ref(false)

const createFlight = computed(() => stream.inFlight[CREATE_TOKEN_KEY] ?? null)
const chosen = computed(() => TOKEN_SCOPES.filter((s) => scopes.value[s]))
const labelOk = computed(() => {
  const l = label.value.trim()
  return l.length > 0 && [...l].length <= 100
})

function openForm(): void {
  label.value = defaultLabel()
  scopes.value = { read: true, lifecycle: true, approve: false }
  days.value = DEFAULT_TOKEN_DAYS
  formError.value = null
  creating.value = true
}

async function create(): Promise<void> {
  if (!labelOk.value || chosen.value.length === 0 || createFlight.value !== null) return
  formError.value = null
  try {
    minted.value = await tokens.create(label.value.trim(), chosen.value, days.value)
    copied.value = false
    creating.value = false
  } catch (e) {
    if (session.status === 'signed-in') formError.value = describeError(e)
  }
}

async function copy(): Promise<void> {
  if (minted.value === null) return
  try {
    await navigator.clipboard.writeText(minted.value.token)
    copied.value = true
  } catch {
    copied.value = false
  }
}

function done(): void {
  minted.value = null
  copied.value = false
}

onUnmounted(done)

const now = ref(Date.now())
const expired = (t: ApiToken) => Date.parse(t.expiresAt) <= now.value
const live = computed(() => tokens.list.filter((t) => t.revokedAt === null && !expired(t)).length)
</script>

<template>
  <section id="tokens" class="block" aria-labelledby="tokens-h" data-test="api-tokens">
    <div class="sec-label">
      <span id="tokens-h">API tokens</span>
      <span>{{ live }}</span>
    </div>
    <p class="note">
      For a client that is not a browser, such as Drydock for VS Code. A token is shown once, when it is created.
      It can never reach secrets, other tokens or signed-in devices.
    </p>

    <!-- The one display of a minted token. -->
    <div v-if="minted" class="sheet ok" role="group" aria-labelledby="minted-h" data-test="token-once">
      <h2 id="minted-h">Copy this token now</h2>
      <p class="note">Drydock will not show it again. Paste it where it is asked for, such as <i>Drydock: Sign In</i> in VS Code.</p>
      <code class="value" data-test="token-value">{{ minted.token }}</code>
      <div class="sheet-act">
        <button type="button" class="btn" data-test="token-copy" @click="copy">{{ copied ? 'Copied' : 'Copy' }}</button>
        <button type="button" class="btn primary" data-test="token-done" @click="done">Done</button>
      </div>
    </div>

    <div v-else-if="creating" class="sheet" role="group" aria-labelledby="new-token-h" data-test="token-form">
      <h2 id="new-token-h">New API token</h2>
      <div class="field" :class="{ err: !labelOk }">
        <label for="token-label">Label</label>
        <input id="token-label" v-model="label" type="text" maxlength="100" autocomplete="off" data-test="token-label">
      </div>
      <fieldset class="scopes">
        <legend>It may</legend>
        <label v-for="s in TOKEN_SCOPES" :key="s" class="scope" :data-test="`token-scope-${s}`">
          <input v-model="scopes[s]" type="checkbox">
          <span>
            <b>{{ SCOPE_TEXT[s].name }}</b>
            <span class="does">{{ SCOPE_TEXT[s].does }}</span>
          </span>
        </label>
        <!-- The card's own sentence about what approving means (frontend §6.5a). -->
        <p class="msg warn" data-test="token-approve-warning">
          <span class="glyph" aria-hidden="true">!</span>
          <span>
            Approving lets a repository's dev container reach this server, as the drydock user or as root.
            A token with this scope can do that without this page.
          </span>
        </p>
      </fieldset>
      <div class="field">
        <label for="token-days">Expires after</label>
        <select id="token-days" v-model.number="days" data-test="token-days">
          <option v-for="d in TOKEN_LIFETIMES" :key="d" :value="d">{{ d }} days</option>
        </select>
      </div>
      <div v-if="formError" class="msg bad" role="alert" data-test="token-error">
        <span class="glyph" aria-hidden="true">×</span><span>{{ formError }}</span>
      </div>
      <div class="sheet-act">
        <button type="button" class="btn" data-test="token-cancel" :disabled="createFlight !== null" @click="creating = false">Cancel</button>
        <button
          type="button" class="btn primary" data-test="token-create"
          :disabled="createFlight !== null || !labelOk || chosen.length === 0" :aria-busy="createFlight !== null"
          @click="create"
        >
          <span v-if="createFlight" class="spinner" aria-hidden="true" />
          Create token
        </button>
      </div>
    </div>

    <div v-else class="action">
      <button type="button" class="btn" data-test="token-new" @click="openForm">Create token</button>
    </div>

    <div v-if="tokens.error" class="msg bad" role="alert" data-test="tokens-load-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ describeError(tokens.error) }}</span>
    </div>

    <ul v-if="tokens.list.length > 0" class="tokens" data-test="tokens">
      <li v-for="t in tokens.list" :key="t.id" class="token" :class="{ over: t.revokedAt !== null || expired(t) }" data-test="token">
        <div class="head">
          <span class="label" data-test="token-row-label">{{ t.label }}</span>
          <span v-if="t.revokedAt !== null" class="tag off" data-test="token-revoked">Revoked</span>
          <span v-else-if="expired(t)" class="tag off" data-test="token-expired">Expired</span>
        </div>
        <div class="meta">
          <span data-test="token-scopes">{{ t.scopes.join(' · ') }}</span>
          <span>created {{ relativeTime(t.createdAt) }}</span>
          <span>{{ t.lastUsedAt ? `used ${relativeTime(t.lastUsedAt)}` : 'never used' }}</span>
          <span v-if="t.revokedAt !== null">revoked {{ relativeTime(t.revokedAt) }}</span>
          <span v-else>{{ expired(t) ? 'expired' : 'expires' }} {{ relativeTime(t.expiresAt) }}</span>
        </div>
        <ActionButton
          v-if="t.revokedAt === null && !expired(t)"
          label="Revoke" danger :flight-key="revokeTokenKey(t.id)" :run="() => tokens.revoke(t.id)"
          :data-test="`token-revoke-${t.id}`"
        />
      </li>
    </ul>
    <p v-else-if="tokens.loaded" class="note" data-test="tokens-none">No API tokens.</p>
  </section>
</template>

<style scoped>
.block { display: flex; flex-direction: column; gap: 10px; }
.note { font-size: 12.5px; color: var(--ink-3); }
.action { display: flex; flex-direction: column; gap: 6px; align-items: flex-start; }
.sheet {
  background: var(--surface); border: 1px solid var(--line); border-top: 2px solid var(--accent);
  border-radius: var(--r); padding: 14px; display: flex; flex-direction: column; gap: 12px;
}
.sheet.ok { border-top-color: var(--ok); }
.sheet h2 { font-size: 15px; margin: 0; }
.sheet-act { display: flex; gap: 8px; }
.sheet-act .btn { flex: 1; }
.value {
  font-family: var(--mono); font-size: 12.5px; overflow-wrap: anywhere; user-select: all;
  padding: 9px 11px; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface-2);
}
.scopes { border: 0; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.scopes legend { font-size: 13px; font-weight: 500; margin-bottom: 4px; }
.scope { display: flex; gap: 8px; align-items: flex-start; font-size: 13px; }
.scope input { margin-top: 3px; }
.does { display: block; color: var(--ink-3); font-size: 12.5px; }
select { font: inherit; padding: 7px 9px; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); color: var(--ink); }
.tokens {
  list-style: none; margin: 0; padding: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.token { padding: 11px 12px; border-bottom: 1px solid var(--line-soft); display: flex; flex-direction: column; gap: 6px; }
.token:last-child { border-bottom: 0; }
.token.over .label { color: var(--ink-3); }
.head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.label { font-size: 14px; font-weight: 500; overflow-wrap: anywhere; }
.tag {
  font-family: var(--mono); font-size: 10px; letter-spacing: .05em; text-transform: uppercase;
  padding: 2px 6px; border-radius: 3px; flex: none;
}
.tag.off { background: var(--line-soft); color: var(--ink-3); }
.meta { font-family: var(--mono); font-size: 11px; color: var(--ink-3); display: flex; flex-wrap: wrap; gap: 2px 12px; }
.spinner {
  width: 12px; height: 12px; border-radius: 50%;
  border: 2px solid var(--line); border-top-color: var(--ink-2);
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
