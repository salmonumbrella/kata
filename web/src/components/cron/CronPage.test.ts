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

function serve(handler: Handler) {
  const fetcher = vi.fn<typeof fetch>(async (input) =>
    handler(new URL(String(input), 'http://127.0.0.1')),
  )
  setGeneratedFetch(fetcher)
  return fetcher
}

function requested(fetcher: ReturnType<typeof serve>, path: string): URL[] {
  return fetcher.mock.calls
    .map(([input]) => new URL(String(input), 'http://127.0.0.1'))
    .filter((url) => url.pathname === path)
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

function props(route: Partial<CronRoute>, projects: Project[], cursor = 1) {
  return {
    route: { tab: 'jobs' as const, projectUIDs: [], statuses: [], ...route },
    projects,
    cursor,
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
    serve(async (url) => {
      inFlight += 1
      mostInFlight = Math.max(mostInFlight, inFlight)
      await new Promise((resolve) => setTimeout(resolve, 1))
      inFlight -= 1
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

    expect(await screen.findByRole('button', { name: 'Review example-project-1' })).not.toBeNull()
    expect(screen.getByRole('button', { name: 'Review example-project-6' })).not.toBeNull()
    expect(
      screen.getByText('example-project-3: this connection cannot read its cron records.'),
    ).not.toBeNull()
    expect(screen.getByText('example-project-4: cron records could not be loaded.')).not.toBeNull()
    expect(screen.getAllByRole('button', { name: 'Try again' })).toHaveLength(1)
    expect(mostInFlight).toBe(4)
  })

  it('loads older runs per project and keeps the cursor when an older page fails', async () => {
    let olderAttempts = 0
    const fetcher = serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'jobs')) {
        return Response.json({ jobs: [job(exampleProject, 'Daily review')] })
      }
      if (url.pathname === cronPath(exampleProject, 'runs')) {
        if (!url.searchParams.has('before_uid')) {
          return Response.json({
            runs: [run(exampleProject, runA, 3), run(exampleProject, runB, 2)],
            next_before_uid: runB,
          })
        }
        olderAttempts += 1
        if (olderAttempts === 1) {
          return Response.json({ error: { code: 'invalid' } }, { status: 400 })
        }
        return Response.json({ runs: [run(exampleProject, runC, 1, { status: 'failed' })] })
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
    await fireEvent.click(
      screen.getByRole('button', { name: 'Load older runs · example-project' }),
    )
    const retry = await screen.findByRole('button', {
      name: 'Try older runs again · example-project',
    })
    await fireEvent.click(retry)

    await waitFor(() =>
      expect(screen.getAllByRole('button', { name: 'Daily review' })).toHaveLength(3),
    )
    expect(screen.queryByRole('button', { name: /older runs/ })).toBeNull()
    expect(
      screen.getByText('Showing loaded runs, newest first. Filters apply to loaded runs.'),
    ).not.toBeNull()
    const older = requested(fetcher, cronPath(exampleProject, 'runs')).filter((url) =>
      url.searchParams.has('before_uid'),
    )
    expect(older.map((url) => url.searchParams.get('before_uid'))).toEqual([runB, runB])
    expect(older.every((url) => url.searchParams.get('limit') === '50')).toBe(true)
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
  })

  it('opens a run older than the loaded history by UID and names a missing run', async () => {
    const issueUID = '01J00000000000000000000001'
    const fetcher = serve((url) => {
      if (url.pathname === cronPath(exampleProject, `runs/${runC}`)) {
        return Response.json({
          run: run(exampleProject, runC, 1, { status: 'failed', issue_uid: issueUID }),
        })
      }
      return emptyCatalog(url) ?? notFound()
    })
    const selection = { kind: 'run' as const, projectUID: exampleProject.uid, uid: runC }
    const current = props({ tab: 'runs', selection }, [exampleProject])
    const view = render(CronPage, { props: current })

    expect(await screen.findByText(`Run ${runC}`)).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Open issue' }))
    expect(current.onOpenIssue).toHaveBeenCalledWith(issueUID)

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
    expect(
      fetcher.mock.calls.some(([input]) => String(input).includes(`/projects/${spokeProject.id}/`)),
    ).toBe(false)
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

    pending[0]!(Response.json({ jobs: [job(exampleProject, 'Stale review')] }))
    pending[1]!(Response.json({ jobs: [job(exampleProject, 'Current review')] }))

    expect(await screen.findByRole('button', { name: 'Current review' })).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Stale review' })).toBeNull()
  })

  it('reads the page and the open job again when the accepted cursor advances', async () => {
    let cursorReads = 0
    const fetcher = serve((url) => {
      if (url.pathname === cronPath(exampleProject, 'jobs')) {
        cursorReads += 1
        return Response.json({ jobs: [job(exampleProject, `Daily review ${cursorReads}`)] })
      }
      if (url.pathname === cronPath(exampleProject, 'runs') && url.searchParams.has('job_uid')) {
        const uid = cursorReads > 1 ? runA : runB
        return Response.json({
          runs: [run(exampleProject, uid, cursorReads, { status: 'failed' })],
        })
      }
      return emptyCatalog(url) ?? notFound()
    })
    const selection = { kind: 'job' as const, projectUID: exampleProject.uid, uid: jobUID }
    const current = props({ selection }, [exampleProject])
    const view = render(CronPage, { props: current })

    expect(await screen.findByRole('button', { name: 'Daily review 1' })).not.toBeNull()
    await waitFor(() =>
      expect(screen.getAllByRole('button', { name: /^failed · / })).toHaveLength(1),
    )

    await view.rerender({ ...current, cursor: 2 })

    expect(await screen.findByRole('button', { name: 'Daily review 2' })).not.toBeNull()
    const jobRuns = () =>
      requested(fetcher, cronPath(exampleProject, 'runs')).filter(
        (url) => url.searchParams.get('job_uid') === jobUID,
      )
    await waitFor(() => expect(jobRuns()).toHaveLength(2))
    expect(jobRuns().map((url) => url.searchParams.get('limit'))).toEqual(['5', '5'])
  })
})
