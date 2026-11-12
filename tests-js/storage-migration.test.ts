import { describe, expect, it } from 'vitest'

import { icebathStorageKey, migrateBrowserStorage, migrateLegacyStorageKeys } from '../apps/shared/src/storage-migration'

function memoryStorage(entries: Record<string, string>): Storage {
  const map = new Map(Object.entries(entries))

  return {
    get length() {
      return map.size
    },
    clear: () => map.clear(),
    getItem: key => map.get(key) ?? null,
    key: index => [...map.keys()][index] ?? null,
    removeItem: key => void map.delete(key),
    setItem: (key, value) => void map.set(key, String(value))
  }
}

describe('legacy storage migration', () => {
  it('maps only the storage key families to icebath names', () => {
    expect(icebathStorageKey('hermes.desktop.keybinds')).toBe('icebath.desktop.keybinds')
    expect(icebathStorageKey('hermes-boot-background')).toBe('icebath-boot-background')
    expect(icebathStorageKey('hermes:ui-preferences:outbox:x')).toBe('icebath:ui-preferences:outbox:x')
    expect(icebathStorageKey('hermes-locale')).toBe('icebath-locale')
    expect(icebathStorageKey('hermes-bots:pane')).toBe('hermes-bots:pane')
    expect(icebathStorageKey('hermes.run')).toBe('hermes.run')
  })

  it('moves each legacy key once and keeps a value already under the icebath name', () => {
    const storage = memoryStorage({
      'hermes.desktop.keybinds': '{"a":1}',
      'hermes-desktop-mode-v1': 'dark',
      'icebath-desktop-mode-v1': 'light',
      'hermes-bots:pane': 'kept'
    })

    expect(migrateLegacyStorageKeys(storage)).toBe(1)
    expect(storage.getItem('icebath.desktop.keybinds')).toBe('{"a":1}')
    expect(storage.getItem('icebath-desktop-mode-v1')).toBe('light')
    expect(storage.getItem('hermes.desktop.keybinds')).toBeNull()
    expect(storage.getItem('hermes-desktop-mode-v1')).toBeNull()
    expect(storage.getItem('hermes-bots:pane')).toBe('kept')
    expect(migrateLegacyStorageKeys(storage)).toBe(0)
  })

  it('survives a storage that throws', () => {
    const scope = {
      get localStorage(): Storage {
        throw new Error('denied')
      },
      sessionStorage: memoryStorage({ 'hermes.webapp.session.v1:x': 't' })
    }

    expect(() => migrateBrowserStorage(scope)).not.toThrow()
    expect(scope.sessionStorage.getItem('icebath.webapp.session.v1:x')).toBe('t')
  })
})
