// Every command the extension contributes (vscode-extension §6.1), as data:
// the adapter registers exactly this list, and test/manifest.spec.ts holds
// package.json's `contributes.commands` to it.
//
// `mutating` is §8 rule 1's input, and it is required, so a command added
// later says which it is. A mutating command never runs its handler outside
// a local or SSH window (dispatch.ts); signing in is one, since it changes
// the credential every window then uses. The hub (`drydock.go`) reads and
// connects, and holds rule 1 itself for the clone it can start (flows.ts).

export interface CommandDef {
  id: `drydock.${string}`
  title: string
  mutating: boolean
  /**
   * Takes a workspace id from a tree item (§8 rule 2): only a command that
   * reads or connects may, never a mutating one (dispatch.spec.ts holds it).
   */
  takesId?: true
}

export const COMMANDS = [
  { id: 'drydock.go', title: 'Go to Workspace…', mutating: false },
  { id: 'drydock.clone', title: 'Clone Repository…', mutating: true },
  { id: 'drydock.connect', title: 'Connect to Workspace…', mutating: false, takesId: true },
  { id: 'drydock.refreshRepositories', title: 'Refresh Repositories', mutating: true },
  { id: 'drydock.signIn', title: 'Sign In', mutating: true },
  { id: 'drydock.signOut', title: 'Sign Out', mutating: true },
  { id: 'drydock.openInBrowser', title: 'Open in Drydock', mutating: false, takesId: true },
  { id: 'drydock.showLog', title: 'Show Log', mutating: false },
] as const satisfies readonly CommandDef[]

export type CommandId = (typeof COMMANDS)[number]['id']
