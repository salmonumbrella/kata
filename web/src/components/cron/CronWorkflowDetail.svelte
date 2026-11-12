<script lang="ts">
  import type { CronWorkflow } from '../../lib/api/generated'

  interface Props {
    workflow: CronWorkflow
    projectName: string
  }

  let { workflow, projectName }: Props = $props()
</script>

<div class="cron-detail">
  <dl>
    <dt>Project</dt>
    <dd>{projectName}</dd>
    <dt>About</dt>
    <dd>{workflow.definition.about || 'No description'}</dd>
    {#if workflow.definition.input}
      <dt>Input</dt>
      <dd class="cron-detail-text">{workflow.definition.input}</dd>
    {/if}
  </dl>

  <section class="cron-detail-steps" aria-label="Steps">
    <h3>Steps</h3>
    <ol>
      {#each workflow.definition.steps as step (step.key)}
        <li>
          <strong>{step.key}</strong>
          <span class="cron-detail-muted">{step.kind}</span>
          {#if step.command ?? step.prompt}
            <code>{step.command ?? step.prompt}</code>
          {/if}
          {#if step.after?.length}
            <span class="cron-detail-muted">After {step.after.join(', ')}</span>
          {/if}
        </li>
      {/each}
    </ol>
  </section>
</div>

<style>
  .cron-detail {
    display: grid;
    gap: var(--space-5);
    padding: var(--space-4) var(--space-5);
    overflow-wrap: anywhere;
  }

  dl {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--space-2) var(--space-4);
    margin: 0;
    font-size: var(--font-size-sm);
  }

  dt {
    color: var(--text-muted);
  }

  dd {
    margin: 0;
  }

  .cron-detail-text,
  code {
    white-space: pre-wrap;
  }

  .cron-detail-steps {
    display: grid;
    gap: var(--space-3);
  }

  h3 {
    margin: 0;
    font-size: var(--font-size-md);
  }

  ol {
    display: grid;
    gap: var(--space-3);
    margin: 0;
    padding-left: var(--space-5);
    font-size: var(--font-size-sm);
  }

  li {
    display: grid;
    gap: var(--space-1);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .cron-detail-muted {
    color: var(--text-muted);
  }
</style>
