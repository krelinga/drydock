<script setup lang="ts">
// /settings — the device list (design §13.2: "a device list in the UI, and
// one button that kills all sessions"), the shared Claude login (design §7.3)
// and the catalog refresh. Capacity joins it in a later phase.
//
// The API offers two revocations, both of which include this device: sign out
// here, or sign out everywhere. Neither is a surprise — the list marks which
// row is you (frontend §4.5 #6), and "everywhere" asks first, naming that it
// includes this device.
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { describeError } from '../api/messages'
import { endSession } from '../signout'
import { relativeTime } from '../lib/time'
import { useSessionStore } from '../stores/session'
import { REFRESH_KEY, useCatalogStore } from '../stores/catalog'
import { useStreamStore } from '../stores/stream'
import { CHECK_KEY, useIdentityStore } from '../stores/identity'
import { identitySentence } from '../lib/identity'
import ClaudeLogin from '../components/ClaudeLogin.vue'

const session = useSessionStore()
const catalog = useCatalogStore()
const stream = useStreamStore()
const router = useRouter()

// The catalog refresh (§5's table): POST, discard the 202, and show only that
// a request is in flight until repo.refreshed or repo.refresh_failed arrives
// (§4.2). The outcome shown is the reducer's, from the event, never from the
// response — so a refresh started on another device reports here too.
const refreshFlight = computed(() => stream.inFlight[REFRESH_KEY] ?? null)
const lastRefresh = computed(() => stream.entities.lastRefresh)
const refreshError = ref<string | null>(null)

async function refreshCatalog(): Promise<void> {
  refreshError.value = null
  try {
    await catalog.refresh()
  } catch (e) {
    if (session.status === 'signed-in') refreshError.value = describeError(e)
  }
}

// The Claude identity (design §7.3). Loaded by the fleet banner on every
// screen; this section renders the same reducer field in full. "Check now"
// is a 202: in flight until the check's event lands.
const identity = useIdentityStore()
const id = computed(() => stream.entities.identity)
const checkFlight = computed(() => stream.inFlight[CHECK_KEY] ?? null)
const checkError = ref<string | null>(null)

async function checkNow(): Promise<void> {
  checkError.value = null
  try {
    await identity.check()
  } catch (e) {
    if (session.status === 'signed-in') checkError.value = describeError(e)
  }
}

// Local state, all of it legitimate per §4.2: one request in flight, one
// sheet open, one error to show.
const busy = ref<'here' | 'everywhere' | 'refresh' | null>(null)
const confirming = ref(false)
const error = ref<string | null>(null)

const devices = computed(() =>
  [...session.devices].sort((a, b) => Number(b.is_current) - Number(a.is_current) || b.last_seen_at.localeCompare(a.last_seen_at)),
)
const others = computed(() => session.devices.filter((d) => !d.is_current).length)

async function refresh(): Promise<void> {
  busy.value = 'refresh'
  error.value = null
  try {
    await session.load()
  } catch (e) {
    if (session.status === 'signed-in') error.value = describeError(e)
  } finally {
    busy.value = null
  }
}

onMounted(refresh)

async function signOut(everywhere: boolean): Promise<void> {
  busy.value = everywhere ? 'everywhere' : 'here'
  error.value = null
  try {
    await session.signOut(everywhere)
    await endSession(router)
  } catch (e) {
    if (session.status === 'signed-in') error.value = describeError(e)
  } finally {
    busy.value = null
    confirming.value = false
  }
}
</script>

