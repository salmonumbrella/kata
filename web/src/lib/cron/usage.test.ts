import { describe, expect, it } from 'vitest'

import type { RunSession, RunUsage } from '../api/generated'
import { formatAmount, runCostText, sessionStateText } from './usage'

function session(state: RunSession['state'], extra: Partial<RunSession> = {}): RunSession {
  return { agent: 'codex', session_id: '00000000-0000-4000-8000-000000000001', state, ...extra }
}

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
    [2_410_000, false, '$2.41'],
    [2_414_999, false, '$2.41'],
    [2_415_000, false, '$2.42'],
    [2_410_000, true, '~$2.41'],
    [1, true, '~<$0.01'],
  ])('formats %i microdollars (estimated %s) as %s', (value, estimated, text) => {
    expect(formatAmount(value, estimated)).toBe(text)
  })

  it.each([
    ['complete', usage({ cost_microdollars: 2_410_000 }), '$2.41'],
    ['estimated', usage({ cost_microdollars: 2_410_000, estimated: true }), '~$2.41'],
    [
      'partial',
      usage({
        state: 'partial',
        estimated: true,
        cost_microdollars: 2_410_000,
        sessions: [session('priced'), session('not_found')],
      }),
      '~$2.41+ (1 of 2 sessions not priced)',
    ],
    [
      'partial only from an incomplete list',
      usage({
        state: 'partial',
        cost_microdollars: 2_410_000,
        sessions_incomplete: true,
        sessions: [session('priced')],
      }),
      '$2.41+ (0 of 1 sessions not priced)',
    ],
    ['unknown', usage({ state: 'unknown' }), 'unknown'],
    ['complete without a total', usage({}), 'unknown'],
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
})
