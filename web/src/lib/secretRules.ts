// The write-time rules for a secret (design §10.1), checked in the browser
// before anything is sent — the server's own, mirrored from
// internal/secrets/validate.go.
//
// This is not the control. The server refuses every one of these on its own
// and stays the authority; a client-side check is the difference between
// learning why at keystroke time and learning it at save time (frontend
// §6.4). So each check returns exactly what the server would — the same
// envelope code and the same detail text — and the form renders both through
// the one lookup in api/messages.ts. A refusal looks the same whichever side
// caught it.
//
// The reserved list is copied, not generated. secretRules.spec.ts reads
// validate.go itself and fails when the two lists disagree, so a name the
// server learns to refuse cannot quietly be one the form accepts.

export interface SecretRefusal {
  /** The server's envelope code for the same refusal (internal/api/problem.go). */
  code: string
  /** The server's detail for it, character for character: which rule, which character. Never the value. */
  detail: string
}

/** internal/secrets: MaxNameLen, MaxValueLen, MaxReachLen, MaxDescriptionLen. */
export const MAX_NAME_LEN = 128
export const MAX_VALUE_LEN = 32 << 10
export const MAX_REACH_LEN = 2000
export const MAX_DESCRIPTION_LEN = 4000

const NAME_PATTERN = /^[A-Z_][A-Z0-9_]*$/

const REMOTE_CONTROL = 'it disables Remote Control (design §2.1)'
const GH_SHIM = "it would shadow the gh shim's per-call, repository-scoped token (§9.2)"
const RESOLVED = 'everything in the container resolves its configuration against it'
const PARSED = 'it changes how every later shell command is parsed'
const READ_ONLY = 'bash makes it read-only, so exporting it fails the prelude on every command'
const TRUST = 'it changes whom Claude Code trusts'
const PROXY = "it would route the workspace's GitHub tokens and session traffic through another host"
const CERTS = 'it changes which certificates the workspace trusts with its GitHub tokens'

/** validate.go reservedExact: name → why. */
export const RESERVED_EXACT: Readonly<Record<string, string>> = {
  DO_NOT_TRACK: REMOTE_CONTROL,
  GITHUB_TOKEN: GH_SHIM,
  GITHUB_ENTERPRISE_TOKEN: GH_SHIM,
  PATH: 'it decides which program every command runs',
  HOME: RESOLVED, SHELL: RESOLVED, USER: RESOLVED, LOGNAME: RESOLVED, PWD: RESOLVED,
  ENV: PARSED, IFS: PARSED, CDPATH: PARSED, PS4: PARSED, PROMPT_COMMAND: PARSED,
  SHELLOPTS: PARSED, BASHOPTS: PARSED, GLOBIGNORE: PARSED,
  UID: READ_ONLY, EUID: READ_ONLY, PPID: READ_ONLY,
  _: 'the shell sets it itself',
  NODE_OPTIONS: 'it loads code into Claude Code, which is a Node program',
  NODE_EXTRA_CA_CERTS: TRUST,
  NODE_TLS_REJECT_UNAUTHORIZED: TRUST,
  HTTP_PROXY: PROXY, HTTPS_PROXY: PROXY, ALL_PROXY: PROXY, NO_PROXY: PROXY,
  SSL_CERT_FILE: CERTS, SSL_CERT_DIR: CERTS, CURL_CA_BUNDLE: CERTS,
}

/** validate.go reservedPrefix, in its order: the first match is the reason given. */
export const RESERVED_PREFIX: ReadonlyArray<readonly [prefix: string, why: string]> = [
  ['ANTHROPIC_', 'Claude Code reads ANTHROPIC_* itself; ANTHROPIC_BASE_URL disables Remote Control (§2.1) and ANTHROPIC_API_KEY replaces its login (§7)'],
  ['CLAUDE_', 'Claude Code reads CLAUDE_* itself: the config directory, this delivery mechanism, its login (§7, §10.3)'],
  ['DISABLE_', 'Claude Code reads DISABLE_* itself; DISABLE_TELEMETRY and DISABLE_GROWTHBOOK disable Remote Control (§2.1), DISABLE_AUTOUPDATER holds the version pin (§11)'],
  ['GH_', "gh reads GH_* itself; GH_TOKEN would shadow the shim's repository-scoped token (§9.2)"],
  ['GIT_', 'git reads GIT_* itself; GIT_CONFIG_* can replace the credential helper and the branch guard (§9.2, §11)'],
  ['DRYDOCK_', "Drydock's own clients read DRYDOCK_* to find the broker (§9)"],
  ['LD_', 'the dynamic loader reads LD_* and loads code into every process'],
  ['BASH_', 'bash reads BASH_* itself; BASH_ENV runs a file before every command'],
]

