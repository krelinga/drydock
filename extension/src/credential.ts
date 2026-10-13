// The extension's one credential (vscode-extension §4.2): an API token
// minted in Drydock's Settings, pasted once, and stored in SecretStorage as
// `{origin, token}` — the token and the exact origin it was verified
// against. It is attached only to requests for that origin (client.ts), so a
// `drydock.url` that no longer equals it means signing in again, never the
// old token sent somewhere new.
//
// The token is never logged (log.ts masks its shape anyway), never put in a
// setting, a file or an argument, and never shown back.

import type { Secrets } from './ports'

/** `drydock_` and 43 base64url characters: 32 random bytes (internal/auth). */
export const TOKEN_SHAPE = /^drydock_[A-Za-z0-9_-]{43}$/

export interface Credential {
  origin: string
  token: string
}

export type PasteResult = { ok: true; token: string } | { ok: false; sentence: string }

/**
 * A pasted token, trimmed of surrounding whitespace and nothing else (a
 * terminal copy can carry a newline), checked for shape before anything is
 * sent with it.
 */
export function checkPaste(raw: string): PasteResult {
  const token = raw.trim()
  if (TOKEN_SHAPE.test(token)) return { ok: true, token }
  if (token === '') return { ok: false, sentence: 'Nothing was pasted.' }
  // Says what a token looks like, never what was pasted: it may be some
  // other secret pasted into the wrong box.
  return {
    ok: false,
    sentence: "That is not a Drydock token. A token starts with drydock_ and is shown once, when it is created in Drydock's Settings.",
  }
}

/** The stored entry, or null when it is absent or not one this extension wrote. */
export function parseStored(raw: string | undefined): Credential | null {
  if (raw === undefined) return null
  let v: unknown
  try {
    v = JSON.parse(raw)
  } catch {
    return null
  }
  if (v === null || typeof v !== 'object') return null
  const o = v as Record<string, unknown>
  if (typeof o.origin !== 'string' || typeof o.token !== 'string' || !TOKEN_SHAPE.test(o.token)) return null
  return { origin: o.origin, token: o.token }
}

/** Reads and writes the credential through SecretStorage, and nowhere else. */
export class CredentialStore {
  constructor(private readonly secrets: Secrets) {}

  async read(): Promise<Credential | null> {
    return parseStored(await this.secrets.get())
  }

  /** The token to send to `origin`, or null: only a credential stored for exactly that origin. */
  async tokenFor(origin: string): Promise<string | null> {
    const c = await this.read()
    return c !== null && c.origin === origin ? c.token : null
  }

  async write(c: Credential): Promise<void> {
    await this.secrets.store(JSON.stringify({ origin: c.origin, token: c.token }))
  }

  async clear(): Promise<void> {
    await this.secrets.delete()
  }
}
