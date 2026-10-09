// The ports store (frontend §6.3; port forwarding §6, §13 step 4): the fetch
// that feeds a workspace's port entities, and the registry's mutations.
//
// Like the workspaces store it writes no entity itself. GET
// /api/workspaces/:id/ports?hidden=true is handed to the stream store as a
// snapshot tagged with the stream position it was asked at — always with the
// hidden rows, so the list is the whole truth and the panel's "show hidden"
// is a filter, not a second fetch. Every mutation is §4.2: mark in flight,
// send, discard the 202, and let the event that ends it clear the mark — or a
// snapshot showing it over, after a resync gap (`OVER_PORT`, one predicate
// for both paths).

import { defineStore } from 'pinia'
import * as api from '../api/client'
import type { HostHeader, PortList, ProbeResult, StreamEvent } from '../api/types'
import type { Entities, Port } from './reducer'
import { useStreamStore } from './stream'

/** The in-flight key for switching a port's preview on or off. */
export const portEnableKey = (portId: string) => `port:${portId}:enable`
/** The in-flight key for its Host header switch. */
export const portHostKey = (portId: string) => `port:${portId}:host`
/** The in-flight key for hiding or unhiding it. */
export const portHideKey = (portId: string) => `port:${portId}:hide`
/** The in-flight key for removing (retiring) it. */
export const portRetireKey = (portId: string) => `port:${portId}:retire`
/** The in-flight key for adding a port by number to a workspace. */
export const portAddKey = (wsId: string, port: number) => `workspace:${wsId}:port:${port}:add`
/** The in-flight key for asking a workspace's ports to be scanned now. */
export const portRescanKey = (wsId: string) => `workspace:${wsId}:ports:rescan`

/**
 * What one input says about a port: the row an event carries or the entities
 * hold, or that it is gone (retired, or its workspace deleted). Null when the
 * input says nothing about it.
 */
export type PortOutcome = { gone: true } | { gone: false; port: Port }

function eventPort(ev: StreamEvent): Partial<Port> & { id?: string; workspaceId?: string } {
  const p = (ev.data?.port ?? null) as Record<string, unknown> | null
  if (p === null || typeof p !== 'object') return {}
  return {
    id: typeof p.id === 'string' ? p.id : undefined,
    workspaceId: typeof p.workspace_id === 'string' ? p.workspace_id : undefined,
    containerPort: typeof p.container_port === 'number' ? p.container_port : undefined,
    enabled: p.enabled === true,
    hidden: p.hidden === true,
    hostHeader: p.host_header === 'passthrough' ? 'passthrough' : 'localhost',
  }
}

/** What an event says about port `id` of workspace `wsId`. */
export function portEventOutcome(wsId: string, id: string, ev: StreamEvent): PortOutcome | null {
  if (ev.workspace_id !== wsId) return null
  if (ev.kind === 'workspace.gone') return { gone: true }
  if (ev.kind === 'port.retired') return ev.data?.port_id === id ? { gone: true } : null
  if (!ev.kind.startsWith('port.')) return null
  const p = eventPort(ev)
  return p.id === id ? { gone: false, port: p as Port } : null
}

/** What the entities say about it. A port they do not hold is gone: only the list, or port.retired, drops one. */
export function portEntityOutcome(e: Entities, wsId: string, id: string): PortOutcome {
  const p = e.ports[id]
  if (p === undefined || e.gone[wsId] !== undefined) return { gone: true }
  return { gone: false, port: p }
}

/**
 * Each port action's end. The registry writes its one event in the same
 * transaction as the row, so the row's new value is the end — the receipt
 * and the end are one fact here, and a press on a port already in that state
 * (another device's) still ends on the event the server writes for it.
 */
export const OVER_PORT = {
  enabled: (want: boolean) => (o: PortOutcome) => o.gone || o.port.enabled === want,
  hostHeader: (want: HostHeader) => (o: PortOutcome) => o.gone || o.port.hostHeader === want,
  hidden: (want: boolean) => (o: PortOutcome) => o.gone || o.port.hidden === want,
  retired: (o: PortOutcome) => o.gone,
} as const

/** What settles a change to port `id`: an event about it that `over` finds the end. */
export function settlesPort(wsId: string, id: string, over: (o: PortOutcome) => boolean): (ev: StreamEvent) => boolean {
  return (ev) => {
    const o = portEventOutcome(wsId, id, ev)
    return o !== null && over(o)
  }
}

/**
 * What settles an add: the `port.added` for that number on that workspace —
 * the 202's body names the new row, and reading it is what §4.2 forbids — or
 * the workspace going.
 */
export function settlesAdd(wsId: string, port: number): (ev: StreamEvent) => boolean {
  return (ev) => {
    if (ev.workspace_id !== wsId) return false
    if (ev.kind === 'workspace.gone') return true
    return ev.kind === 'port.added' && eventPort(ev).containerPort === port
  }
}

/**
 * What settles a rescan: the port.scanned the scan it asked for writes for
 * that workspace (internal/preview KindPortScanned) — newer than the press,
 * since one is written only for a scan asked about — or the workspace going.
 * No snapshot can show a scan happened, so a rescan whose answer fell in a
 * resync gap ends as any other press does, on its own slow note.
 */
