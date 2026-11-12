<script lang="ts">
  import type { CronJob } from '../../lib/api/generated'
  import { cronTriggerLabel } from '../../lib/cron/format'

  interface Props {
    job: CronJob
    projectName: string
  }

  let { job, projectName }: Props = $props()

  let action = $derived(job.definition.action)
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
  </dl>
</div>
