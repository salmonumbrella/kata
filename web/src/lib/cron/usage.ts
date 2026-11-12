import type { RunSession, RunUsage } from '../api/generated'

// These texts mirror `kata cron run usage` (cmd/kata/cron_usage.go). The
// daemon owns the total; the browser only formats what it returns.

export function formatAmount(microdollars: number, estimated: boolean): string {
  let cents = Math.floor(microdollars / 10_000)
  if (microdollars % 10_000 >= 5_000) cents += 1
  const prefix = estimated ? '~' : ''
  if (microdollars > 0 && cents === 0) return `${prefix}<$0.01`
  return `${prefix}$${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, '0')}`
}

export function runCostText(usage: RunUsage): string {
  if (usage.state === 'no_sessions') return 'no sessions'
  if (usage.state === 'unknown' || usage.cost_microdollars === undefined) return 'unknown'
  const total = formatAmount(usage.cost_microdollars, usage.estimated)
  if (usage.state === 'complete') return total
  const missing = usage.sessions.filter((session) => session.state !== 'priced').length
  // Every session priced still leaves a partial total when the recorded
  // session list was incomplete (S5 agentsview.TotalText).
  if (missing === 0 && usage.sessions_incomplete) return `${total}+ (session list incomplete)`
  return `${total}+ (${missing} of ${usage.sessions.length} sessions not priced)`
}

export function sessionStateText(session: RunSession): string {
  switch (session.state) {
    case 'priced': {
      if (session.cost_microdollars === undefined) return 'AgentsView unavailable'
      const estimated = session.cost_source === 'computed' || session.cost_source === 'mixed'
      return formatAmount(session.cost_microdollars, estimated)
    }
    case 'unpriced':
      return session.unpriced_models?.length
        ? `unpriced (${session.unpriced_models.join(', ')})`
        : 'unpriced'
    case 'no_usage':
      return 'no usage recorded'
    case 'not_found':
      return 'not in AgentsView yet'
    case 'ambiguous':
      return session.candidates?.length
        ? `ambiguous session (${session.candidates.join(', ')})`
        : 'ambiguous session'
    case 'unauthorized':
      return "AgentsView rejected Kata's token"
    default:
      return 'AgentsView unavailable'
  }
}

// What the drawer says when the daemon answers a usage read with an error.
export function usageFailureText(status: number): string {
  switch (status) {
    case 403:
      return 'Cost needs a signed-in session'
    case 404:
      return 'Run not found'
    case 502:
      return 'The total cost cannot be represented.'
    case 503:
      return 'Usage is disabled on this daemon.'
    default:
      return 'Could not read cost. Refresh to retry.'
  }
}
