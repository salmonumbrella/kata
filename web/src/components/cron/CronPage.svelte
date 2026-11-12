<script lang="ts">
  import { untrack } from 'svelte'
  import {
    Button,
    Chip,
    DetailDrawer,
    EmptyState,
    FilterDropdown,
    PageHeader,
    SegmentedControl,
    StatusDot,
    Table,
  } from '@kenn-io/kit-ui'

  import type { CronJob, CronRun, CronWorkflow } from '../../lib/api/generated'
  import { cronRunDot, cronTriggerLabel, formatCronTime } from '../../lib/cron/format'
  import {
    compareRunsNewestFirst,
    cronReadConcurrency,
    cronReadFailure,
    cronRecentJobRuns,
    cronRunPageSize,
    mapLimit,
    readCronProject,
    readCronRun,
    readCronRuns,
    type CronProject,
    type CronReadFailure,
  } from '../../lib/cron/load'
  import {
    cronRunStatuses,
    cronTabs,
    type CronRoute,
    type CronRunStatus,
    type CronSelection,
  } from '../../lib/router'
  import CronJobDetail from './CronJobDetail.svelte'
  import CronRunDetail from './CronRunDetail.svelte'
  import CronWorkflowDetail from './CronWorkflowDetail.svelte'

  interface Props {
    route: CronRoute
    // Readable projects from the accepted snapshot catalog.
    projects: readonly CronProject[]
    onNavigate: (route: CronRoute) => void
    onOpenIssue: (issueUID: string) => void
  }

  let { route, projects, onNavigate, onOpenIssue }: Props = $props()

  interface ProjectRecords {
    project: CronProject
    failure?: CronReadFailure | undefined
    jobs: CronJob[]
    workflows: CronWorkflow[]
    runs: CronRun[]
  }

  type Detail =
    | { status: 'idle' }
    | { status: 'loading' }
    | { status: 'failed'; failure: CronReadFailure }
    | { status: 'run'; run: CronRun }
    | { status: 'job'; runs: CronRun[] }

  const tabLabels = { jobs: 'Jobs', runs: 'Runs', workflows: 'Workflows' } as const

  let records = $state.raw<ProjectRecords[]>([])
  let loading = $state(true)
  let refreshGeneration = $state(0)
  let detail = $state.raw<Detail>({ status: 'idle' })
  let detailGeneration = $state(0)
  let loadedCatalog: string | undefined
  let loadedDetail = ''

  let catalogKey = $derived(projects.map((project) => `${project.id}:${project.uid}`).join(','))
  let projectNames = $derived(new Map(projects.map((project) => [project.uid, project.name])))
  // A failed refresh keeps the project's last records beside its notice.
  let shown = $derived(
    records.filter(
      (entry) => (!entry.failure || entry.failure === 'failed') && inFilter(entry.project.uid),
    ),
  )
  let failed = $derived(records.filter((entry) => entry.failure && inFilter(entry.project.uid)))
  let jobNames = $derived(
    new Map(
      records.flatMap((entry) =>
        entry.jobs.map((job) => [`${entry.project.uid}:${job.uid}`, job.name] as const),
      ),
    ),
  )
  let jobRows = $derived(
    shown
      .flatMap((entry) => entry.jobs.map((job) => ({ project: entry.project, job })))
      .sort((first, second) => first.job.name.localeCompare(second.job.name)),
  )
  let workflowRows = $derived(
    shown
      .flatMap((entry) => entry.workflows.map((workflow) => ({ project: entry.project, workflow })))
      .sort((first, second) => first.workflow.name.localeCompare(second.workflow.name)),
  )
  let loadedRunRows = $derived(
    shown
      .flatMap((entry) => entry.runs.map((run) => ({ project: entry.project, run })))
      .sort((first, second) => compareRunsNewestFirst(first.run, second.run)),
  )
  let runRows = $derived(
    route.statuses.length === 0
      ? loadedRunRows
      : loadedRunRows.filter((row) => route.statuses.includes(row.run.status as CronRunStatus)),
  )
  let tabCount = $derived(
    route.tab === 'jobs'
      ? jobRows.length
      : route.tab === 'runs'
        ? runRows.length
        : workflowRows.length,
  )
  let filtered = $derived(route.projectUIDs.length > 0 || route.statuses.length > 0)

  let selectionKey = $derived.by(() => {
    const selection = route.selection
    return selection ? `${selection.kind}:${selection.projectUID}:${selection.uid}` : ''
  })
  let selectedProject = $derived.by(() => {
    const selection = route.selection
    return selection ? projects.find((project) => project.uid === selection.projectUID) : undefined
  })
  let selectedProjectID = $derived(selectedProject?.id)
  let selectedRecords = $derived.by(() => {
    const selection = route.selection
    if (!selection) return undefined
    return records.find((entry) => entry.project.uid === selection.projectUID)
  })
  let selectedJob = $derived.by(() => {
    const selection = route.selection
    return selection?.kind === 'job'
      ? selectedRecords?.jobs.find((job) => job.uid === selection.uid)
      : undefined
  })
  let selectedWorkflow = $derived.by(() => {
    const selection = route.selection
    return selection?.kind === 'workflow'
      ? selectedRecords?.workflows.find((workflow) => workflow.uid === selection.uid)
      : undefined
  })
  let drawerTitle = $derived(
    route.selection?.kind === 'run'
      ? 'Run'
      : route.selection?.kind === 'job'
        ? (selectedJob?.name ?? 'Job')
        : (selectedWorkflow?.name ?? 'Workflow'),
  )

  // The catalog read follows the readable projects and Refresh. A different set
  // of projects drops the records it no longer vouches for; the same set keeps
  // each project's records on screen until its new read lands.
  $effect(() => {
    const key = catalogKey
    void refreshGeneration
    const targets = untrack(() => [...projects])
    const abort = new AbortController()
    if (key !== loadedCatalog) {
      loadedCatalog = key
      records = []
    }
    loading = true
    void loadCatalog(targets, abort.signal)
    return () => abort.abort()
  })

  // The open detail has its own lifetime: changing the selection stops its
  // read without touching the catalog read, and Refresh reads it again.
  $effect(() => {
    const key = selectionKey
    const projectID = selectedProjectID
    void refreshGeneration
    void detailGeneration
    const selection = untrack(() => route.selection)
    const abort = new AbortController()
    if (!selection || projectID === undefined || selection.kind === 'workflow') {
      loadedDetail = ''
      detail = { status: 'idle' }
    } else {
      if (key !== loadedDetail) {
        loadedDetail = key
        detail = { status: 'loading' }
      }
      void loadDetail(selection, projectID, abort.signal)
    }
    return () => abort.abort()
  })

  // Each project's result shows as soon as its reads finish, so a slow project
  // never holds back the others. Results keep the catalog's project order.
  async function loadCatalog(targets: CronProject[], signal: AbortSignal): Promise<void> {
    await mapLimit(targets, cronReadConcurrency, async (project) => {
      const next = await readProject(project, signal)
      if (signal.aborted) return
      const settled = refreshed(records.find((entry) => entry.project.uid === project.uid), next)
      const merged = [settled, ...records.filter((entry) => entry.project.uid !== project.uid)]
      const order = targets.map((target) => target.uid)
      records = merged.sort((a, b) => order.indexOf(a.project.uid) - order.indexOf(b.project.uid))
    })
    if (!signal.aborted) loading = false
  }

  async function readProject(project: CronProject, signal: AbortSignal): Promise<ProjectRecords> {
    const empty = { project, jobs: [], workflows: [], runs: [] }
    try {
      if (signal.aborted) return { ...empty, failure: 'failed' }
      const read = await readCronProject(project, signal)
      return { project, ...read }
    } catch (error) {
      return { ...empty, failure: cronReadFailure(error) }
    }
  }

  // A read that failed keeps the last records it replaces, marked failed. A
  // project the connection can no longer read, or that is gone, is cleared.
  function refreshed(previous: ProjectRecords | undefined, next: ProjectRecords): ProjectRecords {
    if (next.failure !== 'failed' || !previous) return next
    return { ...previous, failure: 'failed' }
  }

  async function loadDetail(
    selection: CronSelection,
    projectID: number,
    signal: AbortSignal,
  ): Promise<void> {
    try {
      if (selection.kind === 'run') {
        const run = await readCronRun(projectID, selection.uid, signal)
        if (!signal.aborted) detail = { status: 'run', run }
        return
      }
      const runs = await readCronRuns(projectID, signal, {
        jobUID: selection.uid,
        limit: cronRecentJobRuns,
      })
      if (!signal.aborted) detail = { status: 'job', runs }
    } catch (error) {
      if (!signal.aborted) detail = { status: 'failed', failure: cronReadFailure(error) }
    }
  }

  function openRunJob(): void {
    const selection = route.selection
    const jobUID = detail.status === 'run' ? detail.run.job_uid : undefined
    if (selection && jobUID) select({ kind: 'job', projectUID: selection.projectUID, uid: jobUID })
  }

  function select(selection: CronSelection | undefined): void {
    onNavigate({ ...route, selection })
  }

  function changeTab(value: string): void {
    const tab = cronTabs.find((candidate) => candidate === value)
    if (!tab || tab === route.tab) return
    onNavigate({ tab, projectUIDs: [...route.projectUIDs], statuses: [] })
  }

  function inFilter(projectUID: string): boolean {
    return route.projectUIDs.length === 0 || route.projectUIDs.includes(projectUID)
  }

  function toggle<T extends string>(values: readonly T[], value: T): T[] {
    return values.includes(value)
      ? values.filter((candidate) => candidate !== value)
      : [...values, value]
  }

  function setProjects(projectUIDs: string[]): void {
    onNavigate({ ...route, projectUIDs })
  }

  function setStatuses(statuses: CronRunStatus[]): void {
    onNavigate({ ...route, statuses })
  }

  function clearFilters(): void {
    onNavigate({ ...route, projectUIDs: [], statuses: [] })
  }

  // A job that is no longer listed has been deleted; its UID stays in the drawer.
  function runJobName(projectUID: string, run: CronRun): string {
    if (!run.job_uid) return 'Run'
    return jobNames.get(`${projectUID}:${run.job_uid}`) ?? 'Deleted job'
  }

  function projectName(projectUID: string): string {
    return projectNames.get(projectUID) ?? 'Unknown project'
  }

  function failureMessage(entry: ProjectRecords): string {
    const name = projectName(entry.project.uid)
    if (entry.failure === 'forbidden') {
      return `${name}: this connection cannot read its cron records.`
    }
    if (entry.failure === 'not_found') return `${name}: this project is no longer available.`
    const kept = entry.jobs.length + entry.workflows.length + entry.runs.length > 0
    return kept
      ? `${name}: cron records could not be loaded. Showing the last records read.`
      : `${name}: cron records could not be loaded.`
  }

  function detailFailure(failure: CronReadFailure, kind: CronSelection['kind']): string {
    if (failure === 'forbidden') return 'This connection cannot read this project’s cron records.'
    if (failure === 'not_found' && kind === 'run') return 'Run not found.'
    return kind === 'run' ? 'The run could not be loaded.' : 'Recent runs could not be loaded.'
  }

  function emptyTitle(): string {
    return route.tab === 'jobs'
      ? 'No cron jobs yet'
      : route.tab === 'runs'
        ? 'No runs recorded yet'
        : 'No workflows yet'
  }
