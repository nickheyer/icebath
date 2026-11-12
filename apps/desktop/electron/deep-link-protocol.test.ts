import assert from 'node:assert/strict'

import { test } from 'vitest'

import { registerDeepLinkProtocol } from './deep-link-protocol'

function run(env: NodeJS.ProcessEnv, defaultApp = true) {
  const calls: unknown[][] = []
  const log: string[] = []
  registerDeepLinkProtocol({
    app: { setAsDefaultProtocolClient: (...args: unknown[]) => { calls.push(args);

 return true } },
    protocol: 'icebath',
    env,
    defaultApp,
    argv: ['/electron', '.'],
    execPath: '/electron',
    resolve: entry => `/checkout/${entry}`,
    log: line => log.push(line)
  })

  return { calls, log }
}

test('the E2E/dev guard leaves the OS icebath:// handler untouched; normal launches still register', () => {
  const skipped = run({ ICEBATH_DESKTOP_SKIP_PROTOCOL_REGISTRATION: '1' })
  assert.deepEqual(skipped.calls, [])
  assert.match(skipped.log.join('\n'), /registration skipped/)

  assert.deepEqual(run({}).calls, [['icebath', '/electron', ['/checkout/.']]])
  assert.deepEqual(run({}, false).calls, [['icebath']])
})
