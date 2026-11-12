import type { Usage } from '@icebath/shared/gateway-events'

export { usageFromTick } from '@icebath/shared/usage-tick'

export const ZERO: Usage = { calls: 0, input: 0, output: 0, total: 0 }
