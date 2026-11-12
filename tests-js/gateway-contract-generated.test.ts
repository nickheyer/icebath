/**
 * The committed gateway contract files under apps/shared/src are exactly what
 * `go generate ./pkg/tuigateway` wrote. The generator records a SHA-256 of each
 * file in pkg/tuigateway/contractgen/digest.go and TestGeneratedFilesAreCurrent
 * fails when the Go catalog no longer renders those bytes, so a hand edit or a
 * partial regeneration of either file shows up here.
 */
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import path from 'node:path'

import { describe, expect, it } from 'vitest'

const REPO_ROOT = path.resolve(__dirname, '..')
const DIGESTS = path.join(REPO_ROOT, 'pkg/tuigateway/contractgen/digest.go')

function committedDigests(): Map<string, string> {
  const source = readFileSync(DIGESTS, 'utf8')
  const body = source.match(/var Committed = map\[string\]string\{\n((?:\t"[^"]+": "[0-9a-f]{64}",\n)+)\}/)

  expect(body, 'digest.go must hold the Committed table go generate writes').toBeTruthy()

  return new Map([...body![1].matchAll(/^\t"([^"]+)": "([0-9a-f]{64})",$/gm)].map(match => [match[1], match[2]]))
}

describe('generated gateway contract files', () => {
  const digests = committedDigests()

  it('lists both generated files', () => {
    expect([...digests.keys()].sort()).toEqual([
      'apps/shared/src/gateway-contract.generated.ts',
      'apps/shared/src/gateway-contract.openrpc.json'
    ])
  })

  it.each([...digests.entries()])('%s matches the digest go generate recorded', (file, digest) => {
    const bytes = readFileSync(path.join(REPO_ROOT, file))

    expect(createHash('sha256').update(bytes).digest('hex'), `${file} is stale: run go generate ./pkg/tuigateway`).toBe(digest)
  })
})
