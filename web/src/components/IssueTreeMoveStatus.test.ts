import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, expect, it, vi } from 'vitest'
import type { MoveTreePreviewResponse, MoveTreeRequest } from '../lib/api/generated'
import IssueTreeMoveStatus from './IssueTreeMoveStatus.svelte'

const request: MoveTreeRequest = {
  actor: 'tester',
  actor_authority_uid: '01AAAAAAAAAAAAAAAAAAAAAAAA',
  source_authority_uid: '01AAAAAAAAAAAAAAAAAAAAAAAA',
  source_project_uid: '01BBBBBBBBBBBBBBBBBBBBBBBB',
  destination_authority_uid: '01AAAAAAAAAAAAAAAAAAAAAAAA',
  destination_project_uid: '01CCCCCCCCCCCCCCCCCCCCCCCC',
  issue_uid: '01DDDDDDDDDDDDDDDDDDDDDDDD',
  request_uid: '01EEEEEEEEEEEEEEEEEEEEEEEE',
}
const preview: MoveTreePreviewResponse = {
  digest: 'preview-digest',
  snapshot_digest: 'snapshot-digest',
  source_revisions: {},
  members: [],
  relations: [],
  blockers: [],
  provider_actions: [
    { kind: 'preserve-provider', message: 'Provider ownership moves with the selected issues.' },
  ],
  recurrence_actions: [
    {
      kind: 'retain-series',
      message: 'Future occurrences remain at the original series authority.',
    },
  ],
}
afterEach(cleanup)
it('shows provider and scheduler consequences before confirmation', () => {
  render(IssueTreeMoveStatus, {
    request,
    preview,
    canMutate: true,
    mutationPending: false,
    onConfirm: vi.fn(),
    onDismiss: vi.fn(),
  })
  expect(screen.getByText('Provider ownership moves with the selected issues.')).toBeTruthy()
  expect(
    screen.getByText('Future occurrences remain at the original series authority.'),
  ).toBeTruthy()
})
it('keeps partial outcomes retryable and shows committed evidence', async () => {
  const onConfirm = vi.fn()
  render(IssueTreeMoveStatus, {
    request,
    preview,
    canMutate: true,
    mutationPending: false,
    onConfirm,
    onDismiss: vi.fn(),
    result: {
      state: 'partial',
      request_uid: request.request_uid,
      conflicts: [],
      finalized: [],
      unknown_roots: [request.issue_uid],
      applied: [
        {
          issue_uid: request.issue_uid,
          authority_uid: request.destination_authority_uid,
          project_uid: request.destination_project_uid,
          short_id: 'abcd',
          revision: 2,
          location_generation: 1,
          from_authority_uid: request.source_authority_uid,
          from_project_uid: request.source_project_uid,
          from_short_id: 'bcde',
          committed_event_uid: '01FFFFFFFFFFFFFFFFFFFFFFFF',
          committed_event_hash: 'event-hash',
        },
      ],
    },
  })
  expect(screen.getByText(/Move partial/)).toBeTruthy()
  expect(screen.getByText(/event-hash/)).toBeTruthy()
  await fireEvent.click(screen.getByRole('button', { name: 'Retry exact move' }))
  expect(onConfirm).toHaveBeenCalledOnce()
  expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull()
})
