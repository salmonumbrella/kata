<script lang="ts">
  import { Button, StatusDot } from '@kenn-io/kit-ui'

  import type { CronJob, CronRun } from '../../lib/api/generated'
  import { cronRunDot, cronTriggerLabel, formatCronTime } from '../../lib/cron/format'

  interface Props {
    job: CronJob
    projectName: string
    // The job's own newest runs, read for this job; undefined while loading.
    recentRuns: readonly CronRun[] | undefined
    recentFailed: boolean
    onOpenRun: (runUID: string) => void
    onRetry: () => void
  }

  let { job, projectName, recentRuns, recentFailed, onOpenRun, onRetry }: Props = $props()

  let action = $derived(job.definition.action)
  let latest = $derived(recentRuns?.[0])
</script>

<div class="cron-detail">
  <dl>
    <dt>Project</dt>
    <dd>{projectName}</dd>
    <dt>Trigger</dt>
    <dd>{cronTriggerLabel(job)}</dd>
    <dt>Enabled</dt>
    <dd>{job.definition.enabled ? 'Yes' : 'No'}</dd>
    <dt>Action</dt>
    <dd>{action.kind}</dd>
    {#if typeof action.prompt === 'string'}
      <dt>Prompt</dt>
      <dd class="cron-detail-text">{action.prompt}</dd>
    {/if}
    {#if typeof action.workflow_uid === 'string'}
      <dt>Workflow</dt>
      <dd>{action.workflow_uid}</dd>
    {/if}
    <dt>Overlap</dt>
    <dd>{job.definition.overlap}</dd>
    <dt>Catch-up</dt>
    <dd>{job.definition.catchup}</dd>
    <dt>Latest recorded run</dt>
    <dd>
      {#if recentFailed}
        Unavailable
      {:else if !recentRuns}
        Loading…
      {:else if latest}
        {latest.status} · {formatCronTime(latest.created_at)}
      {:else}
        No runs recorded
      {/if}
    </dd>
  </dl>

  <section class="cron-detail-section" aria-label="Recent runs">
    <h3>Recent runs</h3>
    {#if recentFailed}
      <p role="status">Recent runs could not be loaded.</p>
      <Button size="sm" label="Try again" onclick={onRetry} />
    {:else if !recentRuns}
      <p role="status">Loading recent runs…</p>
    {:else if recentRuns.length === 0}
      <p>No runs recorded for this job.</p>
    {:else}
      <ul>
        {#each recentRuns as run (run.uid)}
          <li>
            <StatusDot status={cronRunDot(run.status)} label={run.status} />
            <button type="button" class="cron-detail-link" onclick={() => onOpenRun(run.uid)}>
              {run.status} · {formatCronTime(run.created_at)}
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</div>

<style>
  p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  ul {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--font-size-sm);
  }
</style>
