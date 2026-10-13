// The error a request to Drydock's API ends in, shared by every client
// (vscode-extension §5.1): the web UI's api/client.ts and the extension's
// HTTP client each build one from the envelope, and core/messages.ts turns
// its code into the sentence shown. Never built from the server's prose.

import type { ErrorCode } from './types'

export class ApiError extends Error {
  constructor(
    /** HTTP status, or 0 when no response arrived. */
    readonly status: number,
    /** The envelope's code, or `network` / `unparseable`. */
    readonly code: ErrorCode | (string & {}),
    /** Seconds from a `Retry-After` header, when one was sent as seconds. */
    readonly retryAfter: number | null = null,
    serverMessage = '',
    /**
     * The envelope's `detail`: which rule, which character. Shown as text for
     * the few codes whose sentence needs it (messages.ts), never parsed.
     */
    readonly detail = '',
  ) {
    super(serverMessage || `${status} ${code}`)
    this.name = 'ApiError'
  }
}
