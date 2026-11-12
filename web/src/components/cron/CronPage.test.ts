import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { setGeneratedFetch } from '../../lib/api/client'
import type { CronRoute } from '../../lib/router'
import CronPage from './CronPage.svelte'

interface Project {
  id: number
  uid: string
  name: string
}

const exampleProject: Project = {
  id: 7,
  uid: '01J00000000000000000000002',
  name: 'example-project',
}
const spokeProject: Project = { id: 8, uid: '01J00000000000000000000003', name: 'spoke-project' }
const runA = '01K00000000000000000000003'
const runB = '01K00000000000000000000002'
const runC = '01K00000000000000000000001'
const jobUID = '01K0000000000000000000000J'

function job(project: Project, name: string, uid = jobUID) {
  return {
    id: 1,
    uid,
    name,
    project_id: project.id,
    author: 'author-a',
    revision: 1,
    created_at: '2026-10-01T08:00:00Z',
    updated_at: '2026-10-01T08:00:00Z',
    definition_event_uid: '01K0000000000000000000000E',
    definition_hlc: { physical: 1, logical: 0 },
    definition: {
      version: 1,
      kind: 'job',
      enabled: true,
      trigger: { kind: 'interval', interval_seconds: 3600 },
      action: { kind: 'execute', prompt: 'Review open work' },
      overlap: 'forbid',
      catchup: 'latest',
    },
  }
}

function run(project: Project, uid: string, minute: number, extra: Record<string, unknown> = {}) {
  return {
    id: minute,
    uid,
    project_id: project.id,
    revision: 1,
    actor: 'worker',
    status: 'succeeded',
    job_uid: jobUID,
    summary: { version: 1, message: `Run ${uid}` },
    created_at: `2026-10-01T09:${String(minute).padStart(2, '0')}:00Z`,
    updated_at: `2026-10-01T09:${String(minute).padStart(2, '0')}:00Z`,
    ...extra,
  }
}

type Handler = (url: URL) => Response | Promise<Response>

function urlOf(input: RequestInfo | URL): URL {
  return new URL(String(input), 'http://127.0.0.1')
}

function jobRead(url: URL): boolean {
  return url.searchParams.get('job_uid') === jobUID
}

function serve(handler: Handler) {
  const fetcher = vi.fn<typeof fetch>(async (input) => handler(urlOf(input)))
  setGeneratedFetch(fetcher)
  return fetcher
}

function requested(fetcher: ReturnType<typeof serve>, path: string): URL[] {
  return fetcher.mock.calls.map(([input]) => urlOf(input)).filter((url) => url.pathname === path)
}

function cronPath(project: Project, tail: string): string {
  return `/api/v1/projects/${project.id}/cron/${tail}`
}

function emptyCatalog(url: URL): Response | undefined {
  if (url.pathname.endsWith('/cron/jobs')) return Response.json({ jobs: [] })
  if (url.pathname.endsWith('/cron/workflows')) return Response.json({ workflows: [] })
  if (url.pathname.endsWith('/cron/runs')) return Response.json({ runs: [] })
  return undefined
}

function notFound(): Response {
  return Response.json({ error: { code: 'not_found' } }, { status: 404 })
}

function props(route: Partial<CronRoute>, projects: Project[]) {
  return {
    route: { tab: 'jobs' as const, projectUIDs: [], statuses: [], ...route },
    projects,
    onNavigate: vi.fn(),
    onOpenIssue: vi.fn(),
  }
}

afterEach(cleanup)

