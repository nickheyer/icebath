/**
 * @hermes belongs to the default bot only while its source has no real
 * (pre-reservation) `icebath` profile. With one, the roster shows exactly one
 * @hermes — the real profile — and the default answers to @default, so a
 * typed @hermes reaches the same profile the backend's message_agent does.
 */

import { describe, expect, it, vi } from 'vitest'

import { botHandle, botMentionTag, claimDefaultHandles, mergeMultiSourceRoster, resolveRosterMentions } from './data'
import { previewKind } from './row-helpers'
import type { RosterRow } from './types'

vi.mock('@hermes/plugin-sdk', async () => {
  const { atom } = await import('nanostores')

  return { atom, host: {}, queryClient: undefined, useQuery: vi.fn(), useValue: vi.fn() }
})

vi.mock('./shared', () => ({ getPluginCtx: () => null, ID: 'hermes-bots' }))

function roster(names: string[]): RosterRow[] {
  const local = { profiles: names.map(name => ({ name })) }

  // What Electron's buildAgentRoster reports for a single local source: bare-name handles.
  const union = {
    agents: names.map(profile => ({
      connectionId: 'local',
      connectionKind: 'local' as const,
      connectionLabel: 'This device',
      handle: profile,
      profile,
      targetProfile: profile
    })),
    primaryConnectionId: 'local'
  }

  return mergeMultiSourceRoster(local, union, 'local').profiles as RosterRow[]
}

describe('the @hermes handle', () => {
  it.each([
    [['default', 'researcher'], 'default'],
    [['default', 'icebath', 'researcher'], 'icebath']
  ])('roster %j shows one @hermes and a typed @hermes reaches %s', (names, owner) => {
    const rows = roster(names)
    const handles = rows.map(row => botHandle(row.name, row))

    expect(handles.filter(handle => handle === 'icebath')).toHaveLength(1)
    expect(rows.map(row => botMentionTag(row)).filter(tag => tag === 'icebath')).toHaveLength(1)
    expect(resolveRosterMentions('@hermes ping', rows, { name: 'researcher' }).map(row => row.name)).toEqual([owner])
  })

  it('names the default @default once a real icebath profile holds the handle', () => {
    const rows = roster(['default', 'icebath', 'researcher'])
    const byName = Object.fromEntries(rows.map(row => [row.name, botHandle(row.name, row)]))

    expect(byName).toEqual({ default: 'default', hermes: 'icebath', researcher: 'researcher' })
    expect(resolveRosterMentions('@default ping', rows, { name: 'researcher' }).map(row => row.name)).toEqual(['default'])
  })

  it('keeps each source separate: a remote icebath profile does not rename the local default', () => {
    const rows = claimDefaultHandles<Partial<RosterRow>>([
      { connectionId: 'local', name: 'default' },
      { connectionId: 'mini', name: 'default' },
      { connectionId: 'mini', name: 'icebath' }
    ])

    expect(rows.map(row => botHandle(String(row.name), row))).toEqual(['icebath', 'default', 'icebath'])
  })

  it("never lets a source's stale 'icebath' handle on its default answer @hermes", () => {
    // An older backend labels its default @hermes even beside a real icebath profile.
    const rows = claimDefaultHandles<RosterRow>([
      { connectionId: 'mini', handle: 'icebath', name: 'default', remoteSource: true },
      { connectionId: 'mini', handle: 'icebath', name: 'icebath', remoteSource: true },
      { connectionId: 'local', name: 'researcher' }
    ])

    expect(resolveRosterMentions('@hermes ping', rows, { name: 'researcher' }).map(row => row.name)).toEqual(['icebath'])
  })

  it('names a DM sender in a row preview by its roster handle', () => {
    const rows = claimDefaultHandles<RosterRow>(
      ['default', 'icebath', 'researcher'].map(name => ({ connectionId: 'local', name }))
    )

    const sender = (name: string, list: RosterRow[]) =>
      previewKind(`Message from 🤖 ${name} (@x): hi`, list, 'local').fromBot

    expect([sender('default', rows), sender('icebath', rows)]).toEqual(['default', 'icebath'])
    // No real icebath profile: the default is still @hermes (#89484).
    expect(sender('default', claimDefaultHandles<RosterRow>([{ connectionId: 'local', name: 'default' }]))).toBe(
      'icebath'
    )
  })
})
