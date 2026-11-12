import type { StatusDotStatus } from '@kenn-io/kit-ui'

import type { CronJob, CronRun } from '../api/generated'

const runDots: Record<string, StatusDotStatus> = {
  running: 'working',
  succeeded: 'idle',
  failed: 'unclean',
  cancelled: 'quiet',
}

export function cronRunDot(status: string): StatusDotStatus {
  return runDots[status] ?? 'stale'
}

const timeFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })

export function formatCronTime(value: string | undefined): string {
  if (!value) return '—'
  const time = Date.parse(value)
  return Number.isFinite(time) ? timeFormat.format(time) : value
}

export function cronTriggerLabel(job: CronJob): string {
  const trigger = job.definition.trigger
  if (trigger.cron) return trigger.timezone ? `${trigger.cron} (${trigger.timezone})` : trigger.cron
  if (trigger.interval_seconds) return `Every ${intervalLabel(trigger.interval_seconds)}`
  if (trigger.at) return `Once at ${formatCronTime(trigger.at)}`
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
