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

/**
 * One path segment: what `{id}` may stand for (a ULID, in practice). Never a
 * slash, never empty, never a dot segment, and never a `%`: URL parsing
 * decodes `%2e%2e` into a `..` segment and resolves it, so an encoded dot
 * segment would match a route here and reach another one. The transport
 * checks the parsed URL too (node/transport.ts).
 */
const SEGMENT = '(?!\\.{1,2}(?:/|\\?|$))[A-Za-z0-9._~-]+'

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

/** A path for route `name` with its parameters filled, each as one segment; a value no route could match throws. */
export function pathFor(name: string, params: Record<string, string> = {}): string {
  const r = ROUTES.find((x) => x.name === name)
  if (r === undefined) throw new Error(`no contracted route ${name}`)
  return r.pattern.replace(/\{([a-z_]+)\}/g, (_m, k: string) => {
    const v = params[k]
    if (v === undefined) throw new Error(`route ${name} needs ${k}`)
    if (!new RegExp(`^${SEGMENT}$`).test(v)) throw new Error(`route ${name}: ${k} is not one plain path segment`)
    return v
  })
}
