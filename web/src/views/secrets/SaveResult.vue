<script setup lang="ts">
// What a save did (frontend §6.4, design §10.3's table). This is an
// operation's result, not the secret's state: it comes from the PUT's own
// answer, is shown by the screen that asked, and is gone when that screen is.
//
// A rotation lists the running workspaces holding the old value, split by
// what it costs them, and the two are never merged:
//
//   new_commands               every Bash command reads current values, so
//                              the next one has the new value (Spike 03).
//   needs_supervisor_restart   something in it holds a frozen environment —
//                              an MCP server the session server launched.
//
// §10.3's warning about what a restart ends belongs to the second kind only:
// attached to the first it would teach the operator to restart sessions for
// nothing, which is the habit that eventually costs an agent mid-task. Until
// Phase 5 the server's StaleKind hook does not exist and the second list is
// always empty — and it is still rendered, as empty, so the structure the
// operator learns now is the one Phase 5 fills. There is no restart button:
// no route restarts a session server yet, and a button that cannot work is
// not shown disabled (§6.6).
import { RouterLink } from 'vue-router'
import type { PutOutcome } from '../../stores/secrets'

defineProps<{ name: string; outcome: PutOutcome }>()
defineEmits<{ dismiss: [] }>()
</script>

<template>
  <div class="result" role="status" data-test="save-result">
    <template v-if="outcome.created">
      <p class="msg ok"><span class="glyph" aria-hidden="true">✓</span><span>Stored {{ name }}.</span></p>
      <p data-test="result-ungranted">It is granted to nothing yet: no workspace receives it until you grant it a repository below.</p>
    </template>

    <template v-else-if="!outcome.rotated">
      <p class="msg ok"><span class="glyph" aria-hidden="true">✓</span><span>Saved.</span></p>
      <p data-test="result-unchanged">The value is the one already stored, so this was not a rotation and no workspace is stale.</p>
    </template>

    <template v-else>
      <p class="msg ok"><span class="glyph" aria-hidden="true">✓</span><span>Replaced the value of {{ name }}.</span></p>
      <p
        v-if="outcome.stale.new_commands.length === 0 && outcome.stale.needs_supervisor_restart.length === 0"
        data-test="result-none-running"
      >
        No running workspace holds it. Each one reads the new value when it next starts.
      </p>

      <section class="kind" aria-labelledby="stale-next-h" data-test="stale-new-commands">
        <h3 id="stale-next-h">Picks it up on its next command</h3>
        <ul v-if="outcome.stale.new_commands.length > 0">
          <li v-for="w in outcome.stale.new_commands" :key="w.workspace_id" data-test="stale-row">
            <RouterLink :to="{ name: 'workspace', params: { id: w.workspace_id } }">{{ w.full_name }}</RouterLink>
          </li>
        </ul>
        <p v-else class="none">None.</p>
        <p class="help">Nothing to do: every command reads the current value. A background process it already started keeps the old one.</p>
      </section>

      <section class="kind restart" aria-labelledby="stale-restart-h" data-test="stale-needs-restart">
        <h3 id="stale-restart-h">Needs a session server restart</h3>
        <template v-if="outcome.stale.needs_supervisor_restart.length > 0">
          <ul>
            <li v-for="w in outcome.stale.needs_supervisor_restart" :key="w.workspace_id" data-test="stale-row">
              <RouterLink :to="{ name: 'workspace', params: { id: w.workspace_id } }">{{ w.full_name }}</RouterLink>
            </li>
          </ul>
          <!-- §10.3's warning, attached to this kind and only this kind. -->
          <p class="msg warn" data-test="restart-warning">
            <span class="glyph" aria-hidden="true">!</span>
            <span>
              Something in these was started with the old value, such as an MCP server. Only restarting the
              session server re-launches it, and that ends every session it is serving. Ending one session and
              starting another from the Claude app is not enough: a new session inherits the server's old
              environment. Drydock never restarts one on its own.
            </span>
          </p>
        </template>
        <p v-else class="none" data-test="restart-none">None.</p>
      </section>
    </template>

    <button type="button" class="btn ghost" data-test="dismiss" @click="$emit('dismiss')">Done</button>
  </div>
</template>

<style scoped>
.result {
  display: flex; flex-direction: column; gap: 10px; align-items: flex-start;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r); padding: 14px;
  font-size: 14px;
}
.result > .msg { align-self: stretch; }
.kind { align-self: stretch; display: flex; flex-direction: column; gap: 4px; border-top: 1px solid var(--line-soft); padding-top: 8px; }
h3 { font-size: 13.5px; font-weight: 600; margin: 0; }
ul { margin: 0; padding-left: 18px; font-family: var(--mono); font-size: 13px; }
.none { font-size: 13px; color: var(--ink-3); }
.help { font-size: 12.5px; color: var(--ink-3); }
</style>
