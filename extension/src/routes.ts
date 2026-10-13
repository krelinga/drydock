// The routes the extension may call: extension/contract.json, which
// internal/api's TestExtensionContract holds to the server's table. The
// client sends nothing to a path none of them matches (§8 rule 5), so no
// data — a link, a field of a view, a command's argument — can steer a
// request carrying the token anywhere else on the origin.

import contract from '../contract.json'

export interface ContractRoute {
  name: string
  method: string
  pattern: string
}

export const ROUTES: readonly ContractRoute[] = contract.routes

/** One path segment: what `{id}` may stand for. Never a slash, never empty, never a dot segment. */
const SEGMENT = '(?!\\.{1,2}(?:/|$))[A-Za-z0-9._~%-]+'

function compile(pattern: string): RegExp {
  const body = pattern.split(/\{[a-z_]+\}/).map((lit) => lit.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join(SEGMENT)
  return new RegExp(`^${body}(?:\\?[A-Za-z0-9._~%&=-]*)?$`)
}

const COMPILED = ROUTES.map((r) => ({ ...r, re: compile(r.pattern) }))

/** The contracted route `method path` is, or null. `path` may carry a query. */
export function routeOf(method: string, path: string): ContractRoute | null {
  const hit = COMPILED.find((r) => r.method === method && r.re.test(path))
  return hit === undefined ? null : { name: hit.name, method: hit.method, pattern: hit.pattern }
}

/** A path for route `name` with its parameters filled, each encoded as one segment. */
export function pathFor(name: string, params: Record<string, string> = {}): string {
  const r = ROUTES.find((x) => x.name === name)
  if (r === undefined) throw new Error(`no contracted route ${name}`)
  return r.pattern.replace(/\{([a-z_]+)\}/g, (_m, k: string) => {
    const v = params[k]
    if (v === undefined) throw new Error(`route ${name} needs ${k}`)
    return encodeURIComponent(v)
  })
}
