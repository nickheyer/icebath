export type ChallengeOutcome = 'done' | 'failed' | 'closed' | 'timeout' | 'refused' | 'error'

export interface MachineProfile {
  ageDays: number | null
  arch: string
  locale: string
  platform: string
  release: string
  username: string
}

export type UpdateRunOutcome = 'success' | 'failed' | 'noop' | 'refused'
export type UpdateFailedStage = 'download' | 'verify' | 'apply' | 'restart' | 'other'

export interface UpdateRunReport {
  outcome: UpdateRunOutcome
  mechanism: string
  duration_ms: number
  failed_stage?: UpdateFailedStage
  from_commit_date?: number
}

export interface UiPreferencesSnapshot {
  exists: boolean
  generation?: string
  revision: number
  values: Record<string, string>
}

export interface UiPreferencesWrite {
  seed?: boolean
  values: Record<string, null | string>
}