/** validate.go Reserved: why `name` is reserved, or null. */
export function reservedReason(name: string): string | null {
  const exact = RESERVED_EXACT[name]
  if (exact !== undefined) return exact
  for (const [prefix, why] of RESERVED_PREFIX) if (name.startsWith(prefix)) return why
  return null
}

/** validate.go ValidateName. */
export function checkName(name: string): SecretRefusal | null {
  if (name.length === 0 || name.length > MAX_NAME_LEN || !NAME_PATTERN.test(name)) {
    return {
      code: 'secret_name_invalid',
      detail: `Use capital letters, digits and underscores, not starting with a digit, at most ${MAX_NAME_LEN} characters.`,
    }
  }
  const why = reservedReason(name)
  if (why !== null) return { code: 'secret_name_reserved', detail: `Drydock refuses it because ${why}.` }
  return null
}

const utf8 = new TextEncoder()
const byteLength = (s: string) => utf8.encode(s).length

/** Unicode category Cc, which is what Go's unicode.IsControl means: C0, DEL and C1. */
function isControl(cp: number): boolean {
  return cp <= 0x1f || (cp >= 0x7f && cp <= 0x9f)
}

function charName(cp: number): string {
  switch (cp) {
    case 0x0a: return 'a newline (U+000A)'
    case 0x0d: return 'a carriage return (U+000D)'
    case 0x00: return 'a NUL (U+0000)'
    case 0x09: return 'a tab (U+0009)'
  }
  return `the control character U+${cp.toString(16).toUpperCase().padStart(4, '0')}`
}

/**
 * validate.go ValidateValue: not empty, at most 32 KiB, and no control
 * character — a newline above all, because one forges a second line in the
 * GET-SECRETS answer (§10.3). The detail names the character and its UTF-8
 * byte offset, as the server's does, and never the value.
 */
export function checkValue(value: string): SecretRefusal | null {
  if (value === '') {
    return { code: 'secret_value_empty', detail: 'An empty value cannot be told apart from an unset variable.' }
  }
  if (byteLength(value) > MAX_VALUE_LEN) {
    return {
      code: 'secret_value_too_long',
      detail: `A value is at most ${MAX_VALUE_LEN} bytes. Encode a large or multi-line credential, such as a PEM, as base64.`,
    }
  }
  let offset = 0
  for (const ch of value) {
    const cp = ch.codePointAt(0)!
    if (isControl(cp)) {
      return {
        code: 'secret_value_control_character',
        detail: `It contains ${charName(cp)} at byte ${offset}. A multi-line credential, such as a PEM, goes in as base64.`,
      }
    }
    offset += byteLength(ch)
  }
  return null
}

/** validate.go ValidateReach: the required sentence (§10.4). */
export function checkReach(reach: string): SecretRefusal | null {
  if (reach.trim() === '') {
    return { code: 'secret_reach_required', detail: 'The reach field is required: it is the decision to grant, written down.' }
  }
  if (byteLength(reach) > MAX_REACH_LEN) {
    return { code: 'secret_reach_required', detail: `At most ${MAX_REACH_LEN} bytes.` }
  }
  return null
}

/** validate.go validateDescription. */
export function checkDescription(description: string): SecretRefusal | null {
  if (byteLength(description) > MAX_DESCRIPTION_LEN) {
    return { code: 'secret_description_invalid', detail: `At most ${MAX_DESCRIPTION_LEN} bytes of UTF-8.` }
  }
  return null
}
