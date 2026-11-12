export type ParentWatchdogEnv = {
  ICEBATH_PARENT_PID: string
  ICEBATH_PARENT_START_MARKER?: string
  ICEBATH_PARENT_NONCE?: string
  /** Spawn tag consumed by hermes_cli.process_identity (`v1:<install>:<purpose>:<spawner_pid>:<spawner_create_s>`).
   *  Install is `-` (unknown) from the Desktop — the Python side scopes by
   *  venv membership, so the tag only needs lineage, not install identity. */
  ICEBATH_SPAWN?: string
}

/**
 * Build the cross-runtime marker for Electron's own process without spawning
 * an OS helper. Electron reports milliseconds since the Unix epoch; the Python
 * watchdog converts its exact Windows FILETIME to the same representation.
 */
export function electronProcessStartMarker(pid: number, ownPid: number, creationTime: unknown): string | null {
  if (pid !== ownPid || typeof creationTime !== 'number' || !Number.isFinite(creationTime)) {
    return null
  }

  const milliseconds = Math.trunc(creationTime)

  if (!Number.isSafeInteger(milliseconds) || milliseconds <= 0) {
    return null
  }

  return `winms:${milliseconds}`
}

/**
 * Keep the watchdog's marker and nonce atomic. A failed marker probe degrades
 * to the legacy PID-only watchdog instead of preventing the backend spawn.
 */
export function parentWatchdogEnv(pid: number, startMarker: string | null, nonce: string): ParentWatchdogEnv {
  if (!Number.isInteger(pid) || pid <= 0) {
    throw new Error('Parent watchdog requires a positive process ID.')
  }

  const env: ParentWatchdogEnv = { ICEBATH_PARENT_PID: String(pid) }
  env.ICEBATH_SPAWN = spawnTag(pid, startMarker)

  if (startMarker === null) {
    return env
  }

  if (!startMarker || !nonce) {
    throw new Error('Parent watchdog marker and nonce must be non-empty.')
  }

  env.ICEBATH_PARENT_START_MARKER = startMarker
  env.ICEBATH_PARENT_NONCE = nonce

  return env
}

/** Build the `ICEBATH_SPAWN` tag mirrored by hermes_cli/process_identity.py.
 *  The Desktop only ever spawns backend servers, so purpose is `serve`; the
 *  spawner create-time is derived from the same `winms:` marker the parent
 *  watchdog uses (seconds, 3 decimals), `-` when the marker probe failed. */
export function spawnTag(pid: number, startMarker: string | null): string {
  let createPart = '-'

  if (startMarker?.startsWith('winms:')) {
    const ms = Number(startMarker.slice('winms:'.length))

    if (Number.isFinite(ms) && ms > 0) {
      createPart = (ms / 1000).toFixed(3)
    }
  }

  return `v1:-:serve:${pid}:${createPart}`
}
