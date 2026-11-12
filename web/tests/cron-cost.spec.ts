import { createServer } from 'node:http'
import type { AddressInfo } from 'node:net'

import type { Page, TestInfo } from '@playwright/test'

import { expect, test, type KataFixture } from './fixtures'

// Runs and sessions are recorded through the kata CLI on the fixture's real
// local and remote daemons. Each daemon reads usage from its own loopback fake
// AgentsView with its own token; the browser only reads the daemon's answer.
const localJobUID = '01KC0000000000000000000J01'
const remoteJobUID = '01KC0000000000000000000J02'
const completeRunUID = '01KC0000000000000000000R01'
const partialRunUID = '01KC0000000000000000000R02'
const unavailableRunUID = '01KC0000000000000000000R03'
const overflowRunUID = '01KC0000000000000000000R04'
const remoteRunUID = '01KC0000000000000000000R05'

function sessionUID(suffix: string): string {
  return `00000000-0000-4000-8000-0000000000${suffix}`
}

interface FakeAgentsView {
  url: string
  // The Authorization header of every request it received.
  bearers: string[]
  answers: Map<string, { status: number; body: unknown }>
  close(): Promise<void>
}

async function fakeAgentsView(): Promise<FakeAgentsView> {
  const answers = new Map<string, { status: number; body: unknown }>()
  const bearers: string[] = []
  const server = createServer((request, response) => {
    bearers.push(request.headers.authorization ?? '')
    const path = new URL(request.url ?? '/', 'http://127.0.0.1').pathname
    const id = /^\/api\/v1\/sessions\/([^/]+)\/usage$/.exec(path)?.[1]
    const answer = (id && answers.get(decodeURIComponent(id))) || {
      status: 404,
      body: { error: { code: 'session_not_found', message: 'session not found' } },
    }
    response.writeHead(answer.status, { 'Content-Type': 'application/json' })
    response.end(JSON.stringify(answer.body))
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const { port } = server.address() as AddressInfo
  return {
    url: `http://127.0.0.1:${port}`,
    bearers,
    answers,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  }
}

function priced(id: string, microdollars: number) {
  return {
    status: 200,
    body: {
      agent: 'codex',
      session_id: id,
      has_cost: true,
      has_token_data: true,
      cost: { microdollars },
      cost_source: 'reported',
      models: ['example-model'],
      total_output_tokens: 1200,
      subagent_count: 0,
    },
  }
}

function cron(kata: KataFixture, target: 'local' | 'remote', args: string[], body: unknown) {
  const output = kata.cli(['cron', ...args, '--json'], JSON.stringify(body), target)
  return JSON.parse(output) as Record<string, unknown>
}

function seedRuns(
  kata: KataFixture,
  target: 'local' | 'remote',
  jobUID: string,
  runs: Array<{ uid: string; sessions: string[] }>,
): void {
  const job = cron(kata, target, ['job', 'create', '--uid', jobUID, '--file', '-'], {
    name: target === 'local' ? 'Cost review' : 'Remote cost review',
    definition: {
      version: 1,
      kind: 'job',
      enabled: true,
      trigger: { kind: 'cron', cron: '0 9 * * *', timezone: 'UTC' },
      action: { kind: 'execute', prompt: 'Review open work.' },
      issue: { kind: 'per-run', title: 'Cost review' },
      overlap: 'forbid',
      catchup: 'latest',
    },
  })
  const definition = (job.job as { definition_event_uid: string }).definition_event_uid
  for (const run of runs) {
    cron(kata, target, ['run', 'observe', run.uid, '--json-input', '-'], {
      job_uid: jobUID,
      definition_event_uid: definition,
      expected_revision: 0,
      status: 'succeeded',
      summary: {
        version: 1,
        message: `Reviewed open work in ${run.uid}.`,
        sessions: run.sessions.map((session_id) => ({ agent: 'codex', session_id })),
      },
    })
  }
}

async function capture(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  const path = testInfo.outputPath(`${name}.png`)
  await page.screenshot({ path, fullPage: true, animations: 'disabled', caret: 'hide' })
  await testInfo.attach(name, { path, contentType: 'image/png' })
}

test('shows live run cost from each daemon without exposing its AgentsView token', async ({
  page,
  kata,
}, testInfo) => {
  test.setTimeout(180_000)
  const local = await fakeAgentsView()
  const remote = await fakeAgentsView()
  try {
    local.answers.set(sessionUID('a1'), priced(sessionUID('a1'), 2_410_000))
    local.answers.set(sessionUID('b1'), priced(sessionUID('b1'), 1_200_000))
    local.answers.set(sessionUID('c1'), { status: 500, body: {} })
    local.answers.set(sessionUID('d1'), priced(sessionUID('d1'), 9e18))
    local.answers.set(sessionUID('d2'), priced(sessionUID('d2'), 9e18))
    remote.answers.set(sessionUID('e1'), priced(sessionUID('e1'), 7_770_000))
    await kata.agentsView({ local: local.url, remote: remote.url })
    await kata.launch(page)
    seedRuns(kata, 'local', localJobUID, [
      { uid: completeRunUID, sessions: [sessionUID('a1')] },
      { uid: partialRunUID, sessions: [sessionUID('b1'), sessionUID('b2')] },
      { uid: unavailableRunUID, sessions: [sessionUID('c1')] },
      { uid: overflowRunUID, sessions: [sessionUID('d1'), sessionUID('d2')] },
    ])
    seedRuns(kata, 'remote', remoteJobUID, [{ uid: remoteRunUID, sessions: [sessionUID('e1')] }])

    const browserTraffic: Promise<string>[] = []
    page.on('request', (request) => {
      browserTraffic.push(Promise.resolve(JSON.stringify(request.headers())))
    })
    // Only finished requests have a body to read; reading the body of one a
    // navigation or abort cancelled would wait forever.
    page.on('requestfinished', (request) => {
      if (!request.url().includes('/cron/')) return
      const body = request.response().then((response) => response?.text() ?? '')
      browserTraffic.push(body.catch(() => ''))
    })
    const usageRequests: string[] = []
    page.on('request', (request) => {
      if (request.url().includes('/usage')) usageRequests.push(request.url())
    })
    const runRoute = (uid: string) =>
      `${kata.origin}/kata?view=cron&cron_tab=runs&cron_kind=run&cron_scope=${kata.projectUID}&cron_uid=${uid}`
    const drawer = page.getByRole('dialog', { name: 'Run' })
    const cost = drawer.getByRole('region', { name: 'Cost' })

    await page.emulateMedia({ colorScheme: 'dark' })
    await page.setViewportSize({ width: 1440, height: 960 })
    await page.goto(runRoute(completeRunUID))
    await expect(cost.getByText('$2.41', { exact: true })).toBeVisible()
    const link = cost.getByRole('link', { name: 'Open in AgentsView' })
    await expect(link).toHaveAttribute('rel', 'noopener noreferrer')
    await expect(link).toHaveAttribute('target', '_blank')
    await capture(page, testInfo, 'cost-desktop-complete')

    const refreshed = page.waitForRequest((request) =>
      request.url().includes(`/runs/${completeRunUID}/usage?refresh=true`),
    )
    await cost.getByRole('button', { name: 'Refresh cost' }).click()
    await refreshed
    await expect(cost.getByText('$2.41', { exact: true })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-refreshed')

    await page.goto(runRoute(partialRunUID))
    await expect(cost.getByText(/\$1\.20\+ \(1 of 2 sessions not priced\)$/)).toBeVisible()
    await expect(cost.getByText(/not in AgentsView yet/)).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-partial')

    await page.goto(runRoute(unavailableRunUID))
    await expect(cost.getByText('unknown', { exact: true })).toBeVisible()
    await expect(cost.getByText(/AgentsView unavailable/)).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-unavailable')

    await page.goto(runRoute(overflowRunUID))
    await expect(cost.getByText('The total cost cannot be represented.')).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-error')

    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto(runRoute(completeRunUID))
    await expect(cost.getByText('$2.41', { exact: true })).toBeVisible()
    const scrollWidth = await page.evaluate(() => document.documentElement.scrollWidth)
    expect(scrollWidth).toBeLessThanOrEqual(390)
    await capture(page, testInfo, 'cost-mobile-complete')

    await page.setViewportSize({ width: 1440, height: 960 })
    await page.goto(`${kata.origin}/kata?view=all-open`)
    await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
    await page.getByRole('menuitemradio', { name: /example-remote/ }).click()
    await expect(page.getByRole('button', { name: /Remote daemon task/ })).toBeVisible()
    await page.getByRole('button', { name: 'Cron', exact: true }).click()
    await page.getByRole('radio', { name: 'Runs' }).click()
    await page.getByRole('button', { name: 'Remote cost review', exact: true }).click()
    await expect(cost.getByText('$7.77', { exact: true })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-remote')
    // The open drawer's overlay covers the page, so a reader closes it first.
    await drawer.getByRole('button', { name: 'Close' }).click()
    await expect(drawer).toBeHidden()
    await page.getByRole('button', { name: 'Switch Kata daemon: example-remote' }).click()
    await page.getByRole('menuitemradio', { name: /example-local/ }).click()

    expect(local.bearers.length).toBeGreaterThan(0)
    expect(remote.bearers.length).toBeGreaterThan(0)
    expect(new Set(local.bearers)).toEqual(new Set([`Bearer ${kata.agentsViewTokens.local}`]))
    expect(new Set(remote.bearers)).toEqual(new Set([`Bearer ${kata.agentsViewTokens.remote}`]))
    const traffic = (await Promise.all(browserTraffic)).join('\n') + (await page.content())
    expect(traffic).not.toContain(kata.agentsViewTokens.local)
    expect(traffic).not.toContain(kata.agentsViewTokens.remote)

    await kata.restartReadonly()
    usageRequests.length = 0
    await page.goto(runRoute(completeRunUID))
    await expect(cost.getByText('Cost needs a signed-in session')).toBeVisible()
    expect(usageRequests).toEqual([])
    await capture(page, testInfo, 'cost-desktop-anonymous')

    await kata.agentsView({})
    await kata.launch(page)
    await page.goto(runRoute(completeRunUID))
    await expect(drawer.getByText('Sessions')).toBeVisible()
    await expect(drawer.getByRole('heading', { name: 'Cost' })).toHaveCount(0)
    expect(usageRequests).toEqual([])
    await capture(page, testInfo, 'cost-desktop-off')
  } finally {
    await kata.agentsView({})
    await Promise.all([local.close(), remote.close()])
  }
})
