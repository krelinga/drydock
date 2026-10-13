// The client tier's global setup (vitest.client.config.mts): the CA and its
// leaves, made before the workers fork, and NODE_EXTRA_CA_CERTS pointing at
// the CA — never a switch that turns validation off.

import { existsSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { makeTierCerts } from '../../../test/extension/stack'

export default function setup(): () => void {
  // A CA made beforehand (test/extension/system-ca.sh, for the host tier,
  // whose VS Code reads the system store) is used as it is, and left.
  const given = process.env.DRYDOCK_EXTENSION_CERTS
  if (given !== undefined && existsSync(path.join(given, 'ca.crt'))) {
    process.env.NODE_EXTRA_CA_CERTS = path.join(given, 'ca.crt')
    return () => {}
  }
  const dir = mkdtempSync(path.join(tmpdir(), 'drydock-extension-certs-'))
  process.env.NODE_EXTRA_CA_CERTS = makeTierCerts(dir)
  process.env.DRYDOCK_EXTENSION_CERTS = dir
  return () => rmSync(dir, { recursive: true, force: true })
}
