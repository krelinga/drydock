<script setup lang="ts">
// Design §12, *Repeated failed sign-ins*: surfaced once, after the sign-in
// that learned of them. On a home LAN this is usually a stale saved password
// on a forgotten device, and you want to see it either way; the one action,
// if it was not you, is Settings' Sign out everywhere and a new password.
import { RouterLink } from 'vue-router'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()
</script>

<template>
  <div v-if="session.failedNotice" class="msg warn" role="status" data-test="failed-sign-ins">
    <span class="glyph" aria-hidden="true">!</span>
    <div>
      {{ session.failedNotice.attempts === 1 ? 'One failed sign-in' : `${session.failedNotice.attempts} failed sign-ins` }}
      since the last successful one<template v-if="session.failedNotice.sources.length > 0">,
        from {{ session.failedNotice.sources.join(', ') }}</template>.
      Usually a device with an old saved password. If it was not you,
      <RouterLink to="/settings">sign out everywhere</RouterLink> and set a new password with
      <code>drydock passwd</code>.
      <button type="button" class="btn ghost" data-test="failed-sign-ins-dismiss" @click="session.failedNotice = null">
        Dismiss
      </button>
    </div>
  </div>
</template>