export function settlesRescan(wsId: string, since: number): (ev: StreamEvent) => boolean {
  return (ev) => ev.workspace_id === wsId && ev.id > since && (ev.kind === 'port.scanned' || ev.kind === 'workspace.gone')
}

/** The same, asked of the entities: the workspace lists that port. */
export function addOverIn(wsId: string, port: number): (e: Entities) => boolean {
  return (e) => e.gone[wsId] !== undefined ||
    Object.values(e.ports).some((p) => p.workspaceId === wsId && p.containerPort === port)
}

const path = (wsId: string, portId?: string) =>
  `/api/workspaces/${encodeURIComponent(wsId)}/ports${portId === undefined ? '' : `/${encodeURIComponent(portId)}`}`

export const usePortsStore = defineStore('ports', {
  state: () => ({
    /** Per workspace id: the list fetch's own lifecycle — not entity state. */
    status: {} as Record<string, { status: 'loading' | 'ready' | 'error'; error: api.ApiError | null }>,
  }),
  actions: {
    /** GET …/ports?hidden=true, into the reducer. */
    async load(wsId: string): Promise<void> {
      const stream = useStreamStore()
      const { at, tick } = stream.snapshotTag()
      if (this.status[wsId]?.status !== 'ready') this.status = { ...this.status, [wsId]: { status: 'loading', error: null } }
      try {
        const view = await api.get<PortList>(`${path(wsId)}?hidden=true`)
        stream.snapshot({ type: 'ports', at, workspaceId: wsId, view }, tick)
        this.status = { ...this.status, [wsId]: { status: 'ready', error: null } }
      } catch (e) {
        const err = e instanceof api.ApiError ? e : new api.ApiError(0, 'network')
        if (err.status === 401) return
        const status = this.status[wsId]?.status === 'ready' ? 'ready' : 'error'
        this.status = { ...this.status, [wsId]: { status, error: err } }
      }
    },

    /** POST …/ports: list a port by hand, off. */
    add(wsId: string, port: number, label: string): Promise<void> {
      const body: Record<string, unknown> = { container_port: port }
      if (label.trim() !== '') body.label = label.trim()
      return this.mutate(portAddKey(wsId, port), settlesAdd(wsId, port), addOverIn(wsId, port), 'POST', path(wsId), body)
    },

    /** PATCH …/ports/:port {enabled}: the click that makes a URL live, or ends it. */
    setEnabled(wsId: string, id: string, enabled: boolean): Promise<void> {
      const over = OVER_PORT.enabled(enabled)
      return this.mutate(portEnableKey(id), settlesPort(wsId, id, over), (e) => over(portEntityOutcome(e, wsId, id)),
        'PATCH', path(wsId, id), { enabled })
    },

    /** PATCH …/ports/:port {host_header}. */
    setHostHeader(wsId: string, id: string, hostHeader: HostHeader): Promise<void> {
      const over = OVER_PORT.hostHeader(hostHeader)
      return this.mutate(portHostKey(id), settlesPort(wsId, id, over), (e) => over(portEntityOutcome(e, wsId, id)),
        'PATCH', path(wsId, id), { host_header: hostHeader })
    },

    /** PATCH …/ports/:port {hidden}. */
    setHidden(wsId: string, id: string, hidden: boolean): Promise<void> {
      const over = OVER_PORT.hidden(hidden)
      return this.mutate(portHideKey(id), settlesPort(wsId, id, over), (e) => over(portEntityOutcome(e, wsId, id)),
        'PATCH', path(wsId, id), { hidden })
    },

    /** DELETE …/ports/:port: retire it; its slug is never used again. */
    retire(wsId: string, id: string): Promise<void> {
      return this.mutate(portRetireKey(id), settlesPort(wsId, id, OVER_PORT.retired),
        (e) => OVER_PORT.retired(portEntityOutcome(e, wsId, id)), 'DELETE', path(wsId, id))
    },

    /**
     * POST …/ports/rescan: look at what the container listens on now rather
     * than at the next scan. It changes no switch — what it finds is listed,
     * off — and is settled by port.scanned.
     */
    async rescan(wsId: string): Promise<void> {
      const stream = useStreamStore()
      const key = portRescanKey(wsId)
      if (key in stream.inFlight) return
      stream.begin(key, settlesRescan(wsId, stream.entities.lastEventId))
      try {
        await api.send('POST', `${path(wsId)}/rescan`)
        stream.accepted(key)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },

    /**
     * GET …/ports/:port/probe: a read, through the proxy's own dial. Its
     * answer is for the screen that asked and is never entity state.
     */
    probe(wsId: string, id: string): Promise<ProbeResult> {
      return api.get<ProbeResult>(`${path(wsId, id)}/probe`)
    },

    /** @internal §4.2 for one request, as the workspaces store's mutate. */
    async mutate(
      key: string, settles: (ev: StreamEvent) => boolean, resolved: (e: Entities) => boolean,
      method: 'POST' | 'PATCH' | 'DELETE', p: string, body?: unknown,
    ): Promise<void> {
      const stream = useStreamStore()
      if (key in stream.inFlight) return
      stream.begin(key, settles, resolved)
      try {
        await api.send(method, p, body)
        stream.accepted(key)
      } catch (e) {
        stream.end(key)
        throw e
      }
    },
  },
})