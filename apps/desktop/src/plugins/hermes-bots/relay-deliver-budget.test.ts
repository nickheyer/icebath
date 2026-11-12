import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import {
  RELAY_DELIVER_BACKEND_CEILING_MS,
  RELAY_DELIVER_SETTLEMENT_MARGIN_MS,
  RELAY_DELIVER_TIMEOUT_MS,
  RELAY_TURN_ATTEMPT_MS,
  RELAY_TURN_LOCK_WAIT_MS,
  RELAY_TURN_MAX_ATTEMPTS
} from './relay-budget'

// #93911 review follow-up: the Desktop deadline for bot_relay.deliver mirrors
// three backend numbers. Nothing in the type system links a TS constant to a
// Go default, so this file is the seam: it reads the backend defaults and
// fails when a mirror drifts or the settlement margin stops being positive.
// Without it, raising the backend turn timeout would silently reintroduce
// #93911 — the client giving up before a valid typed settlement arrives.

const repoRoot = join(process.cwd(), '..', '..')
const configDefaults = readFileSync(join(repoRoot, 'pkg/config/defaults.go'), 'utf8')
const relayPlumbing = readFileSync(join(repoRoot, 'pkg/botmode/relay_mailbox.go'), 'utf8')

function goConstant(name: string): number {
  const match = relayPlumbing.match(new RegExp(`^\\s*${name}\\s*=\\s*(\\d+)$`, 'm'))
  expect(match, `${name} must exist as a literal in pkg/botmode/relay_mailbox.go`).toBeTruthy()

  return Number(match![1])
}

describe('bot_relay.deliver budget mirrors', () => {
  it('mirrors the backend turn-lock default', () => {
    const lockWaitMatch = configDefaults.match(/"turn_wait_seconds":\s*(\d+)/)

    expect(lockWaitMatch, 'bot_mode.turn_wait_seconds default must exist in pkg/config/defaults.go').toBeTruthy()
    expect(RELAY_TURN_LOCK_WAIT_MS).toBe(Number(lockWaitMatch![1]) * 1000)
  })

  it('mirrors the backend per-attempt turn timeout', () => {
    // The backend names both numbers explicitly (pkg/botmode/relay_mailbox.go) so the mirror is a
    // constant-to-constant check, not a count of textual subprocess.run(...) call sites.
    expect(RELAY_TURN_ATTEMPT_MS).toBe(goConstant('TurnAttemptTimeoutSeconds') * 1000)
    expect(RELAY_TURN_MAX_ATTEMPTS).toBe(goConstant('TurnMaxAttempts'))
  })

  it('shares its settlement margin with the sender-side waiter budget', () => {
    // pkg/botmode/bot_relay_cases_test.go checks that ReplyWaitSeconds exceeds the rebuilt sum.
    expect(RELAY_DELIVER_SETTLEMENT_MARGIN_MS).toBe(goConstant('DesktopDeliverSettlementMarginSeconds') * 1000)
  })

  it('keeps the client deadline strictly greater than the backend ceiling', () => {
    // Strictly greater, not equal: a backend that answers at its own limit
    // still has to serialize and transport that answer.
    expect(RELAY_DELIVER_SETTLEMENT_MARGIN_MS, 'settlement margin must be positive').toBeGreaterThan(0)

    // The ceiling is the backend's own worst case: lock wait + every attempt;
    // the mirror tests above tie each term to its Go default.
    expect(RELAY_DELIVER_BACKEND_CEILING_MS).toBe(
      RELAY_TURN_LOCK_WAIT_MS + RELAY_TURN_ATTEMPT_MS * RELAY_TURN_MAX_ATTEMPTS
    )
    expect(RELAY_DELIVER_TIMEOUT_MS).toBe(RELAY_DELIVER_BACKEND_CEILING_MS + RELAY_DELIVER_SETTLEMENT_MARGIN_MS)
  })
})