<template>
  <section class="view" aria-labelledby="settings-h">
    <h1 id="settings-h" tabindex="-1">Settings</h1>

    <section class="block" aria-labelledby="devices-h">
      <div class="sec-label">
        <span id="devices-h">Signed-in devices</span>
        <span>{{ session.devices.length }}</span>
      </div>

      <div v-if="error" class="msg bad" role="alert">
        <span class="glyph" aria-hidden="true">×</span><span>{{ error }}</span>
      </div>

      <ul class="devices" data-test="devices">
        <li v-for="d in devices" :key="d.id" class="device" :class="{ current: d.is_current }" data-test="device">
          <div class="head">
            <span class="label">{{ d.label }}</span>
            <span v-if="d.is_current" class="tag" data-test="this-device">This device</span>
          </div>
          <div class="meta">
            <span>{{ d.created_ip }}</span>
            <span>active {{ relativeTime(d.last_seen_at) }}</span>
            <span>signed in {{ relativeTime(d.created_at) }}</span>
          </div>
        </li>
      </ul>

      <div class="actions">
        <div class="action">
          <button
            type="button" class="btn" data-test="sign-out"
            :disabled="busy !== null" @click="signOut(false)"
          >
            Sign out of this device
          </button>
          <p class="note">Ends this device's session only.</p>
        </div>

        <div class="action">
          <button
            v-if="!confirming"
            type="button" class="btn ghost danger-text" data-test="sign-out-everywhere"
            :disabled="busy !== null" @click="confirming = true"
          >
            Sign out everywhere
          </button>

          <!-- A sheet in place, not a modal (frontend §7). -->
          <div v-else class="sheet" role="group" aria-labelledby="everywhere-h" data-test="confirm-everywhere">
            <h2 id="everywhere-h">Sign out every device?</h2>
            <p class="warning msg warn" data-test="includes-current">
              <span class="glyph" aria-hidden="true">!</span>
              <span>
                This includes <b>this device</b><template v-if="others > 0">
                  and {{ others }} other{{ others === 1 ? '' : 's' }}</template>.
                You will need the password to sign back in.
              </span>
            </p>
            <div class="sheet-act">
              <button type="button" class="btn" data-test="cancel" :disabled="busy !== null" @click="confirming = false">
                Cancel
              </button>
              <button type="button" class="btn danger" data-test="confirm" :disabled="busy !== null" @click="signOut(true)">
                Sign out everywhere
              </button>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- The shared Claude login (design §7.3, frontend §5). The fleet banner's
         one Sign in to Claude lands here. -->
    <section id="claude" class="block" aria-labelledby="claude-h" data-test="claude-identity">
      <div class="sec-label"><span id="claude-h">Claude</span></div>
      <p class="state-line" data-test="claude-state">{{ identitySentence(id) }}</p>
      <dl v-if="id !== null" class="facts">
        <template v-if="id.accountEmail">
          <dt>Account</dt><dd data-test="claude-account">{{ id.accountEmail }}</dd>
        </template>
        <!-- The login's own end (the refresh token's), when Claude Code
             recorded one. The access token's expiry is not shown as an
             expiry: it is hours away after every sign-in and moves with
             every refresh (design §7.3) — only its lapse, which is
             informational. -->
        <template v-if="id.loginExpiresAt">
          <dt>Login expires</dt>
          <dd data-test="claude-expires">{{ relativeTime(id.loginExpiresAt) }}</dd>
        </template>
        <template v-if="id.state === 'expired' && id.expiresAt">
          <dt>Access token lapsed</dt>
          <dd data-test="claude-access-lapsed">{{ relativeTime(id.expiresAt) }}</dd>
        </template>
        <template v-if="id.loggedInAt">
          <dt>First seen</dt><dd>{{ relativeTime(id.loggedInAt) }}</dd>
        </template>
        <template v-if="id.lastCheckedAt">
          <dt>Checked</dt><dd data-test="claude-checked">{{ relativeTime(id.lastCheckedAt) }}</dd>
        </template>
        <dt>Volume</dt><dd class="mono">{{ id.volume }}</dd>
      </dl>
      <div v-if="id?.checkError" class="msg warn" role="status" data-test="claude-check-error">
        <span class="glyph" aria-hidden="true">!</span>
        <span>{{ id.checkError.message }} Last tried {{ relativeTime(id.checkError.at) }}.</span>
      </div>
      <!-- The login handshake (frontend §6.2): the fleet banner's one Sign in
           to Claude leads here, and on this page the banner offers no second
           one — this is it. -->
      <ClaudeLogin />
      <div class="action">
        <button
          type="button" class="btn" data-test="claude-check"
          :disabled="checkFlight !== null" :aria-busy="checkFlight !== null" @click="checkNow"
        >
          <span v-if="checkFlight" class="spinner" aria-hidden="true" />
          Check now
        </button>
        <p class="note">Drydock checks the login every six hours by itself.</p>
      </div>
      <div v-if="checkError" class="msg bad" role="alert" data-test="claude-check-refused">
        <span class="glyph" aria-hidden="true">×</span><span>{{ checkError }}</span>
      </div>
    </section>

    <section class="block" aria-labelledby="catalog-h">
      <div class="sec-label"><span id="catalog-h">Repository catalog</span></div>
      <div class="action">
        <button
          type="button" class="btn" data-test="refresh-catalog"
          :disabled="refreshFlight !== null" :aria-busy="refreshFlight !== null"
          @click="refreshCatalog"
        >
          <span v-if="refreshFlight" class="spinner" aria-hidden="true" />
          Refresh catalog
        </button>
        <p class="note" aria-live="polite" data-test="refresh-status">
          <template v-if="refreshFlight?.slow">No response yet. The refresh was accepted and is still running.</template>
          <template v-else-if="refreshFlight">Asking GitHub…</template>
          <template v-else-if="lastRefresh?.ok">
            Refreshed {{ relativeTime(lastRefresh.at) }}: {{ lastRefresh.count }} repositories.
          </template>
          <template v-else-if="lastRefresh">{{ lastRefresh.message }}</template>
          <template v-else>Drydock re-reads the list from GitHub every 15 minutes.</template>
        </p>
      </div>
      <div v-if="refreshError" class="msg bad" role="alert" data-test="refresh-error">
        <span class="glyph" aria-hidden="true">×</span><span>{{ refreshError }}</span>
      </div>
    </section>
  </section>
