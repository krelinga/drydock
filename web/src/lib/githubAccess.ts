// The detail view's "GitHub access" row (frontend §9, §11 Phase 3): what the
// workspace's newest token event says, as one of design §12's sentences —
// never a git error, and never a retry that will fail the same way.
//
// Read from the workspace's feed (the stream's events and the detail's own),
// keyed on the event's `data.reason`, never on its message: a pure function of
// what the reducer holds, so it invents nothing.
//
// The repository's own state comes first. The broker refuses a repository the
// catalog has marked removed or archived from its own row, before asking
// GitHub, and writes no event for it — one per git command would flood the
// feed — so the feed's newest token event can be an issue from before. The
// catalog is the authority on those two, and it is what the read-only badge
// reads, so the row and the badge say one thing.

import type { StreamEvent } from '../api/types'

export interface GitHubAccess {
  ok: boolean
  sentence: string
  at: string
}

const REFUSED: Record<string, string> = {
  rate_limited:
    "GitHub is refusing requests: the App's rate limit is spent, or the App is suspended. Tokens already issued work until they expire; check the App on GitHub — there is nothing to retry from here.",
  app_permission_missing:
    "The GitHub App lacks a permission this workspace needs. Check the App's permissions, and accept any pending permission request on its installation.",
  revoked:
    "This repository is no longer in the GitHub App's installation, so the workspace is read-only. Unpushed work in the working tree survives.",
  repo_archived: 'The repository is archived on GitHub, so nothing can be pushed to it.',
}

const OTHER = "GitHub did not issue a token just now. The next git or gh command asks again."

/** What the catalog says of the workspace's repository, when it has read it. */
export interface RepoState {
  removed: boolean
  archived: boolean
}

/** Null when nothing is known. `feed` is newest first. */
export function githubAccess(feed: readonly StreamEvent[] | undefined, repo: RepoState | null = null): GitHubAccess | null {
  if (repo?.removed) return { ok: false, sentence: REFUSED.revoked!, at: '' }
  if (repo?.archived) return { ok: false, sentence: REFUSED.repo_archived!, at: '' }
  for (const ev of feed ?? []) {
    if (ev.kind === 'token.issued') return { ok: true, sentence: 'Working: a token was issued', at: ev.at }
    if (ev.kind === 'token.refused') {
      const reason = typeof ev.data?.reason === 'string' ? ev.data.reason : ''
      return { ok: false, sentence: REFUSED[reason] ?? OTHER, at: ev.at }
    }
  }
  return null
}
