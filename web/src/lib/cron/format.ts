import type { StatusDotStatus } from '@kenn-io/kit-ui'

import type { CronJob, CronRun } from '../api/generated'
import { mediumTimestamp } from '../kata/dates'

// Cron times read like the Credentials page's.
export { mediumTimestamp as formatCronTime }

const runDots: Record<string, StatusDotStatus> = {
  running: 'working',
  succeeded: 'idle',
  failed: 'unclean',
  cancelled: 'quiet',
}

export function cronRunDot(status: string): StatusDotStatus {
  return runDots[status] ?? 'stale'
}

export function cronTriggerLabel(job: CronJob): string {
  const trigger = job.definition.trigger
  if (trigger.cron) return trigger.timezone ? `${trigger.cron} (${trigger.timezone})` : trigger.cron
  if (trigger.interval_seconds) return `Every ${intervalLabel(trigger.interval_seconds)}`
  if (trigger.at) return `Once at ${mediumTimestamp(trigger.at)}`
  return trigger.kind
}

function intervalLabel(seconds: number): string {
  if (seconds % 3600 === 0) return `${seconds / 3600}h`
  if (seconds % 60 === 0) return `${seconds / 60}m`
  return `${seconds}s`
}

export function cronTokensLabel(run: CronRun): string | undefined {
  const input = run.summary.input_tokens
  const output = run.summary.output_tokens
  if (input === undefined && output === undefined) return undefined
  return `${input ?? 0} in · ${output ?? 0} out`
}
