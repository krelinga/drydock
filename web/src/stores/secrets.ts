// The secrets store (frontend §4.1): a read model over the stream store's
// secret entities, the fetch that feeds them, and the three writes.
//
// It writes no entity itself. GET /api/secrets is handed to the reducer as a
// snapshot tagged with the stream position it was asked at; everything after
// that arrives as secret.* events. The writes answer 200 or 204, not 202 —
// nothing about them is slow — so a write's in-flight mark ends when its
// response lands: the response *is* the outcome. What the write did to the
// secret still renders only from its event (reducer.ts explains why the 200
// body is not applied).
//
// No value is ever held here. `put` takes one as an argument, hands it to the
// request, and returns only the operation's result — created, rotated, and
// which running workspaces are now stale — with the body's metadata dropped
// on the floor. The form that typed the value owns it, in a component-local
// ref, and clears it (views/secrets/SecretForm.vue).

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { PutSecretResult, SecretList, Stale } from '../api/types'
import { secretList, type Secret } from './reducer'
import { useStreamStore } from './stream'

export type SecretsStatus = 'idle' | 'loading' | 'ready' | 'not_configured' | 'error'

/** What a PUT tells the screen that sent it. Deliberately not the secret. */
export interface PutOutcome {
  created: boolean
  rotated: boolean
  /** Whether this save sent a value. False: the stored one was kept, and only the prose changed. */
  valueSent: boolean
  stale: Stale
}

export const putKey = (name: string) => `secret:${name}:put`
export const grantsKey = (name: string) => `secret:${name}:grants`
export const deleteKey = (name: string) => `secret:${name}:delete`

const path = (name: string) => `/api/secrets/${encodeURIComponent(name)}`

let inFlight: Promise<void> | null = null
let again = false

export const useSecretsStore = defineStore('secrets', {
  state: () => ({
    status: 'idle' as SecretsStatus,
    error: null as api.ApiError | null,
  }),
  getters: {
    list(): Secret[] {
      return secretList(useStreamStore().entities)
    },
    loaded(): boolean {
      return useStreamStore().entities.secretsLoaded
    },
  },
  actions: {
    /** The secret by name, or undefined. */
    byName(name: string): Secret | undefined {
      return useStreamStore().entities.secrets[name]
    },

    /** GET /api/secrets into the reducer; joined like the catalog's load. */
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
        const view = await api.get<SecretList>('/api/secrets')
        stream.dispatch({ type: 'secrets', at, view })
        this.status = 'ready'
        this.error = null
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        if (err.code === 'secrets_not_configured') {
          this.status = 'not_configured'
          this.error = null
          return
        }
        this.error = err
        if (this.status !== 'ready') this.status = 'error'
      }
    },

    /**
     * PUT /api/secrets/:name: create, rotate, or change the reach and
     * description. Returns what the operation did; throws the refusal.
     *
     * `value` null sends no `value` key at all, which the server reads as
     * "keep the stored one" (frontend §4.5 #13): changing the prose never
     * handles the credential. That is not the same request as an empty
     * string, which the server refuses — so null is the only spelling of
     * "keep", and the key is absent rather than null or "".
     */
    async put(name: string, value: string | null, reach: string, description: string): Promise<PutOutcome> {
      const stream = useStreamStore()
      const key = putKey(name)
      if (key in stream.inFlight) throw new api.ApiError(409, 'in_progress')
      stream.begin(key, () => false)
      try {
        const body = value === null ? { reach, description } : { value, reach, description }
        const res = await api.sendForResult<PutSecretResult>('PUT', path(name), body)
        // The metadata in `res.secret` is not applied: its event is.
        return {
          created: res.created === true,
          rotated: res.rotated === true,
          valueSent: value !== null,
          stale: {
            new_commands: res.stale?.new_commands ?? [],
            needs_supervisor_restart: res.stale?.needs_supervisor_restart ?? [],
          },
        }
      } finally {
        stream.end(key)
      }
    },

    /** PUT /api/secrets/:name/grants. Not a rotation: nothing becomes stale (§6.4). */
    async setGrants(name: string, repositoryIds: number[], allRepos: boolean): Promise<void> {
      const stream = useStreamStore()
      const key = grantsKey(name)
      if (key in stream.inFlight) return
      stream.begin(key, () => false)
      try {
        await api.send('PUT', `${path(name)}/grants`, { repository_ids: repositoryIds, all_repos: allRepos })
      } finally {
        stream.end(key)
      }
    },

    /** DELETE /api/secrets/:name: the value and every grant. */
    async remove(name: string): Promise<void> {
      const stream = useStreamStore()
      const key = deleteKey(name)
      if (key in stream.inFlight) return
      stream.begin(key, () => false)
      try {
        await api.send('DELETE', path(name))
      } finally {
        stream.end(key)
      }
    },
  },
})
