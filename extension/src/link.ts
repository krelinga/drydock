// Connect's URI (vscode-extension §7.1). The extension does not build the
// authority: it takes the server's own link from a view, checks it with
// core's `vscodeURL` (the shape internal/vscode.AttachURL produces, and the
// web UI accepts), and rewrites only the scheme and authority prefix —
// `vscode://vscode-remote/` becomes `vscode-remote://` — which is what VS
// Code's URL handler would do with it. The hex JSON, the SSH authority's
// encoding and the path's percent-encoding stay in internal/vscode.

import { vscodeURL } from '../../web/src/core/reducer'

const PREFIX = 'vscode://vscode-remote/'

/** The `vscode-remote://` folder URI for a view's link, or null for anything `vscodeURL` refuses. */
export function remoteURI(link: unknown): string | null {
  const u = vscodeURL(link)
  if (u === null || !u.startsWith(PREFIX)) return null
  return 'vscode-remote://' + u.slice(PREFIX.length)
}
