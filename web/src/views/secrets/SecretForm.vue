<script setup lang="ts">
// The secret form (frontend §6.4): create one, or edit one — its reach, its
// description, its value, or any of them. Where `reach` is the feature —
// design §10.4: "Drydock will not store a secret until you have written down
// what someone could do with it."
//
// Editing leaves the value alone unless one is typed. An empty value field
// sends no `value` key, which the server reads as "keep the stored one"
// (frontend §4.5 #13), so narrowing what a secret reaches never asks for the
// credential. That is the only meaning an empty field has here: the request
// never carries `"value": ""`, which the server refuses as a different thing.
//
// The value is the one thing in the app that must never be kept, so where it
// lives is the design:
//
//  - In this component's own `ref`, and nowhere else. Not a Pinia store (a
//    store outlives the view), not a prop passed back up, not the URL, not
//    `localStorage` (§4.2 allows "a form field the user is currently typing
//    into", and nothing more). It is cleared on a successful save and on
//    unmount, so navigating away drops it and coming back finds it empty.
//  - It is never pre-filled. There is nothing to fill it from — no route
//    returns a value — and on edit it opens empty and says so (§2.5).
//  - A `<textarea>`, not an `<input>`. An input's value sanitization strips
//    newlines silently, so a pasted PEM would be stored joined into one line
//    and look like it worked. A textarea keeps the newline, and the check
//    below refuses it with the server's own sentence: the operator learns
//    that a multi-line credential goes in as base64.
//  - `spellcheck`, autocorrect, autocapitalize and autocomplete are all off:
//    a browser's spelling service is a network request carrying the text,
//    and form restoration is how a reload would bring the value back.
//  - No `<form>` element and no `name` attributes. A native submission is a
//    GET of the named fields into the URL — history, the access log — and
//    §8 forbids one anyway; with neither, there is nothing it could carry.
//
// Every check here is the server's, mirrored (lib/secretRules.ts), with the
// same code and detail, rendered through the same lookup as the server's
// refusal. The server still checks everything; its refusal lands on the same
// field, in the same words.
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ApiError } from '../../api/client'
import { describeError, sentenceFor } from '../../api/messages'
import {
  MAX_DESCRIPTION_LEN, MAX_REACH_LEN, checkDescription, checkName, checkReach, checkValue, type SecretRefusal,
} from '../../lib/secretRules'
import { putKey, useSecretsStore, type PutOutcome } from '../../stores/secrets'
import { useStreamStore } from '../../stores/stream'
import { useSessionStore } from '../../stores/session'

const props = defineProps<{
  mode: 'create' | 'edit'
  /** On edit: the secret's name and its current prose. Never a value: there is none to give. */
  initial?: { name: string; reach: string; description: string }
}>()

const emit = defineEmits<{
  saved: [name: string, outcome: PutOutcome]
  cancel: []
}>()

const secrets = useSecretsStore()
const stream = useStreamStore()
const session = useSessionStore()

// Form drafts (§4.2's third kind of local state). The reach and description
// start from the secret's — they are metadata, shown in the list anyway.
const name = ref(props.initial?.name ?? '')
const value = ref('')
const reach = ref(props.initial?.reach ?? '')
const description = ref(props.initial?.description ?? '')

type Field = 'name' | 'value' | 'reach' | 'description' | 'form'
const errors = ref<Partial<Record<Field, string>>>({})
const nameTouched = ref(false)

// A refusal stands until its field is edited; then the live check (or the
// next save) is what speaks.
function clear(f: Field): void {
  if (errors.value[f] === undefined) return
  const next = { ...errors.value }
  delete next[f]
  errors.value = next
}
watch(name, () => clear('name'))
watch(value, () => clear('value'))
watch(reach, () => clear('reach'))
watch(description, () => clear('description'))

const say = (r: SecretRefusal) => sentenceFor(r.code, r.detail) ?? r.code

// At keystroke time (§6.4): a reserved name says why as it is typed, and a
// pasted newline is refused before anything is sent. An invalid name waits
// for the field to be left — "not starting with a digit" while typing the
// first letter is noise.
const liveName = computed(() => {
  if (props.mode !== 'create' || name.value === '') return null
  const r = checkName(name.value)
  if (r === null) return null
  return r.code === 'secret_name_reserved' || nameTouched.value ? say(r) : null
})
const liveValue = computed(() => {
  if (value.value === '') return null
  const r = checkValue(value.value)
  return r === null ? null : say(r)
})

const flight = computed(() => stream.inFlight[putKey(name.value)] ?? null)

/** Editing with the value field empty: keep the stored value, send no `value`. */
const keepValue = computed(() => props.mode === 'edit' && value.value === '')

function fieldFor(code: string): Field {
  if (code.startsWith('secret_name_')) return 'name'
  if (code.startsWith('secret_value_')) return 'value'
  if (code.startsWith('secret_reach_')) return 'reach'
  if (code.startsWith('secret_description_')) return 'description'
  return 'form'
}

async function save(): Promise<void> {
  if (flight.value !== null) return
  nameTouched.value = true
  // The server's order: name, value, reach, description — but every field
  // reports at once, so one save shows everything to fix.
  const found: Partial<Record<Field, string>> = {}
  const checks: Array<[Field, SecretRefusal | null]> = [
    ['name', checkName(name.value)],
    ['value', keepValue.value ? null : checkValue(value.value)],
    ['reach', checkReach(reach.value)],
    ['description', checkDescription(description.value)],
  ]
  for (const [f, r] of checks) if (r !== null) found[f] = say(r)
  errors.value = found
  if (Object.keys(found).length > 0) return

  try {
    const outcome = await secrets.put(name.value, keepValue.value ? null : value.value, reach.value, description.value)
    value.value = ''
    emit('saved', name.value, outcome)
  } catch (e) {
    if (session.status !== 'signed-in') return // the 401 path has it
    const code = e instanceof ApiError ? e.code : ''
    errors.value = { [fieldFor(code)]: describeError(e) }
  }
}

