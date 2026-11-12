<script lang="ts">
  import { Button, StatusDot } from '@kenn-io/kit-ui'

  import type { CronRun } from '../../lib/api/generated'
  import { cronRunDot, cronTokensLabel, formatCronTime } from '../../lib/cron/format'
  import CronRunCost from './CronRunCost.svelte'

  interface Props {
    run: CronRun
    projectID: number
    projectUID: string
    projectName: string
    readOnly: boolean
    anonymous: boolean
    jobName?: string | undefined
    onOpenJob?: (() => void) | undefined
    onOpenIssue: (issueUID: string) => void
  }

  let {
    run,
    projectID,
    projectUID,
    projectName,
    readOnly,
    anonymous,
    jobName = undefined,
    onOpenJob = undefined,
    onOpenIssue,
  }: Props = $props()

  let tokens = $derived(cronTokensLabel(run))
  let sessions = $derived(run.summary.sessions ?? [])
</script>

<div class="cron-detail">
  <p class="cron-detail-summary">{run.summary.message || 'No summary recorded.'}</p>
  <p class="cron-detail-status">
    <StatusDot status={cronRunDot(run.status)} label={run.status} />
    <strong>{run.status}</strong>
  </p>
  <dl>
    <dt>Project</dt>
    <dd>{projectName}</dd>
    <dt>Created</dt>
    <dd><time datetime={run.created_at}>{formatCronTime(run.created_at)}</time></dd>
    <dt>Started</dt>
    <dd>{formatCronTime(run.started_at)}</dd>
    <dt>Ended</dt>
    <dd>{formatCronTime(run.ended_at)}</dd>
    <dt>Recorded by</dt>
    <dd>{run.actor}</dd>
    {#if run.executor_label}
      <dt>Executor</dt>
      <dd>{run.executor_label}</dd>
    {/if}
    {#if run.teammate}
      <dt>Teammate</dt>
      <dd>{run.teammate}</dd>
    {/if}
    {#if tokens}
      <dt>Recorded by adapter</dt>
      <dd>{tokens}</dd>
    {/if}
    {#if sessions.length > 0 || run.summary.sessions_incomplete}
      <dt>Sessions</dt>
      <dd>
        <ul class="cron-run-sessions">
          {#each sessions as session, index (`${index}:${session.session_id}`)}
            <li title={session.session_id}>
              {session.agent}
              {session.session_id.slice(0, 8)}{session.step ? ` · ${session.step}` : ''}
            </li>
          {/each}
        </ul>
        {#if run.summary.sessions_incomplete}
          <span class="cron-run-incomplete">Session list incomplete</span>
        {/if}
      </dd>
    {/if}
    <dt>Job</dt>
    <dd>
      {#if run.job_uid && onOpenJob}
        <button type="button" class="cron-detail-link" onclick={onOpenJob}>
          {jobName ?? run.job_uid}
        </button>
      {:else}
        {run.job_uid ?? 'No job'}
      {/if}
    </dd>
    <dt>Issue</dt>
    <dd>
      {#if run.issue_uid}
        {@const issueUID = run.issue_uid}
        <Button size="sm" label="Open issue" onclick={() => onOpenIssue(issueUID)} />
      {:else}
        No linked issue
      {/if}
    </dd>
  </dl>
  {#key `${projectID}:${projectUID}:${run.uid}:${readOnly}:${anonymous}`}
    <CronRunCost {projectID} {projectUID} {run} {anonymous} />
  {/key}
</div>

<style>
  .cron-detail-summary {
    margin: 0;
    white-space: pre-wrap;
    font-size: var(--font-size-md);
  }

  .cron-run-sessions {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .cron-run-incomplete {
    color: var(--text-muted);
  }

  .cron-detail-status {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0;
  }
</style>
