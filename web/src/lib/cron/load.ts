import {
  listCronJobs,
  listCronRuns,
  listCronWorkflows,
  showCronRun,
  type CronJob,
  type CronRun,
  type CronWorkflow,
} from '../api/generated'
import type { KataProjectSummary } from '../kata/types'

// Run history is one bounded newest page per project.
export const cronRunPageSize = 50
export const cronRecentJobRuns = 5
// Never more reads in flight than this, however many projects are readable.
export const cronReadConcurrency = 4

export type CronProject = Pick<KataProjectSummary, 'id' | 'uid' | 'name'>

export type CronReadFailure = 'forbidden' | 'not_found' | 'failed'

export class CronReadError extends Error {
  constructor(readonly failure: CronReadFailure) {
    super(`cron read ${failure}`)
  }
}

export interface CronProjectRecords {
  jobs: CronJob[]
  workflows: CronWorkflow[]
  runs: CronRun[]
}

export function cronReadFailure(error: unknown): CronReadFailure {
  return error instanceof CronReadError ? error.failure : 'failed'
}

function failureFor(status: number): CronReadError {
  return new CronReadError(status === 403 ? 'forbidden' : status === 404 ? 'not_found' : 'failed')
}

// One project's reads run one after another, so the catalog's concurrency
// limit is also the limit on requests in flight.
export async function readCronProject(
  project: CronProject,
  signal: AbortSignal,
): Promise<CronProjectRecords> {
  const options = { signal, retry: true as const }
  const jobs = await listCronJobs({ projectId: project.id }, undefined, options)
  if (jobs.status !== 200) throw failureFor(jobs.status)
  const workflows = await listCronWorkflows({ projectId: project.id }, undefined, options)
  if (workflows.status !== 200) throw failureFor(workflows.status)
  const runs = await readCronRuns(project.id, signal)
  return { jobs: jobs.data.jobs, workflows: workflows.data.workflows, runs }
}

export async function readCronRuns(
  projectID: number,
  signal: AbortSignal,
  query: { jobUID?: string | undefined; limit?: number } = {},
): Promise<CronRun[]> {
  const response = await listCronRuns(
    { projectId: projectID },
    {
      limit: query.limit ?? cronRunPageSize,
      ...(query.jobUID ? { job_uid: query.jobUID } : {}),
    },
    { signal, retry: true },
  )
  if (response.status !== 200) throw failureFor(response.status)
  return response.data.runs
}

export async function readCronRun(
  projectID: number,
  runUID: string,
  signal: AbortSignal,
): Promise<CronRun> {
  const options = { signal, retry: true as const }
  const response = await showCronRun({ projectId: projectID, runUid: runUID }, options)
  if (response.status !== 200) throw failureFor(response.status)
  return response.data.run
}

// Runs task over items with at most limit calls pending at once and keeps
// the input order in the results. Tasks report their own failures.
export async function mapLimit<T, R>(
  items: readonly T[],
  limit: number,
  task: (item: T) => Promise<R>,
): Promise<R[]> {
  const results: R[] = []
  let next = 0
  const worker = async (): Promise<void> => {
    while (next < items.length) {
      const index = next
      next += 1
      results[index] = await task(items[index]!)
    }
  }
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker))
  return results
}

// Newest first across projects. Each project's page already arrives in this
// order; equal instants fall back to the ULID, which also sorts by time.
export function compareRunsNewestFirst(first: CronRun, second: CronRun): number {
  const difference = Date.parse(second.created_at) - Date.parse(first.created_at)
  if (difference) return difference
  return first.uid < second.uid ? 1 : first.uid > second.uid ? -1 : 0
}
