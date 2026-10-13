// The API token store (vscode-extension §4.2): a read model over the stream
// store's token entities, the fetch that feeds them, and the two writes.
//
// It writes no entity itself. GET /api/tokens is handed to the reducer as a
// snapshot tagged with the stream position it was asked at; everything after
// that arrives as api_token.* events.
//
// No token is ever held here. `create` returns the mint's token to the screen
// that asked — the one response in the API that carries a credential — and
// keeps nothing of it: the metadata in the same body is dropped, since its
// event carries it, and the screen shows the token once from a component-local
// ref and clears it (components/ApiTokens.vue). The mint is a 201 whose body
// is the result, so its mark ends when the response lands, as a secret PUT's
// does. A revoke is a 202, in flight until its api_token.revoked.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { MintedToken, TokenList, TokenScope } from '../core/types'
import { tokenList, type ApiToken } from '../core/reducer'
import { useStreamStore } from './stream'

export const CREATE_TOKEN_KEY = 'token:create'
export const revokeTokenKey = (id: string) => `token:${id}:revoke`

/** The lifetimes Settings offers, in days; the server takes 1 to 365. */
export const TOKEN_LIFETIMES = [30, 90, 180, 365] as const
export const DEFAULT_TOKEN_DAYS = 90

/** What a mint tells the screen that asked: the token, to show once. Nothing else. */
export interface Minted {
  id: string
  token: string
}

let inFlight: Promise<void> | null = null
let again = false

export const useTokensStore = defineStore('tokens', {
  state: () => ({
    error: null as api.ApiError | null,
  }),
  getters: {
    list(): ApiToken[] {
      return tokenList(useStreamStore().entities)
    },
    loaded(): boolean {
      return useStreamStore().entities.tokensLoaded
    },
  },
  actions: {
    /** GET /api/tokens into the reducer; joined like the secrets' load. */
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
      try {
        const view = await api.get<TokenList>('/api/tokens')
        stream.snapshot({ type: 'tokens', at, view }, tick)
        this.error = null
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        this.error = err
      }
    },

    /**
     * POST /api/tokens. Returns the token for its one display; throws the
     * refusal. The list's row arrives as api_token.created.
     */
    async create(label: string, scopes: TokenScope[], days: number): Promise<Minted> {
      const stream = useStreamStore()
      if (CREATE_TOKEN_KEY in stream.inFlight) throw new api.ApiError(409, 'in_progress')
      stream.begin(CREATE_TOKEN_KEY, () => false)
      try {
        const res = await api.sendForResult<MintedToken>('POST', '/api/tokens', { label, scopes, expires_in_days: days })
        if (typeof res.token !== 'string' || typeof res.id !== 'string') throw new api.ApiError(201, 'unparseable')
        return { id: res.id, token: res.token }
      } finally {
        stream.end(CREATE_TOKEN_KEY)
      }
    },

    /**
     * DELETE /api/tokens/:id. In flight until its api_token.revoked, or a
     * snapshot showing it revoked — never on the 202.
     */
    async revoke(id: string): Promise<void> {
      const stream = useStreamStore()
      const key = revokeTokenKey(id)
      if (key in stream.inFlight) return
      const from = stream.lastEventId
      stream.begin(
        key,
        (ev) => ev.id > from && ev.kind === 'api_token.revoked' && (ev.data?.token as { id?: unknown } | undefined)?.id === id,
        (e) => e.tokens[id]?.revokedAt != null,
      )
      try {
        await api.send('DELETE', `/api/tokens/${encodeURIComponent(id)}`)
        stream.accepted(key)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },
  },
})
