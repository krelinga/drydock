// Signed-in-ness and the device list (frontend §4.1).
//
// Signed-in-ness is derived from traffic, never stored anywhere the browser
// keeps (§2.2): `unknown` until GET /api/auth/session answers, `signed-in`
// when it does, `signed-out` after any 401. Nothing here survives a reload.

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { Device, SessionInfo } from '../api/types'

export type SessionStatus = 'unknown' | 'signed-in' | 'signed-out'

export const useSessionStore = defineStore('session', {
  state: () => ({
    status: 'unknown' as SessionStatus,
    current: '',
    devices: [] as Device[],
    /** Set when the boot probe failed for a reason other than a 401. */
    probeError: null as api.ApiError | null,
  }),
  actions: {
    /** The boot probe, and the device list's refresh. A 401 is handled by the client. */
    async load(): Promise<void> {
      const s = await api.get<SessionInfo>('/api/auth/session')
      this.current = s.current
      this.devices = s.devices
      this.status = 'signed-in'
    },
    async signIn(password: string): Promise<void> {
      await api.send('POST', '/api/auth/session', { password })
      await this.load()
    },
    /** Ends this session, or every session. The caller routes to /signin. */
    async signOut(everywhere: boolean): Promise<void> {
      await api.send('DELETE', everywhere ? '/api/auth/session?all=true' : '/api/auth/session')
    },
  },
})
