// The output channel's one rule: nothing that looks like a credential reaches
// it (vscode-extension §12.8). The extension never passes the token to the
// log on purpose; this is the belt for an error message or a body that
// quotes one. A Drydock token, a bearer header's value and a GitHub token are
// masked wherever they appear.

import type { Log } from './ports'

const PATTERNS: readonly RegExp[] = [
  /drydock_[A-Za-z0-9_-]{8,}/g,
  /(Bearer\s+)\S+/gi,
  /\bgh[pousr]_[A-Za-z0-9]{20,}/g,
  /\bgithub_pat_[A-Za-z0-9_]{20,}/g,
]

/** `line` with every credential-shaped run replaced by `[redacted]`. */
export function redact(line: string): string {
  let out = line
  for (const re of PATTERNS) {
    out = out.replace(re, (_m, bearer?: string) => (typeof bearer === 'string' ? `${bearer}[redacted]` : '[redacted]'))
  }
  return out
}

/** A Log that redacts every line before `sink` sees it. */
export function redacting(sink: Log): Log {
  return { info: (l) => sink.info(redact(l)), warn: (l) => sink.warn(redact(l)) }
}
