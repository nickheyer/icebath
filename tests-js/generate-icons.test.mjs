import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from 'vitest'

// Builds the Go icon generator of scripts/brand once per run
const source = fileURLToPath(new URL('..', import.meta.url))
const generator = (() => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'icebath-brand-'))
  const exe = path.join(dir, process.platform === 'win32' ? 'brand.exe' : 'brand')
  const built = spawnSync('go', ['-C', path.join(source, 'scripts/brand'), 'build', '-o', exe, '.'], { encoding: 'utf8' })
  if (built.status !== 0) throw new Error(`scripts/brand did not build: ${built.error ?? built.stderr}`)
  return exe
})()
const brand = (args, env) => spawnSync(generator, args, { env, encoding: 'utf8' })

test('the real generator builds and checks native icon artifacts in an independent output', () => {
  const out = fs.mkdtempSync(path.join(os.tmpdir(), 'icebath-icons-'))
  const env = { ...process.env, ICEBATH_HOME: path.join(out, 'home'),
    ICEBATH_RUNTIME_DIR: path.join(out, 'tools'), ICEBATH_PAYLOAD_TAG: 'v1.2.3', ICEBATH_BUILD_COMMIT: '',
    PYTHONPATH: path.join(out, 'foreign-site'), PYTHONHOME: path.join(out, 'foreign-python') }
  try {
    const args = ['--source', source, '--out', out]
    expect(brand(args, env).status).toBe(0)
    const png = fs.readFileSync(path.join(out, 'apps/desktop/assets/icon.png'))
    expect(png.subarray(0, 8)).toEqual(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))
    expect([png.readUInt32BE(16), png.readUInt32BE(20)]).toEqual([1024, 1024])
    expect(brand([...args, '--check'], env).status).toBe(0)
    expect(env.PYTHONPATH).toBe(path.join(out, 'foreign-site'))
  } finally {
    fs.rmSync(out, { recursive: true, force: true })
  }
}, 180_000)

test('failed icon processes cannot report a successful build', () => {
  const empty = fs.mkdtempSync(path.join(os.tmpdir(), 'icebath-icons-empty-'))
  try {
    expect(brand(['--unknown-flag'], process.env).status).toBe(2)
    const missing = brand(['--source', empty, '--out', path.join(empty, 'out')], process.env)
    expect(missing.status).toBe(1)
    expect(missing.stderr).not.toBe('')
    expect(fs.existsSync(path.join(empty, 'out', 'apps/desktop/assets/icon.png'))).toBe(false)
  } finally {
    fs.rmSync(empty, { recursive: true, force: true })
  }
})
