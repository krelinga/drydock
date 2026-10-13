// The ports the pure layer talks through (vscode-extension §12.2). Every
// effect the extension has is one of these, implemented by src/adapter (VS
// Code) and src/node (Node's fetch), and replaced by a fake in a spec. The
// pure layer is compiled with no ambient types (tsconfig.pure.json), so it
// cannot reach around them.

export type { Clock } from '../../web/src/core/marks'

/** A response's headers, as much as the client reads. */
export interface Headers {
  get(name: string): string | null
}

export interface HttpRequest {
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  /** Absolute: the configured origin and a path the client checked. */
  url: string
  headers: Record<string, string>
  body?: string
}

export interface HttpResponse {
  status: number
  headers: Headers
  text(): Promise<string>
}

export interface StreamResponse {
  status: number
  headers: Headers
  /** The body as decoded text, chunk by chunk, as it arrives. Ends when the server closes. */
  chunks: AsyncIterable<string>
  /** Stops reading and closes the connection. Idempotent. */
  cancel(): void
}

/**
 * HTTP, and nothing else. A transport never follows a redirect (a 3xx is
 * returned as itself), never sends a credential it was not handed in
 * `headers`, and rejects only when no response arrived.
 */
export interface Transport {
  request(req: HttpRequest): Promise<HttpResponse>
  stream(req: HttpRequest): Promise<StreamResponse>
}

/** The extension's one stored secret (VS Code's SecretStorage, under one key). */
export interface Secrets {
  get(): Promise<string | undefined>
  store(value: string): Promise<void>
  delete(): Promise<void>
}

export type Level = 'info' | 'warning' | 'error'

/** What the operator is asked and told. Nothing here takes focus unasked. */
export interface Prompter {
  /** A notification; resolves to the action pressed, or undefined when dismissed. */
  message(level: Level, text: string, actions?: readonly string[]): Promise<string | undefined>
  /** An input box. Resolves to undefined on Escape or focus loss. */
  input(opts: { prompt: string; password?: boolean; ignoreFocusOut?: boolean; placeHolder?: string }): Promise<string | undefined>
}

/** The window this instance runs in (§8 rule 1). */
export interface Window {
  /** `vscode.env.remoteName`: undefined in a local window. */
  remoteName(): string | undefined
}

/** Hands a URL to the operating system (the browser). */
export interface Opener {
  openExternal(url: string): Promise<void>
}

/** The extension's output channel. Every line passes `redact` first (log.ts). */
export interface Log {
  info(line: string): void
  warn(line: string): void
}

/** The settings the extension reads, at the moment it reads them. */
export interface Settings {
  /** `drydock.url` as the operator wrote it. */
  url(): string
}
