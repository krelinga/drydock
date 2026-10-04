<script setup lang="ts">
// /settings — Phase 1 carries the device list (design §13.2: "a device list in
// the UI, and one button that kills all sessions"). Claude identity, capacity
// and the catalog refresh join it in later phases.
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

const session = useSessionStore()
const router = useRouter()

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

.sheet {
  align-self: stretch;
  background: var(--surface); border: 1px solid var(--line); border-top: 2px solid var(--bad);
  border-radius: var(--r);
  padding: 14px; display: flex; flex-direction: column; gap: 12px;
}
.sheet-act { display: flex; gap: 8px; }
.sheet-act .btn { flex: 1; }
</style>
