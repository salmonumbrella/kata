<script lang="ts">
  import { Button, StatusDot } from '@kenn-io/kit-ui'

  import type { CronRun } from '../../lib/api/generated'
  import { cronRunDot, cronTokensLabel, formatCronTime } from '../../lib/cron/format'

  interface Props {
    run: CronRun
    projectName: string
    jobName?: string | undefined
    onOpenJob?: (() => void) | undefined
    onOpenIssue: (issueUID: string) => void
  }

  let {
    run,
    projectName,
    jobName = undefined,
    onOpenJob = undefined,
    onOpenIssue,
  }: Props = $props()

  let tokens = $derived(cronTokensLabel(run))
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
      <dt>Tokens</dt>
      <dd>{tokens}</dd>
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
</div>

<style>
  .cron-detail-summary {
    margin: 0;
    white-space: pre-wrap;
    font-size: var(--font-size-md);
  }

  .cron-detail-status {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: 0;
  }
</style>
