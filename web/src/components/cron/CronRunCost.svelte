<script lang="ts">
  import { untrack } from 'svelte'
  import { Button } from '@kenn-io/kit-ui'

  import {
    getCronCapabilities,
    getCronRunUsage,
    type CronRun,
    type RunUsage,
  } from '../../lib/api/generated'
  import { formatCronTime } from '../../lib/cron/format'
  import { runCostText, sessionStateText, usageFailureText } from '../../lib/cron/usage'
  import { safeSessionURL } from '../../lib/history/sessionLink'

  // The parent keys this component on daemon, authority, project, run and
  // read-only state, so any of those changing mounts a fresh, empty section.
  interface Props {
    projectID: number
    projectUID: string
    run: CronRun
    // The connection is the anonymous read-only reader, which cannot read usage.
    anonymous: boolean
  }

  let { projectID, projectUID, run, anonymous }: Props = $props()

  type Cost =
    | { status: 'hidden' }
    | { status: 'anonymous' }
    | { status: 'reading' }
    | { status: 'failed'; message: string }
    | { status: 'ready'; usage: RunUsage; revision: number }

  // Hidden until the daemon says usage is on: a project without a usage
  // source, or a capability read that fails before that, shows no section.
  let cost = $state.raw<Cost>({ status: 'hidden' })
  let pending = $state(false)
  let generation = 0
  let abort: AbortController | undefined

  $effect(() => {
    untrack(() => void load(false))
    return () => {
      generation += 1
      abort?.abort()
    }
  })

  async function load(refresh: boolean): Promise<void> {
    abort?.abort()
    const controller = new AbortController()
    abort = controller
    const current = ++generation
    const { uid: runUID, revision } = run
    const project = { id: projectID, uid: projectUID }
    // A continuation may only assign while it is still the newest read for the
    // same project and run.
    const live = () =>
      current === generation &&
      !controller.signal.aborted &&
      run.uid === runUID &&
      projectID === project.id &&
      projectUID === project.uid
    pending = true
    if (refresh) cost = { status: 'reading' }
    try {
      const options = { signal: controller.signal }
      const capabilities = await getCronCapabilities({ projectId: project.id }, options)
      if (!live()) return
      if (capabilities.status === 401) {
        cost = { status: 'hidden' }
        return
      }
      if (capabilities.status !== 200 || capabilities.data.project_uid !== project.uid) {
        cost = refresh ? { status: 'failed', message: usageFailureText(0) } : { status: 'hidden' }
        return
      }
      if (capabilities.data.usage.source !== 'agentsview') {
        cost = { status: 'hidden' }
        return
      }
      if (anonymous) {
        cost = { status: 'anonymous' }
        return
      }
      cost = { status: 'reading' }
      const usage = await getCronRunUsage(
        { projectId: project.id, runUid: runUID },
        { refresh },
        options,
      )
      if (!live()) return
      if (usage.status === 401) {
        cost = { status: 'hidden' }
      } else if (usage.status !== 200 || usage.data.run_uid !== runUID) {
        cost = { status: 'failed', message: usageFailureText(usage.status) }
      } else {
        cost = { status: 'ready', usage: usage.data, revision }
      }
    } catch {
      if (live() && cost.status !== 'hidden') {
        cost = { status: 'failed', message: usageFailureText(0) }
      }
    } finally {
      if (current === generation) pending = false
    }
  }

  function sessionLine(session: RunUsage['sessions'][number]): string {
    const step = session.step ? ` ${session.step}` : ''
    return `${session.agent} ${session.session_id.slice(0, 8)}${step}: ${sessionStateText(session)}`
  }
</script>

{#if cost.status !== 'hidden'}
  <section class="cron-detail-section" aria-label="Cost">
    <div class="cron-cost-heading">
      <h3>Cost</h3>
      <Button
        size="sm"
        label="Refresh cost"
        busy={pending}
        disabled={pending}
        onclick={() => void load(true)}
      />
    </div>
    {#if cost.status === 'anonymous'}
      <p role="status">Cost needs a signed-in session</p>
    {:else if cost.status === 'reading'}
      <p role="status">Reading cost…</p>
    {:else if cost.status === 'failed'}
      <p role="status">{cost.message}</p>
    {:else if cost.revision !== run.revision}
      <p role="status">Run changed. Refresh cost.</p>
    {:else}
      <p class="cron-cost-total">{runCostText(cost.usage)}</p>
      {#if cost.usage.sessions_incomplete}
        <p>The recorded session list is incomplete; the total may omit usage.</p>
      {/if}
      <p>Checked {formatCronTime(cost.usage.fetched_at)}</p>
      {#if cost.usage.sessions.length > 0}
        <ul>
          {#each cost.usage.sessions as session, index (`${index}:${session.session_id}`)}
            {@const url = safeSessionURL(session.url)}
            <li>
              <span title={session.session_id}>{sessionLine(session)}</span>
              {#if url}
                <a
                  href={url}
                  class="cron-detail-link"
                  target="_blank"
                  rel="noopener noreferrer">Open in AgentsView</a
                >
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
    {/if}
  </section>
{/if}

<style>
  .cron-cost-heading {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .cron-cost-total {
    color: var(--text-primary);
    font-size: var(--font-size-md);
  }

  ul {
    display: grid;
    gap: var(--space-2);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--font-size-sm);
  }

  li {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
</style>
