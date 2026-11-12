import { describe, expect, it } from 'vitest'

import type { CronRun } from '../api/generated'
import { refreshedRuns } from './load'

function runs(...uids: string[]): CronRun[] {
  return uids.map((uid, index) => ({
    id: index + 1,
    uid,
    project_id: 7,
    revision: 1,
    actor: 'worker',
    status: 'succeeded',
    summary: { version: 1 },
    created_at: '2026-10-01T09:00:00Z',
    updated_at: '2026-10-01T09:00:00Z',
  }))
}

describe('refreshedRuns', () => {
  it('keeps loaded older pages when the newest page reaches into them', () => {
    const loaded = { runs: runs('r6', 'r5', 'r4', 'r3', 'r2'), nextBeforeUID: 'r2', extended: true }

    const result = refreshedRuns(loaded, { runs: runs('r7', 'r6', 'r5'), nextBeforeUID: 'r5' })

    expect(result.runs.map((run) => run.uid)).toEqual(['r7', 'r6', 'r5', 'r4', 'r3', 'r2'])
    expect(result.nextBeforeUID).toBe('r2')
    expect(result.extended).toBe(true)
  })

  it('drops older pages when the newest page no longer reaches them', () => {
    const loaded = { runs: runs('r4', 'r3', 'r2'), nextBeforeUID: 'r2', extended: true }
    const newest = { runs: runs('r9', 'r8', 'r7'), nextBeforeUID: 'r7' }

    expect(refreshedRuns(loaded, newest)).toEqual({ ...newest, extended: false })
  })
})
