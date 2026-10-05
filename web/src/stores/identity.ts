// The Claude identity store (frontend §4.1, §6.6): a read model over the
// stream store's one identity field, the fetch that feeds it, and "check now".
//
// It writes no entity itself. GET /api/auth/claude is handed to the reducer
// as a snapshot tagged with the stream position it was asked at; the verdict
// after that arrives as auth.identity, a failed check as
// auth.identity_check_failed. POST /api/auth/claude/check answers 202 and its
// body is discarded (§2.1): the check's outcome is the event.
//
// `login` — the in-flight handshake — is in the body and is not read: it is
// always null until the handshake is built (SEAM, lib/identity.ts).

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { ClaudeIdentityBody } from '../api/types'
import type { ClaudeIdentity } from './reducer'
import { useStreamStore } from './stream'

export const CHECK_KEY = 'claude:check'

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
      const at = stream.lastEventId
      if (this.status !== 'ready') this.status = 'loading'
      try {
        const view = await api.get<ClaudeIdentityBody>('/api/auth/claude')
        stream.dispatch({ type: 'identity', at, view })
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

    dismiss(): void {
      this.dismissedFor = this.identity?.expiresAt ?? ''
    },
  },
})
