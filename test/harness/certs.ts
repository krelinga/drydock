// A throwaway CA and its leaves, each with a subjectAltName (Chromium ignores
// CN), shared by the browser tier and the extension's client tier. Two days'
// validity; removed with the run's root.

import { execFileSync } from 'node:child_process'
import { mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'

export interface Leaf {
  /** The file stem: <dir>/<name>.crt and .key. */
  name: string
  /** The subjectAltName value, e.g. `DNS:drydock.test`. */
  san: string
}

/** Writes <dir>/ca.crt, ca.key and one key pair per leaf, signed by the CA. */
export function makeCerts(dir: string, leaves: readonly Leaf[], caName = 'Drydock test-tier throwaway CA'): void {
  mkdirSync(dir, { recursive: true })
  const ossl = (...args: string[]) => execFileSync('openssl', args, { cwd: dir, stdio: ['ignore', 'ignore', 'pipe'] })
  ossl('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-sha256', '-days', '2',
    '-keyout', 'ca.key', '-out', 'ca.crt', '-subj', `/CN=${caName}`,
    '-addext', 'basicConstraints=critical,CA:TRUE,pathlen:0',
    '-addext', 'keyUsage=critical,keyCertSign,cRLSign')
  for (const { name, san } of leaves) {
    writeFileSync(path.join(dir, `${name}.ext`), [
      `subjectAltName=${san}`,
      'basicConstraints=critical,CA:FALSE',
      'keyUsage=critical,digitalSignature,keyEncipherment',
      'extendedKeyUsage=serverAuth',
    ].join('\n'))
    ossl('req', '-newkey', 'rsa:2048', '-nodes', '-sha256', '-keyout', `${name}.key`, '-out', `${name}.csr`, '-subj', `/CN=${name}`)
    ossl('x509', '-req', '-in', `${name}.csr`, '-CA', 'ca.crt', '-CAkey', 'ca.key', '-CAcreateserial',
      '-out', `${name}.crt`, '-days', '2', '-sha256', '-extfile', `${name}.ext`)
  }
}
