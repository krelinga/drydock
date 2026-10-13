// §8 rule 1, the primary control: outside an allowlist of window kinds, the
// extension does not mutate at all. Each window's local extension host runs
// its own instance of this extension, and a command another extension host
// invokes in a window is served by that window's instance, so the instance
// decides from its own window.
//
// Allowed: a local window (no remoteName) and `ssh-remote`, whose remote
// host runs as the operator's own SSH user. Everything else — an attached or
// reopened dev container, a tunnel, a codespace, WSL, and any name VS Code
// adds later — reads, connects and opens Drydock, and mutates nothing: it is
// an allowlist, so an unknown kind of window fails closed.

const MAY_MUTATE: ReadonlySet<string | undefined> = new Set([undefined, 'ssh-remote'])

export function mayMutate(remoteName: string | undefined): boolean {
  return MAY_MUTATE.has(remoteName)
}

/** What a mutating command says instead, in a window that may not mutate. */
export const ELSEWHERE = 'Do this from a local or SSH window, or in Drydock: this window may run code from the workspace it is attached to.'