describe('CronPage', () => {
  it('reads every readable project with at most four reads in flight and keeps readable projects beside failed ones', async () => {
    const projects = Array.from({ length: 6 }, (_, index) => ({
      id: index + 1,
      uid: `01J0000000000000000000000${index + 1}`,
      name: `example-project-${index + 1}`,
    }))
    let inFlight = 0
    let mostInFlight = 0
    const refreshFailures = new Map<string, number>()
    // One project's first read stays pending until the test releases it.
    let releaseSlow: () => void = () => {}
    const slow = new Promise<void>((resolve) => (releaseSlow = resolve))
    let slowReads = 0
    serve(async (url) => {
      inFlight += 1
      mostInFlight = Math.max(mostInFlight, inFlight)
      await new Promise((resolve) => setTimeout(resolve, 1))
      if (url.pathname === cronPath(projects[4]!, 'jobs') && slowReads++ === 0) await slow
      inFlight -= 1
      const failure = refreshFailures.get(url.pathname)
      if (failure) return Response.json({ error: { code: 'example' } }, { status: failure })
      if (url.pathname === cronPath(projects[2]!, 'jobs')) {
        return Response.json({ error: { code: 'forbidden' } }, { status: 403 })
      }
      if (url.pathname === cronPath(projects[3]!, 'jobs')) {
        return Response.json({ error: { code: 'invalid' } }, { status: 400 })
      }
      if (url.pathname.endsWith('/cron/jobs')) {
        const project = projects.find((candidate) => url.pathname === cronPath(candidate, 'jobs'))!
        return Response.json({ jobs: [job(project, `Review ${project.name}`)] })
      }
      return emptyCatalog(url) ?? notFound()
    })

    render(CronPage, { props: props({}, projects) })

    // A slow project does not hold back the projects that already answered.
    expect(await screen.findByRole('button', { name: 'Review example-project-1' })).not.toBeNull()
    expect(await screen.findByRole('button', { name: 'Review example-project-6' })).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Review example-project-5' })).toBeNull()
    releaseSlow()
    expect(await screen.findByRole('button', { name: 'Review example-project-5' })).not.toBeNull()
    expect(
      screen.getByText('example-project-3: this connection cannot read its cron records.'),
    ).not.toBeNull()
    expect(screen.getByText('example-project-4: cron records could not be loaded.')).not.toBeNull()
    expect(mostInFlight).toBe(4)

    // A failed Refresh keeps the last records it read and says they failed to
    // refresh; a project the connection can no longer read is cleared.
    const refresh = async () => {
      const button = screen.getByRole('button', { name: 'Refresh' })
      await waitFor(() => expect(button.getAttribute('aria-busy')).toBeNull())
      await fireEvent.click(button)
    }
    refreshFailures.set(cronPath(projects[0]!, 'jobs'), 400)
    refreshFailures.set(cronPath(projects[1]!, 'jobs'), 403)
    await refresh()
    await screen.findByText(/^example-project-1: cron records could not be loaded\./)
    expect(screen.getByRole('button', { name: 'Review example-project-1' })).not.toBeNull()
    // Each project's refresh lands on its own, so wait for project 2's answer.
    expect(
      await screen.findByText('example-project-2: this connection cannot read its cron records.'),
    ).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Review example-project-2' })).toBeNull()

    // The next Refresh recovers the project.
    refreshFailures.clear()
    await refresh()
    await waitFor(() =>
      expect(screen.queryByText(/^example-project-1: cron records could not be loaded/)).toBeNull(),
    )
    expect(screen.getByRole('button', { name: 'Review example-project-1' })).not.toBeNull()
  })

  it('shows only the newest bounded page of runs per project in the daemon order', async () => {
    // Imported runs can differ by nanoseconds within one millisecond. The
    // daemon orders them by the full time, which here opposes their UIDs.
    const newer = '2026-10-01T09:03:00.000000002Z'
    const older = '2026-10-01T09:03:00.000000001Z'
    const fetcher = serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'jobs')) {
        return Response.json({ jobs: [job(exampleProject, 'Daily review')] })
      }
      if (url.pathname === cronPath(exampleProject, 'runs')) {
        return Response.json({
          runs: [
            run(exampleProject, runC, 3, { status: 'failed', created_at: newer }),
            run(exampleProject, runA, 3, { created_at: older }),
          ],
          next_before_uid: runA,
        })
      }
      return emptyCatalog(url) ?? notFound()
    })

    render(CronPage, { props: props({ tab: 'runs' }, [exampleProject]) })

    expect(
      await screen.findByText(
        'Showing the latest 50 runs per project. Filters apply to loaded runs.',
      ),
    ).not.toBeNull()
    expect(screen.getAllByRole('button', { name: 'Daily review' })).toHaveLength(2)
    const rows = screen.getByRole('table', { name: 'Cron runs' }).querySelectorAll('tbody tr')
    expect(Array.from(rows, (row) => /failed/.test(row.textContent ?? ''))).toEqual([true, false])
    const runsPath = cronPath(exampleProject, 'runs')
    expect(requested(fetcher, runsPath).map((url) => url.searchParams.toString())).toEqual([
      'limit=50',
    ])
  })

  it('filters loaded runs by status and tells a filtered page from an empty one', async () => {
    serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'runs')) {
        return Response.json({ runs: [run(exampleProject, runA, 3)] })
      }
      return emptyCatalog(url) ?? notFound()
    })
    const filtered = props({ tab: 'runs', statuses: ['failed'] }, [exampleProject])
    render(CronPage, { props: filtered })

    expect(await screen.findByText('Nothing matches these filters')).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(filtered.onNavigate).toHaveBeenCalledWith({
      tab: 'runs',
      projectUIDs: [],
      statuses: [],
    })

    cleanup()
    serve((url) => emptyCatalog(url) ?? notFound())
    render(CronPage, { props: props({ tab: 'runs' }, [exampleProject]) })
    expect(await screen.findByText('No runs recorded yet')).not.toBeNull()

    // An empty project that answered first does not make the page empty while
    // another project's read is still pending.
    cleanup()
    let releaseSpoke: () => void = () => {}
    const spoke = new Promise<void>((resolve) => (releaseSpoke = resolve))
    serve(async (url) => {
      if (url.pathname === cronPath(spokeProject, 'jobs')) {
        return Response.json({ jobs: [job(spokeProject, 'Spoke review')] })
      }
      if (url.pathname === cronPath(spokeProject, 'runs')) {
        await spoke
        return Response.json({ runs: [run(spokeProject, runA, 3)] })
      }
      return emptyCatalog(url) ?? notFound()
    })
    render(CronPage, { props: props({ tab: 'runs' }, [exampleProject, spokeProject]) })
    // Refresh shows busy only once a project's records are on the page.
    const refresh = screen.getByRole('button', { name: 'Refresh' })
    await waitFor(() => expect(refresh.getAttribute('aria-busy')).not.toBeNull())
    expect(screen.getByText('Loading cron records…')).not.toBeNull()
    expect(screen.queryByText('No runs recorded yet')).toBeNull()
    releaseSpoke()
    expect(await screen.findByRole('button', { name: 'Spoke review' })).not.toBeNull()
  })

  it('opens a run older than the loaded history by UID and names a missing run', async () => {
    const issueUID = '01J00000000000000000000001'
    const deletedJobUID = '01K0000000000000000000000D'
    let detailReads = 0
    const fetcher = serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'jobs')) {
        return Response.json({ jobs: [job(exampleProject, 'Daily review')] })
      }
      if (url.pathname === cronPath(exampleProject, `runs/${runA}`)) {
        return Response.json({ run: run(exampleProject, runA, 3, { job_uid: deletedJobUID }) })
      }
      if (url.pathname === cronPath(exampleProject, `runs/${runC}`)) {
        detailReads += 1
        if (detailReads === 1) {
          return Response.json({ error: { code: 'invalid' } }, { status: 400 })
        }
        return Response.json({
          run: run(exampleProject, runC, 1, { status: 'failed', issue_uid: issueUID }),
        })
      }
      return emptyCatalog(url) ?? notFound()
    })
    const selection = { kind: 'run' as const, projectUID: exampleProject.uid, uid: runC }
    const current = props({ tab: 'runs', selection }, [exampleProject])
    const view = render(CronPage, { props: current })

    // Refresh reads the open drawer again, so it also retries a failed detail.
    expect(await screen.findByText('The run could not be loaded.')).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    expect(await screen.findByText(`Run ${runC}`)).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Open issue' }))
    expect(current.onOpenIssue).toHaveBeenCalledWith(issueUID)
    // A job still in the catalog opens from the run.
    const refresh = screen.getByRole('button', { name: 'Refresh' })
    await waitFor(() => expect(refresh.getAttribute('aria-busy')).toBeNull())
    await fireEvent.click(screen.getByRole('button', { name: 'Daily review' }))
    const jobSelection = { kind: 'job', projectUID: exampleProject.uid, uid: jobUID }
    expect(current.onNavigate).toHaveBeenCalledWith(
      expect.objectContaining({ selection: jobSelection }),
    )

    // A deleted job keeps its UID as text, with nothing to open.
    await view.rerender({
      ...current,
      route: { ...current.route, selection: { ...selection, uid: runA } },
    })
    expect(await screen.findByText(`Run ${runA}`)).not.toBeNull()
    expect(screen.getByText(deletedJobUID)).not.toBeNull()
    expect(screen.queryByRole('button', { name: deletedJobUID })).toBeNull()

    await view.rerender({
      ...current,
      route: { ...current.route, selection: { ...selection, uid: runB } },
    })
    expect(await screen.findByText('Run not found.')).not.toBeNull()
    expect(requested(fetcher, cronPath(exampleProject, `runs/${runB}`))).toHaveLength(1)
  })

  it('shows a selected project the connection cannot read without reading it', async () => {
    const fetcher = serve((url) => emptyCatalog(url) ?? notFound())
    render(CronPage, {
      props: props(
        { tab: 'runs', selection: { kind: 'run', projectUID: spokeProject.uid, uid: runA } },
        [exampleProject],
      ),
    })

    expect(
      await screen.findByText('This project is not readable from the current connection.'),
    ).not.toBeNull()
    const paths = fetcher.mock.calls.map(([input]) => urlOf(input).pathname)
    expect(paths.filter((path) => path.includes('/projects/undefined/'))).toEqual([])
    expect(paths.filter((path) => path.endsWith(`/cron/runs/${runA}`))).toEqual([])
  })

  it('ignores a late catalog response after the projects change and change back', async () => {
    const pending: Array<(response: Response) => void> = []
    serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'jobs')) {
        return new Promise<Response>((resolve) => pending.push(resolve))
      }
      if (url.pathname === cronPath(spokeProject, 'jobs')) {
        return Response.json({ jobs: [job(spokeProject, 'Spoke review')] })
      }
      return emptyCatalog(url) ?? notFound()
    })
    const current = props({}, [exampleProject])
    const view = render(CronPage, { props: current })
    await waitFor(() => expect(pending).toHaveLength(1))

    await view.rerender({ ...current, projects: [spokeProject] })
    expect(await screen.findByRole('button', { name: 'Spoke review' })).not.toBeNull()
    await view.rerender({ ...current, projects: [exampleProject] })
    await waitFor(() => expect(pending).toHaveLength(2))
    expect(screen.queryByRole('button', { name: 'Spoke review' })).toBeNull()

    pending[1]!(Response.json({ jobs: [job(exampleProject, 'Current review')] }))
    expect(await screen.findByRole('button', { name: 'Current review' })).not.toBeNull()

    // The stale read finishes last. The mock ignores the abort signal, so its
    // body arrives. One zero-delay timer turn flushes the queued promise and
    // render continuations of that chain before checking nothing changed.
    pending[0]!(Response.json({ jobs: [job(exampleProject, 'Stale review')] }))
    await new Promise((resolve) => setTimeout(resolve, 0))

    expect(screen.getByRole('button', { name: 'Current review' })).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Stale review' })).toBeNull()
    expect(screen.queryByText('example-project: cron records could not be loaded.')).toBeNull()
  })

  it('reads the catalog and the open job again on Refresh', async () => {
    let jobsReads = 0
    const fetcher = serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'jobs')) {
        jobsReads += 1
        return Response.json({ jobs: [job(exampleProject, `Daily review ${jobsReads}`)] })
      }
      if (url.pathname === cronPath(exampleProject, 'runs') && url.searchParams.has('job_uid')) {
        return Response.json({ runs: [run(exampleProject, runA, 1, { status: 'failed' })] })
      }
      return emptyCatalog(url) ?? notFound()
    })
    const selection = { kind: 'job' as const, projectUID: exampleProject.uid, uid: jobUID }
    render(CronPage, { props: props({ selection }, [exampleProject]) })

    expect(await screen.findByRole('button', { name: 'Daily review 1' })).not.toBeNull()
    await waitFor(() =>
      expect(screen.getAllByRole('button', { name: /^failed · / })).toHaveLength(1),
    )

    const refresh = screen.getByRole('button', { name: 'Refresh' })
    await waitFor(() => expect(refresh.getAttribute('aria-busy')).toBeNull())
    await fireEvent.click(refresh)

    expect(await screen.findByRole('button', { name: 'Daily review 2' })).not.toBeNull()
    const jobRuns = () => requested(fetcher, cronPath(exampleProject, 'runs')).filter(jobRead)
    await waitFor(() => expect(jobRuns()).toHaveLength(2))
    expect(jobRuns().map((url) => url.searchParams.get('limit'))).toEqual(['5', '5'])
  })
})
