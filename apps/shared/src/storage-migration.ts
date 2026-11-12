/**
 * Browser storage keys a hermes build wrote under its own name. Each front end
 * reads and writes the icebath spelling; this moves every legacy key once,
 * keeping a value already stored under the icebath name.
 */
export const LEGACY_STORAGE_KEY =
  /^hermes(?=\.(?:desktop|webapp|transcript-tail|updates|connectors|pty|tokenReloadAttempted)\b|-(?:desktop|boot|onboarding|chat|dashboard|sidebar)-|-locale\b|:(?:ui-preferences|webapp|browser|backend-skew|discontinued|update|install|gui|mcp|native|plugin)[-:])/

export function icebathStorageKey(key: string): string {
  return LEGACY_STORAGE_KEY.test(key) ? `icebath${key.slice('hermes'.length)}` : key
}

export function migrateLegacyStorageKeys(storage: Storage | null | undefined): number {
  if (!storage) {
    return 0
  }

  const legacy: string[] = []

  for (let index = 0; index < storage.length; index += 1) {
    const key = storage.key(index)

    if (key !== null && LEGACY_STORAGE_KEY.test(key)) {
      legacy.push(key)
    }
  }

  let moved = 0

  for (const key of legacy) {
    const value = storage.getItem(key)
    const next = icebathStorageKey(key)

    if (value !== null && storage.getItem(next) === null) {
      storage.setItem(next, value)
      moved += 1
    }

    storage.removeItem(key)
  }

  return moved
}

export function migrateBrowserStorage(scope: { localStorage?: Storage; sessionStorage?: Storage }): void {
  for (const pick of [() => scope.localStorage, () => scope.sessionStorage]) {
    try {
      migrateLegacyStorageKeys(pick())
    } catch {
      // Storage unavailable (privacy mode, sandboxed frame): nothing to move.
    }
  }
}