</script>

<section class="cron-page" aria-label="Cron">
  <PageHeader level={2} size="medium" title="Cron">
    {#snippet actions()}
      <Button
        size="sm"
        label="Refresh"
        busy={loading && records.length > 0}
        disabled={loading && records.length === 0}
        onclick={() => refreshGeneration++}
      />
    {/snippet}
  </PageHeader>

  <div class="cron-controls">
    <SegmentedControl
      ariaLabel="Cron records"
      options={cronTabs.map((tab) => ({ value: tab, label: tabLabels[tab] }))}
      value={route.tab}
      onchange={changeTab}
    />
    <FilterDropdown
      label="Project"
      badgeCount={route.projectUIDs.length}
      sections={[
        {
          items: projects.map((project) => ({
            id: project.uid,
            label: project.name,
            active: route.projectUIDs.includes(project.uid),
            onSelect: () => setProjects(toggle(route.projectUIDs, project.uid)),
          })),
        },
      ]}
      emptyLabel="No readable projects"
      onDeselectAll={() => setProjects([])}
    />
    {#if route.tab === 'runs'}
      <FilterDropdown
        label="Status"
        badgeCount={route.statuses.length}
        sections={[
          {
            items: cronRunStatuses.map((status) => ({
              id: status,
              label: status,
              active: route.statuses.includes(status),
              onSelect: () => setStatuses(toggle(route.statuses, status)),
            })),
          },
        ]}
        onDeselectAll={() => setStatuses([])}
      />
    {/if}
  </div>

  {#if filtered}
    <div class="cron-chips" aria-label="Active filters">
      {#each route.projectUIDs as projectUID (projectUID)}
        <Chip
          size="sm"
          interactive
          uppercase={false}
          ariaLabel={`Remove project filter ${projectName(projectUID)}`}
          onclick={() => setProjects(toggle(route.projectUIDs, projectUID))}
        >
          {projectName(projectUID)} ×
        </Chip>
      {/each}
      {#each route.statuses as status (status)}
        <Chip
          size="sm"
          interactive
          uppercase={false}
          ariaLabel={`Remove status filter ${status}`}
          onclick={() => setStatuses(toggle(route.statuses, status))}
        >
          {status} ×
        </Chip>
      {/each}
    </div>
  {/if}

  {#each failed as entry (entry.project.uid)}
    <p class="cron-notice" role="status">{failureMessage(entry)}</p>
  {/each}

  {#if projects.length === 0}
    <EmptyState
      title="No readable projects"
      description="Cron records belong to projects. This connection cannot read any."
    />
  {:else if loading && records.length === 0}
    <p class="cron-status" role="status">Loading cron records…</p>
  {:else if tabCount === 0}
    {#if filtered && records.some((entry) => !entry.failure)}
      <EmptyState title="Nothing matches these filters">
        <Button size="sm" label="Clear filters" onclick={clearFilters} />
      </EmptyState>
    {:else if shown.length > 0}
      <EmptyState title={emptyTitle()} description="Records appear here once they are created." />
    {/if}
  {:else if route.tab === 'jobs'}
    <div class="cron-table">
      <Table bare unsorted ariaLabel="Cron jobs">
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Project</th>
            <th scope="col">Trigger</th>
            <th scope="col">Enabled</th>
          </tr>
        </thead>
        <tbody>
          {#each jobRows as { project, job } (`${project.uid}:${job.uid}`)}
            <tr>
              <td>
                <button
                  type="button"
                  class="cron-row-link"
                  onclick={() => select({ kind: 'job', projectUID: project.uid, uid: job.uid })}
                >
                  {job.name}
                </button>
              </td>
              <td>{projectName(project.uid)}</td>
              <td>{cronTriggerLabel(job)}</td>
              <td>{job.definition.enabled ? 'Yes' : 'No'}</td>
            </tr>
          {/each}
        </tbody>
      </Table>
    </div>
  {:else if route.tab === 'runs'}
    <div class="cron-table">
      <Table bare unsorted ariaLabel="Cron runs">
        <thead>
          <tr>
            <th scope="col">Status</th>
            <th scope="col">Job</th>
            <th scope="col">Project</th>
            <th scope="col">Created</th>
          </tr>
        </thead>
        <tbody>
          {#each runRows as { project, run } (`${project.uid}:${run.uid}`)}
            <tr>
              <td>
                <span class="cron-run-status">
                  <StatusDot status={cronRunDot(run.status)} label={run.status} />
                  {run.status}
                </span>
              </td>
              <td>
                <button
                  type="button"
                  class="cron-row-link"
                  onclick={() => select({ kind: 'run', projectUID: project.uid, uid: run.uid })}
                >
                  {runJobName(project.uid, run)}
                </button>
              </td>
              <td>{projectName(project.uid)}</td>
              <td><time datetime={run.created_at}>{formatCronTime(run.created_at)}</time></td>
            </tr>
          {/each}
        </tbody>
      </Table>
    </div>
  {:else}
    <div class="cron-table">
      <Table bare unsorted ariaLabel="Cron workflows">
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Project</th>
            <th scope="col">Steps</th>
          </tr>
        </thead>
        <tbody>
          {#each workflowRows as { project, workflow } (`${project.uid}:${workflow.uid}`)}
            <tr>
              <td>
                <button
                  type="button"
                  class="cron-row-link"
                  onclick={() =>
                    select({ kind: 'workflow', projectUID: project.uid, uid: workflow.uid })}
                >
                  {workflow.name}
                </button>
              </td>
              <td>{projectName(project.uid)}</td>
              <td>{workflow.definition.steps.length}</td>
            </tr>
          {/each}
        </tbody>
      </Table>
    </div>
  {/if}

  {#if route.tab === 'runs' && shown.length > 0}
    <footer class="cron-history">
      <p>Showing the latest {cronRunPageSize} runs per project. Filters apply to loaded runs.</p>
    </footer>
  {/if}
</section>

{#if route.selection}
  {@const selection = route.selection}
  <DetailDrawer title={drawerTitle} width="min(480px, 100vw)" onclose={() => select(undefined)}>
    {#if !selectedProject}
      <p class="cron-drawer-status" role="status">
        This project is not readable from the current connection.
      </p>
    {:else if selection.kind === 'run'}
      {#if detail.status === 'run' && detail.run.uid === selection.uid}
        <CronRunDetail
          run={detail.run}
          projectName={selectedProject.name}
          jobName={detail.run.job_uid
            ? jobNames.get(`${selection.projectUID}:${detail.run.job_uid}`)
            : undefined}
          onOpenJob={detail.run.job_uid ? openRunJob : undefined}
          {onOpenIssue}
        />
      {:else if detail.status === 'failed'}
        <div class="cron-drawer-status" role="status">
          <p>{detailFailure(detail.failure, 'run')}</p>
          {#if detail.failure === 'failed'}
            <Button size="sm" label="Try again" onclick={() => detailGeneration++} />
          {/if}
        </div>
      {:else}
        <p class="cron-drawer-status" role="status">Loading run…</p>
      {/if}
    {:else if selection.kind === 'job'}
      {#if selectedJob}
        <CronJobDetail
          job={selectedJob}
          projectName={selectedProject.name}
          recentRuns={detail.status === 'job' ? detail.runs : undefined}
          recentFailed={detail.status === 'failed'}
          onOpenRun={(uid) => select({ kind: 'run', projectUID: selection.projectUID, uid })}
          onRetry={() => detailGeneration++}
        />
      {:else if selectedRecords?.failure}
        <p class="cron-drawer-status" role="status">{failureMessage(selectedRecords)}</p>
      {:else if loading || !selectedRecords}
        <p class="cron-drawer-status" role="status">Loading job…</p>
      {:else}
        <p class="cron-drawer-status" role="status">Job not found.</p>
      {/if}
    {:else if selectedWorkflow}
      <CronWorkflowDetail workflow={selectedWorkflow} projectName={selectedProject.name} />
    {:else if selectedRecords?.failure}
      <p class="cron-drawer-status" role="status">{failureMessage(selectedRecords)}</p>
    {:else if loading || !selectedRecords}
      <p class="cron-drawer-status" role="status">Loading workflow…</p>
    {:else}
      <p class="cron-drawer-status" role="status">Workflow not found.</p>
    {/if}
  </DetailDrawer>
{/if}

<style>
  .cron-page {
    flex: 1;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-5);
    background: var(--bg-primary);
  }

  .cron-controls,
  .cron-chips,
  .cron-notice,
  .cron-history {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
  }

  .cron-notice {
    margin: 0;
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .cron-status,
  .cron-history p {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .cron-table {
    min-width: 0;
    overflow-x: auto;
  }

  .cron-table :global(table) {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--font-size-sm);
  }

  th,
  td {
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
    text-align: left;
    white-space: nowrap;
  }

  th {
    color: var(--text-muted);
    font-weight: var(--font-weight-medium);
  }

  .cron-run-status {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .cron-row-link {
    border: 0;
    padding: 0;
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-align: left;
    text-decoration: underline;
    text-underline-offset: 3px;
    cursor: pointer;
  }

  .cron-drawer-status {
    display: grid;
    gap: var(--space-3);
    justify-items: start;
    margin: 0;
    padding: var(--space-4) var(--space-5);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .cron-drawer-status p {
    margin: 0;
  }

  /* Shared by the run, job and workflow detail bodies inside the drawer. */
  :global(.cron-detail) {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-4) var(--space-5);
    overflow-wrap: anywhere;
  }

  :global(.cron-detail dl) {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--space-2) var(--space-4);
    margin: 0;
    font-size: var(--font-size-sm);
  }

  :global(.cron-detail dt) {
    color: var(--text-muted);
  }

  :global(.cron-detail dd) {
    margin: 0;
  }

  :global(.cron-detail h3) {
    margin: 0;
    font-size: var(--font-size-md);
  }

  :global(.cron-detail-text) {
    white-space: pre-wrap;
  }

  :global(.cron-detail-section) {
    display: grid;
    gap: var(--space-3);
  }

  :global(.cron-detail-link) {
    border: 0;
    padding: 0;
    background: transparent;
    color: var(--accent-blue);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }

  @media (max-width: 640px) {
    .cron-page {
      padding: var(--space-3);
    }
  }
</style>