onBeforeUnmount(() => {
  value.value = ''
})
</script>

<template>
  <div class="form" role="group" :aria-label="mode === 'create' ? 'New secret' : 'Edit the secret'" data-test="secret-form">
    <h2 v-if="mode === 'edit'">Edit {{ name }}</h2>

    <div v-if="mode === 'create'" class="field" :class="{ err: errors.name || liveName }">
      <label for="secret-name">Name</label>
      <input
        id="secret-name" v-model.trim="name" type="text" data-test="name"
        autocomplete="off" autocapitalize="characters" autocorrect="off" spellcheck="false"
        :maxlength="200" aria-describedby="secret-name-help" @blur="nameTouched = true"
      >
      <p id="secret-name-help" class="help">
        Also the environment variable's name, so capital letters, digits and underscores.
      </p>
      <p v-if="errors.name || liveName" class="msg bad" role="alert" data-test="name-error">
        <span class="glyph" aria-hidden="true">×</span><span>{{ errors.name ?? liveName }}</span>
      </p>
    </div>

    <div class="field" :class="{ err: errors.value || liveValue }">
      <label for="secret-value">{{ mode === 'create' ? 'Value' : 'New value (optional)' }}</label>
      <textarea
        id="secret-value" v-model="value" rows="2" class="value" data-test="value"
        autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false"
        data-1p-ignore data-lpignore="true" aria-describedby="secret-value-help"
      />
      <p id="secret-value-help" class="help" data-test="value-help">
        <template v-if="mode === 'edit'">
          The current value is not shown. Leave this empty to keep it and change only what is written below;
          a value entered here replaces it.
        </template>
        <template v-else>
          Write-only: once saved, Drydock never shows it again, here or anywhere. One line; a multi-line
          credential, such as a PEM, goes in as base64.
        </template>
      </p>
      <p v-if="errors.value || liveValue" class="msg bad" role="alert" data-test="value-error">
        <span class="glyph" aria-hidden="true">×</span><span>{{ errors.value ?? liveValue }}</span>
      </p>
    </div>

    <div class="field reach" :class="{ err: errors.reach }">
      <!-- §6.4: the visible label is the literal question, never a placeholder. -->
      <label for="secret-reach" data-test="reach-label">What can someone do with this?</label>
      <textarea
        id="secret-reach" v-model="reach" rows="3" data-test="reach" :maxlength="MAX_REACH_LEN"
        aria-describedby="secret-rules"
      />
      <p v-if="errors.reach" class="msg bad" role="alert" data-test="reach-error">
        <span class="glyph" aria-hidden="true">×</span><span>{{ errors.reach }}</span>
      </p>
      <!-- §10.4's rules of thumb, at the moment of the decision (§6.4). -->
      <div id="secret-rules" class="rules" data-test="rules">
        <p>
          Everything running in a workspace this reaches can read it — including Claude, which reads
          untrusted text as part of its job. Before saving:
        </p>
        <ol>
          <li>Prefer a credential to a disposable thing over a scoped credential to a real thing.</li>
          <li>Prefer the sandbox account: a test key, a staging tenant, a throwaway project.</li>
          <li>Never store one that can spend money, delete data, or reach production.</li>
          <li>Grant it to one repository, not to all.</li>
        </ol>
      </div>
    </div>

    <div class="field" :class="{ err: errors.description }">
      <label for="secret-description">Where it came from, and how to rotate it</label>
      <textarea
        id="secret-description" v-model="description" rows="2" data-test="description"
        :maxlength="MAX_DESCRIPTION_LEN"
      />
      <p class="help">Optional. Written now, read at 2am in six months.</p>
      <p v-if="errors.description" class="msg bad" role="alert" data-test="description-error">
        <span class="glyph" aria-hidden="true">×</span><span>{{ errors.description }}</span>
      </p>
    </div>

    <div v-if="errors.form" class="msg bad" role="alert" data-test="form-error">
      <span class="glyph" aria-hidden="true">×</span><span>{{ errors.form }}</span>
    </div>

    <div class="act">
      <button
        type="button" class="btn primary" data-test="save"
        :disabled="flight !== null" :aria-busy="flight !== null" @click="save"
      >
        <span v-if="flight" class="spinner" aria-hidden="true" />
        {{ mode === 'create' ? 'Save secret' : keepValue ? 'Save changes' : 'Replace value' }}
      </button>
      <button type="button" class="btn ghost" data-test="cancel" :disabled="flight !== null" @click="emit('cancel')">
        Cancel
      </button>
    </div>
  </div>
</template>

<style scoped>
.form {
  display: flex; flex-direction: column; gap: 16px;
  background: var(--surface); border: 1px solid var(--line); border-radius: var(--r); padding: 14px;
}
.help { font-size: 12.5px; color: var(--ink-3); }
/* Masked where the engine supports it; the field is write-only either way. */
.value { font-family: var(--mono); -webkit-text-security: disc; }
.rules {
  font-size: 12.5px; color: var(--ink-2); background: var(--surface-2);
  border-radius: var(--r); padding: 9px 11px; display: flex; flex-direction: column; gap: 4px;
}
.rules ol { margin: 0; padding-left: 20px; display: flex; flex-direction: column; gap: 2px; }
.act { display: flex; gap: 8px; flex-wrap: wrap; }
.spinner {
  width: 12px; height: 12px; border-radius: 50%;
  border: 2px solid var(--line); border-top-color: var(--ink-2);
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
