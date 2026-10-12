<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import type {
    MoveTreeRequest,
    MoveTreePreviewResponse,
    MoveTreeResultResponse,
  } from '../lib/api/generated'
  interface Props {
    request: MoveTreeRequest
    preview: MoveTreePreviewResponse
    result?: MoveTreeResultResponse | undefined
    canMutate: boolean
    mutationPending: boolean
    onConfirm: () => void | Promise<void>
    onDismiss: () => void
  }
  let { request, preview, result, canMutate, mutationPending, onConfirm, onDismiss }: Props =
    $props()
</script>

<section aria-label="Issue tree move" role="status">
  <p>
    {result ? `Move ${result.state}` : `Preview: ${preview.members.length} issues`}. Request {request.request_uid}
  </p>
  {#each preview.provider_actions as action, index (index)}<p>{action.message}</p>{/each}
  {#each preview.recurrence_actions as action, index (index)}<p>{action.message}</p>{/each}
  <details>
    <summary>Selected issues and relationships</summary>
    {#each preview.members as member (member.issue_uid)}<p>
        {member.issue_uid} · {member.authority_uid}/{member.project_uid}/{member.short_id}
      </p>{/each}
    {#each preview.relations as relation, index (index)}<p>
        {relation.type}: {relation.from_issue_uid} → {relation.to_issue_uid}
      </p>{/each}
  </details>
  {#each preview.blockers as blocker, index (index)}
    <p>{blocker.message}</p>
  {/each}
  {#if result}
    {#each result.applied as moved (moved.issue_uid)}
      <p>
        {moved.issue_uid} → {moved.project_uid}/{moved.short_id} · event {moved.committed_event_uid ??
          'pending'}
        {moved.committed_event_hash ?? ''}
      </p>
    {/each}
    {#each result.conflicts as conflict, index (index)}<p>
        {conflict.message}
      </p>{/each}
    {#each result.unknown_roots as root (root)}<p>Unresolved: {root}</p>{/each}
    <p>{result.retry_action ?? ''}</p>
  {/if}
  {#if !result || result.state === 'partial' || result.state === 'outcome-unknown'}
    <Button
      disabled={!canMutate || mutationPending || preview.blockers.length > 0}
      onclick={() => void onConfirm()}>{result ? 'Retry exact move' : 'Confirm move'}</Button
    >
  {/if}
  {#if !result || result.state === 'completed' || result.state === 'conflict'}
    <Button disabled={mutationPending} onclick={onDismiss}
      >{result ? 'Dismiss' : 'Cancel preview'}</Button
    >
  {/if}
</section>
