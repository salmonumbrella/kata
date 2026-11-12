import { describe, expect, it } from 'vitest'

import type { RunSession, RunUsage } from '../api/generated'
import { formatAmount, runCostText, sessionStateText, usageFailureText } from './usage'

function session(state: RunSession['state'], extra: Partial<RunSession> = {}): RunSession {
  return { agent: 'codex', session_id: '00000000-0000-4000-8000-000000000001', state, ...extra }
}

// A priced session as the daemon returns it: a priced session always has a cost.
const priced = session('priced', { cost_microdollars: 2_410_000, cost_source: 'reported' })

function usage(extra: Partial<RunUsage>): RunUsage {
  return {
    run_uid: '01K00000000000000000000001',
    state: 'complete',
    estimated: false,
    fetched_at: '2026-10-01T09:30:00Z',
    sessions_incomplete: false,
    sessions: [],
    ...extra,
  }
}

describe('cron run cost text', () => {
  it.each([
    [0, false, '$0.00'],
    [1, false, '<$0.01'],
    [4_999, false, '<$0.01'],
    [5_000, false, '$0.01'],
    [2_415_000, false, '$2.42'],
    [2_410_000, true, '~$2.41'],
    [1, true, '~<$0.01'],
  ])('formats %i microdollars (estimated %s) as %s', (value, estimated, text) => {
    expect(formatAmount(value, estimated)).toBe(text)
  })

  it.each([
    ['complete', usage({ cost_microdollars: 2_410_000, sessions: [priced] }), '$2.41'],
    [
      'estimated',
      usage({ cost_microdollars: 2_410_000, estimated: true, sessions: [priced] }),
      '~$2.41',
    ],
    [
      'partial',
      usage({
        state: 'partial',
        estimated: true,
        cost_microdollars: 2_410_000,
        sessions: [priced, session('not_found')],
      }),
      '~$2.41+ (1 of 2 sessions not priced)',
    ],
    [
      'partial only from an incomplete list',
      usage({
        state: 'partial',
        cost_microdollars: 2_410_000,
        sessions_incomplete: true,
        sessions: [priced],
      }),
      '$2.41+ (session list incomplete)',
    ],
    ['unknown', usage({ state: 'unknown' }), 'unknown'],
    ['no sessions', usage({ state: 'no_sessions' }), 'no sessions'],
  ])('describes a %s total', (_name, value, text) => {
    expect(runCostText(value)).toBe(text)
  })

  it.each([
    [session('priced', { cost_microdollars: 1_200_000, cost_source: 'reported' }), '$1.20'],
    [session('priced', { cost_microdollars: 1_200_000, cost_source: 'computed' }), '~$1.20'],
    [session('priced', { cost_microdollars: 1_200_000, cost_source: 'mixed' }), '~$1.20'],
    [
      session('unpriced', { unpriced_models: ['example-a', 'example-b'] }),
      'unpriced (example-a, example-b)',
    ],
    [session('no_usage'), 'no usage recorded'],
    [session('not_found'), 'not in AgentsView yet'],
    [
      session('ambiguous', { candidates: ['example-1', 'example-2'] }),
      'ambiguous session (example-1, example-2)',
    ],
    [session('unauthorized'), "AgentsView rejected Kata's token"],
    [session('unavailable'), 'AgentsView unavailable'],
  ])('describes a %s session', (value, text) => {
    expect(sessionStateText(value)).toBe(text)
  })

  // The daemon's error code tells a gateway failure from an overflowing total,
  // both of which answer 502.
  it.each([
    [403, 'usage_forbidden', 'Cost needs a signed-in session'],
    [404, 'run_not_found', 'Run not found'],
    [502, 'usage_total_overflow', 'The total cost cannot be represented.'],
    [502, 'daemon_unreachable', 'Could not read cost. Refresh to retry.'],
    [502, 'daemon_auth_required', 'Could not read cost. Refresh to retry.'],
    [503, 'usage_source_disabled', 'Usage is disabled on this daemon.'],
    [500, 'internal', 'Could not read cost. Refresh to retry.'],
  ])('explains a usage read answered with %i %s', (status, code, text) => {
    expect(usageFailureText(status, code)).toBe(text)
  })
})