</template>

<style scoped>
.view { display: flex; flex-direction: column; gap: 18px; }
.block { display: flex; flex-direction: column; gap: 10px; }

.devices {
  list-style: none; margin: 0; padding: 0;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r);
}
.device { padding: 11px 12px; border-bottom: 1px solid var(--line-soft); display: flex; flex-direction: column; gap: 3px; }
.device:last-child { border-bottom: 0; }
.device.current { border-left: 3px solid var(--accent); padding-left: 9px; }
.head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.label { font-size: 14px; font-weight: 500; }
.tag {
  font-family: var(--mono); font-size: 10px; letter-spacing: .05em; text-transform: uppercase;
  padding: 2px 6px; border-radius: 3px; background: var(--ok-bg); color: var(--ok); flex: none;
}
.meta {
  font-family: var(--mono); font-size: 11px; color: var(--ink-3);
  display: flex; flex-wrap: wrap; gap: 2px 12px;
}

.actions { display: flex; flex-direction: column; gap: 14px; margin-top: 4px; }
.action { display: flex; flex-direction: column; gap: 6px; align-items: flex-start; }
.note { font-size: 12.5px; color: var(--ink-3); }
.danger-text { color: var(--bad); border-color: var(--bad); }
.state-line { font-size: 14px; font-weight: 500; }
.facts {
  margin: 0; display: grid; grid-template-columns: max-content 1fr; gap: 3px 12px;
  font-size: 12.5px; color: var(--ink-2);
}
.facts dt { color: var(--ink-3); }
.facts dd { margin: 0; overflow-wrap: anywhere; }
.facts .mono { font-family: var(--mono); font-size: 12px; }
.spinner {
  width: 12px; height: 12px; border-radius: 50%;
  border: 2px solid var(--line); border-top-color: var(--ink-2);
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

.sheet {
  align-self: stretch;
  background: var(--surface); border: 1px solid var(--line); border-top: 2px solid var(--bad);
  border-radius: var(--r);
  padding: 14px; display: flex; flex-direction: column; gap: 12px;
}
.sheet-act { display: flex; gap: 8px; }
.sheet-act .btn { flex: 1; }
</style>
