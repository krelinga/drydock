// What a host-access request says to the operator (design §6, "What a
// configuration may ask of the host"; frontend §6.5a), as data both clients
// render: the web card (components/HostAccessApproval.vue) and the VS Code
// extension's review document (vscode-extension §10.4). A warning added here
// is then in both, and neither can show a request differently.
//
// Every repository-sourced string — a setting's field, its source, its value —
// is escaped here (`escapeFormat`, `showValue`), so a value the container
// wrote can neither forge a line nor visually reorder what is read. Values are
// the settings' JSON, shown as text and never interpreted.
//
// What this does not do (#144): the root sentences key on field names alone,
// so a request that is host root by another route (`runArgs` with
// `--privileged`, a mount of the Docker socket, `capAdd: SYS_ADMIN`) earns no
// extra sentence. Fixing that here fixes both clients.

import type { ApprovalView, HostSettingChange, HostSettingView } from '../types'
import { escapeFormat, showValue } from './text'

/** The title, in two parts so a client may set the first in bold. */
export const APPROVAL_TITLE = { lead: 'Needs approval:', rest: 'this configuration asks for host access.' } as const

/** What approving means, said for every request. */
export const APPROVAL_INTRO =
  "Approving lets this repository's dev container reach this server, as the drydock user or as root (or, with --network host, services on the server's loopback). " +
  'Anything running in the container — an agent included — can then use it, without changing the configuration again. ' +
  'Workspaces share one Claude Code configuration, so while this one runs, the agent in every other running workspace can very likely reach the server too.'

/** `privileged`, new or changed: the container runs as root on the host. */
export const PRIVILEGED_SENTENCE =
  "privileged is root on the host: Drydock's keys and every other workspace are in reach."

/** `initializeCommand`, new or changed: it runs on the host itself, before any container exists. */
export const INITIALIZE_COMMAND_SENTENCE =
  "initializeCommand runs on this server, not in the container, as the drydock user — which can read Drydock's keys and is root through Docker."

/** The request's last word, after any root sentence. */
export const APPROVAL_CLOSING = 'Approve only what you would trust in every running workspace. It is recorded for this repository, and asked again if it changes.'

const SOURCE: Record<string, string> = {
  repository: "the repository's devcontainer.json",
  feature_or_image: 'a Feature or the image',
}

/** Where a setting came from, in words; an unknown source is shown as itself, escaped. */
export function sourceLabel(source: string): string {
  return SOURCE[source] ?? escapeFormat(source)
}

export interface ApprovalItem {
  /** Stable across renders: the raw field and source. Never shown. */
  key: string
  /** The setting's name, escaped. */
  field: string
  /** `from <where>`, escaped. */
  source: string
  /** The value as JSON text, one line each: the value; or `was …` then `now …`; or none. */
  values: string[]
}

export interface ApprovalGroup {
  kind: 'added' | 'changed' | 'removed'
  label: string
  items: ApprovalItem[]
}

export interface ApprovalSections {
  /** The root sentences that apply, in order: each is said beside the intro, and repeated where the approval is confirmed. */
  roots: string[]
  /** Whether `privileged` is new or changed. */
  privileged: boolean
  /** Whether `initializeCommand` is new or changed. */
  initializeCommand: boolean
  /** The non-empty groups, in the order shown: new, changed, no longer asked for. */
  groups: ApprovalGroup[]
}

function item(s: HostSettingView | HostSettingChange, values: string[]): ApprovalItem {
  return { key: `${s.field}\u0000${s.source}`, field: escapeFormat(s.field), source: `from ${sourceLabel(s.source)}`, values }
}

/**
 * The request as both clients show it. A setting no longer asked for is
 * informational and earns no root sentence; one new or changed does.
 */
export function approvalSections(req: ApprovalView): ApprovalSections {
  const asks = (field: string) => [...req.added, ...req.changed].some((s) => s.field === field)
  const privileged = asks('privileged')
  const initializeCommand = asks('initializeCommand')
  const roots: string[] = []
  if (privileged) roots.push(PRIVILEGED_SENTENCE)
  if (initializeCommand) roots.push(INITIALIZE_COMMAND_SENTENCE)
  const groups: ApprovalGroup[] = []
  if (req.added.length > 0) {
    groups.push({ kind: 'added', label: 'New', items: req.added.map((s) => item(s, [showValue(s.value)])) })
  }
  if (req.changed.length > 0) {
    groups.push({
      kind: 'changed', label: 'Changed since the last approval',
      items: req.changed.map((s) => item(s, [`was ${showValue(s.from)}`, `now ${showValue(s.to)}`])),
    })
  }
  if (req.removed.length > 0) {
    groups.push({ kind: 'removed', label: 'No longer asked for', items: req.removed.map((s) => item(s, [])) })
  }
  return { roots, privileged, initializeCommand, groups }
}
