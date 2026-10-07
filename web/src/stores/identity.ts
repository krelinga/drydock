// The Claude identity store (frontend §4.1, §6.6): a read model over the
// stream store's one identity field, the fetch that feeds it, and "check now".
//
// It writes no entity itself. GET /api/auth/claude is handed to the reducer
// as a snapshot tagged with the stream position it was asked at; the verdict
// after that arrives as auth.identity, a failed check as
// auth.identity_check_failed. POST /api/auth/claude/check answers 202 and its
// body is discarded (§2.1): the check's outcome is the event.
//
// The body's `login` — the handshake (design §7.2) — goes to the reducer with
// it, and auth.login carries every phase after. The three login actions
// below send, and wait for the event that ends what they asked; none applies
// a response, and the code passes through `submitCode` into a request body
// and nowhere else.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import { LOGIN_ENDED, type ClaudeIdentityBody, type StreamEvent } from '../api/types'
import type { ClaudeIdentity } from './reducer'
import { useStreamStore } from './stream'

export const CHECK_KEY = 'claude:check'
export const LOGIN_BEGIN_KEY = 'claude:login:begin'
export const LOGIN_CODE_KEY = 'claude:login:code'
export const LOGIN_CANCEL_KEY = 'claude:login:cancel'

/** The phase an auth.login event carries, or null. */
function loginPhase(ev: StreamEvent): string | null {
  const l = (ev.data ?? {}).login as { phase?: unknown } | undefined
  return l !== undefined && l !== null && typeof l.phase === 'string' ? l.phase : null
}

let inFlight: Promise<void> | null = null
let again = false

export const useIdentityStore = defineStore('identity', {
  state: () => ({
    status: 'idle' as 'idle' | 'loading' | 'ready' | 'error',
    error: null as api.ApiError | null,
    /** The expiring banner put away, for this countdown only: the expires_at it was put away at. */
    dismissedFor: null as string | null,
  }),
  getters: {
    identity(): ClaudeIdentity | null {
      return useStreamStore().entities.identity
    },
  },
  actions: {
    /** GET /api/auth/claude into the reducer; joined like the other loads. */
    load(): Promise<void> {
      if (inFlight !== null) {
        again = true
        return inFlight
      }
      inFlight = (async () => {
        try {
          do {
            again = false
            await this.fetchOnce()
          } while (again)
        } finally {
          inFlight = null
        }
      })()
      return inFlight
    },

    /** @internal */
    async fetchOnce(): Promise<void> {
      const stream = useStreamStore()
      const { at, tick } = stream.snapshotTag()
      if (this.status !== 'ready') this.status = 'loading'
      try {
        const view = await api.get<ClaudeIdentityBody>('/api/auth/claude')
        stream.snapshot({ type: 'identity', at, view }, tick)
        this.status = 'ready'
        this.error = null
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        this.error = err
        if (this.status !== 'ready') this.status = 'error'
      }
    },

    /**
     * POST /api/auth/claude/check. In flight until the check's event lands —
     * an auth.identity or auth.identity_check_failed newer than the request —
     * or the request is refused.
     */
    async check(): Promise<void> {
      const stream = useStreamStore()
      if (CHECK_KEY in stream.inFlight) return
      const from = stream.lastEventId
      stream.begin(CHECK_KEY, (ev) => ev.id > from && ev.kind.startsWith('auth.identity'))
      try {
        await api.send('POST', '/api/auth/claude/check')
      } catch (e) {
        stream.end(CHECK_KEY)
        throw e
      }
    },

    /**
     * POST /api/auth/claude/login. The 202's `login_id` is not read (§4.2
     * step 3): the login, id and all, arrives as auth.login, and a reload
     * finds it in GET /api/auth/claude. In flight until the login is past
     * `starting` — its URL is up, or it ended — never on the receipt.
     */
    async beginLogin(): Promise<void> {
      const stream = useStreamStore()
      if (LOGIN_BEGIN_KEY in stream.inFlight) return
      const from = stream.lastEventId
      stream.begin(LOGIN_BEGIN_KEY, (ev) => ev.id > from && ev.kind === 'auth.login' && loginPhase(ev) !== 'starting')
      try {
        await api.send('POST', '/api/auth/claude/login')
      } catch (e) {
        stream.end(LOGIN_BEGIN_KEY)
        throw e
      }
    },

    /**
     * POST the pasted code. The caller has already taken it out of its field
     * and cleared the field: this function holds it only as long as the
     * request does, and it goes into nothing but the request body — no
     * store, no storage, no URL, no in-flight key (frontend §2.4, §6.2). In
     * flight until the verdict: an auth.login past `submitting`.
     */
    async submitCode(loginId: string, code: string): Promise<void> {
      const stream = useStreamStore()
      if (LOGIN_CODE_KEY in stream.inFlight) return
      const from = stream.lastEventId
      stream.begin(LOGIN_CODE_KEY, (ev) => {
        const p = loginPhase(ev)
        return ev.id > from && ev.kind === 'auth.login' && p !== null && p !== 'submitting' && p !== 'awaiting_code'
      })
      try {
        await api.send('POST', `/api/auth/claude/login/${encodeURIComponent(loginId)}/code`, { code })
      } catch (e) {
        stream.end(LOGIN_CODE_KEY)
        throw e
      }
    },

    /** DELETE the login. In flight until it is announced over. */
    async cancelLogin(loginId: string): Promise<void> {
      const stream = useStreamStore()
      if (LOGIN_CANCEL_KEY in stream.inFlight) return
      const from = stream.lastEventId
      stream.begin(LOGIN_CANCEL_KEY, (ev) => {
        const p = loginPhase(ev)
        return ev.id > from && ev.kind === 'auth.login' && p !== null && (LOGIN_ENDED as readonly string[]).includes(p)
      })
      try {
        await api.send('DELETE', `/api/auth/claude/login/${encodeURIComponent(loginId)}`)
      } catch (e) {
        stream.end(LOGIN_CANCEL_KEY)
        throw e
      }
    },

    dismiss(): void {
      this.dismissedFor = this.identity?.expiresAt ?? ''
    },
  },
})
