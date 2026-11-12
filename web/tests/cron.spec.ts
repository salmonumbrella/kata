import { createServer } from 'node:http'
import type { AddressInfo } from 'node:net'

import type { BrowserContext, Page, TestInfo } from '@playwright/test'

import { expect, test, type KataFixture } from './fixtures'

// Every record below is written through the kata CLI to the fixture's real
// daemon; the browser reads it back through its ordinary local-web session.
const workflowUID = '01KA0000000000000000000W01'
const dailyJobUID = '01KA0000000000000000000J01'
const inboxJobUID = '01KA0000000000000000000J02'
const failedRunUID = '01KA0000000000000000000R98'
const runningRunUID = '01KA0000000000000000000R99'
const missingRunUID = '01KA0000000000000000000R00'

function fillerRunUID(index: number): string {
  return `01KA0000000000000000000${String(index).padStart(3, '0')}`
}

function cron(
  kata: KataFixture,
  args: string[],
  body: unknown,
  target: 'local' | 'remote' = 'local',
): Record<string, unknown> {
  const output = kata.cli(['cron', ...args, '--json'], JSON.stringify(body), target)
  return JSON.parse(output) as Record<string, unknown>
}

function seedCron(kata: KataFixture, issueUID: string): void {
  cron(kata, ['workflow', 'create', '--uid', workflowUID, '--file', '-'], {
    name: 'nightly-checks',
    definition: {
      version: 1,
      about: 'Check the workspace before the nightly summary.',
      steps: [
        { key: 'check', kind: 'command', command: 'git diff --check' },
        { key: 'test', kind: 'command', command: 'go test ./...', after: ['check'] },
      ],
    },
  })
  const daily = cron(kata, ['job', 'create', '--uid', dailyJobUID, '--file', '-'], {
    name: 'Daily summary',
    definition: {
      version: 1,
      kind: 'job',
      enabled: true,
      trigger: { kind: 'cron', cron: '0 9 * * *', timezone: 'UTC' },
      action: { kind: 'execute', prompt: 'Summarize open work and next actions.' },
      issue: { kind: 'per-run', title: 'Daily summary' },
      overlap: 'forbid',
      catchup: 'latest',
    },
  })
  const inbox = ['job', 'create', '--project', 'example-inbox', '--uid', inboxJobUID, '--file', '-']
  cron(kata, inbox, {
    name: 'Inbox sweep',
    definition: {
      version: 1,
      kind: 'job',
      enabled: false,
      trigger: { kind: 'interval', interval_seconds: 3600 },
      action: { kind: 'execute', prompt: 'Sort new inbox requests.' },
      issue: { kind: 'per-run', title: 'Inbox sweep' },
      overlap: 'forbid',
      catchup: 'latest',
    },
  })
  const definition = (daily.job as { definition_event_uid: string }).definition_event_uid
  const observe = (uid: string, observation: Record<string, unknown>) =>
    cron(kata, ['run', 'observe', uid, '--json-input', '-'], {
      job_uid: dailyJobUID,
      definition_event_uid: definition,
      expected_revision: 0,
      ...observation,
    })
  // Fifty older runs fill the newest page with these two, so two stay unloaded.
  for (let index = 1; index <= 50; index += 1) {
    observe(fillerRunUID(index), {
      status: 'succeeded',
      summary: { version: 1, message: `Summary ${index} recorded.` },
    })
  }
  observe(failedRunUID, {
    status: 'failed',
    issue_uid: issueUID,
    teammate: 'example-worker',
    executor_label: 'example-runner',
    started_at: '2026-11-12T09:00:00Z',
    ended_at: '2026-11-12T09:04:00Z',
    summary: {
      version: 1,
      message: 'Stopped after the nightly check reported whitespace errors.',
      input_tokens: 1200,
      output_tokens: 340,
    },
  })
  observe(runningRunUID, {
    status: 'running',
    started_at: '2026-11-12T09:30:00Z',
    summary: { version: 1, message: 'Summarizing open work.' },
  })
}

async function capture(page: Page, testInfo: TestInfo, name: string): Promise<void> {
  const path = testInfo.outputPath(`${name}.png`)
  await page.screenshot({ path, fullPage: true, animations: 'disabled', caret: 'hide' })
  await testInfo.attach(name, { path, contentType: 'image/png' })
}

async function noHorizontalOverflow(page: Page, width: number): Promise<void> {
  const scrollWidth = await page.evaluate(() => document.documentElement.scrollWidth)
  expect(scrollWidth).toBeLessThanOrEqual(width)
}

test('browses real cron jobs, runs and workflows across projects and daemons', async ({
  page,
  kata,
}, testInfo) => {
  test.setTimeout(120_000)
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Fix nightly whitespace errors' })
  seedCron(kata, issue.uid)
  // Reload so the accepted snapshot already holds every seeded change.
  await page.goto(`${kata.origin}/kata?view=all-open`)
  const cronRoute = `${kata.origin}/kata?view=cron`
  const runRoute = (uid: string) =>
    `${cronRoute}&cron_tab=runs&cron_kind=run&cron_scope=${kata.projectUID}&cron_uid=${uid}`

  await page.emulateMedia({ colorScheme: 'dark' })
  await page.setViewportSize({ width: 1440, height: 960 })
  await page.getByRole('button', { name: 'Cron', exact: true }).click()
  await expect(page).toHaveURL(/\?view=cron$/)
  await expect(page.getByRole('heading', { name: 'Cron', exact: true })).toBeVisible()
  const jobs = page.getByRole('table', { name: 'Cron jobs' })
  await expect(jobs.getByRole('button', { name: 'Daily summary', exact: true })).toBeVisible()
  await expect(
    jobs.getByRole('row', { name: /Inbox sweep\s+example-inbox\s+Every 1h\s+No/ }),
  ).toBeVisible()
  await capture(page, testInfo, 'cron-desktop-jobs')

  await page.getByRole('radio', { name: 'Workflows' }).click()
  await expect(page).toHaveURL(/cron_tab=workflows/)
  await page.getByRole('button', { name: 'nightly-checks', exact: true }).click()
  const drawer = page.getByRole('dialog', { name: 'nightly-checks' })
  await expect(drawer.getByText('go test ./...')).toBeVisible()
  await drawer.getByRole('button', { name: 'Close' }).click()

  await page.getByRole('radio', { name: 'Runs' }).click()
  await expect(page).toHaveURL(/cron_tab=runs/)
  const runs = page.getByRole('table', { name: 'Cron runs' }).locator('tbody tr')
  // The daemon holds 52 runs; the page loads only the newest 50.
  await expect(runs).toHaveCount(50)
  await capture(page, testInfo, 'cron-desktop-runs')

  await page.getByRole('button', { name: 'Status', exact: true }).click()
  await page.getByRole('button', { name: 'failed', exact: true }).click()
  await expect(page).toHaveURL(/cron_status=failed/)
  await page.keyboard.press('Escape')
  await expect(runs).toHaveCount(1)
  await expect(page.getByRole('button', { name: 'Remove status filter failed' })).toBeVisible()

  await runs.getByRole('button', { name: 'Daily summary', exact: true }).click()
  await expect(page).toHaveURL(new RegExp(`cron_uid=${failedRunUID}`))
  const run = page.getByRole('dialog', { name: 'Run' })
  await expect(
    run.getByText('Stopped after the nightly check reported whitespace errors.'),
  ).toBeVisible()
  await expect(run.getByText('example-runner')).toBeVisible()
  await expect(run.getByText('1200 in · 340 out')).toBeVisible()
  await capture(page, testInfo, 'cron-desktop-run-detail')

  await page.reload()
  await run.getByRole('button', { name: 'Open issue' }).click()
  await expect(page).toHaveURL(new RegExp(`\\?issue=${issue.uid}$`))
  await page.goBack()
  await expect(page).toHaveURL(new RegExp(`cron_uid=${failedRunUID}`))
  await expect(page.getByRole('dialog', { name: 'Run' })).toBeVisible()

  await page.goto(runRoute(missingRunUID))
  await expect(page.getByRole('dialog', { name: 'Run' }).getByText('Run not found.')).toBeVisible()
  await capture(page, testInfo, 'cron-desktop-run-not-found')

  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto(`${cronRoute}&cron_tab=runs`)
  await expect(runs).toHaveCount(50)
  await noHorizontalOverflow(page, 390)
  await capture(page, testInfo, 'cron-mobile-runs')
  await page.goto(runRoute(failedRunUID))
  await expect(
    page
      .getByRole('dialog', { name: 'Run' })
      .getByText('Stopped after the nightly check reported whitespace errors.'),
  ).toBeVisible()
  await noHorizontalOverflow(page, 390)
  await capture(page, testInfo, 'cron-mobile-run-detail')

  await page.setViewportSize({ width: 1440, height: 960 })
  await page.goto(cronRoute)
  await expect(jobs.getByRole('button', { name: 'Daily summary', exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
  await page.getByRole('menuitemradio', { name: /example-remote/ }).click()
  await expect(page.getByRole('button', { name: /Remote daemon task/ })).toBeVisible()
  await page.getByRole('button', { name: 'Cron', exact: true }).click()
  await expect(page.getByText('No cron jobs yet')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Daily summary', exact: true })).toHaveCount(0)
  await capture(page, testInfo, 'cron-desktop-remote-empty')
  await page.getByRole('button', { name: 'Switch Kata daemon: example-remote' }).click()
  await page.getByRole('menuitemradio', { name: /example-local/ }).click()
  await expect(jobs.getByRole('button', { name: 'Daily summary', exact: true })).toBeVisible()
})

// Runs after the test above in the same worker, so its records never reach that
// test, and it restores both daemons to no usage source when it ends.
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
const estimatedRunUID = '01KC0000000000000000000R06'
const noSessionsRunUID = '01KC0000000000000000000R07'

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

function priced(id: string, microdollars: number, costSource = 'reported') {
  return {
    status: 200,
    body: {
      agent: 'codex',
      session_id: id,
      has_cost: true,
      has_token_data: true,
      cost: { microdollars },
      cost_source: costSource,
      models: ['example-model'],
      total_output_tokens: 1200,
      subagent_count: 0,
    },
  }
}

function seedRuns(
  kata: KataFixture,
  target: 'local' | 'remote',
  jobUID: string,
  runs: Array<{ uid: string; sessions: string[] }>,
): void {
  const jobBody = {
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
  }
  const job = cron(kata, ['job', 'create', '--uid', jobUID, '--file', '-'], jobBody, target)
  const definition = (job.job as { definition_event_uid: string }).definition_event_uid
  for (const run of runs) {
    const sessions = run.sessions.map((session_id) => ({ agent: 'codex', session_id }))
    const observation = {
      job_uid: jobUID,
      definition_event_uid: definition,
      expected_revision: 0,
      status: 'succeeded',
      summary: {
        version: 1,
        message: `Reviewed open work in ${run.uid}.`,
        ...(sessions.length > 0 ? { sessions } : {}),
      },
    }
    cron(kata, ['run', 'observe', run.uid, '--json-input', '-'], observation, target)
  }
}

test('shows live run cost from each daemon without exposing its AgentsView token', async ({
  browser,
  page,
  kata,
}, testInfo) => {
  test.setTimeout(180_000)
  const local = await fakeAgentsView()
  const remote = await fakeAgentsView()
  let anonymousContext: BrowserContext | undefined
  try {
    local.answers.set(sessionUID('a1'), priced(sessionUID('a1'), 2_410_000))
    local.answers.set(sessionUID('a2'), priced(sessionUID('a2'), 1_205_000, 'computed'))
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
      { uid: estimatedRunUID, sessions: [sessionUID('a2')] },
      { uid: noSessionsRunUID, sessions: [] },
    ])
    seedRuns(kata, 'remote', remoteJobUID, [{ uid: remoteRunUID, sessions: [sessionUID('e1')] }])

    const browserTraffic: Promise<string>[] = []
    const usageRequests: string[] = []
    const watch = (target: Page) => {
      target.on('request', (request) => {
        browserTraffic.push(Promise.resolve(JSON.stringify(request.headers())))
        if (request.url().includes('/usage')) usageRequests.push(request.url())
      })
      // Only finished requests have a body to read; reading the body of one a
      // navigation or abort cancelled would wait forever.
      target.on('requestfinished', (request) => {
        if (!request.url().includes('/cron/')) return
        const body = request.response().then((response) => response?.text() ?? '')
        browserTraffic.push(body.catch(() => ''))
      })
    }
    watch(page)
    const runRoute = (uid: string) =>
      `${kata.origin}/kata?view=cron&cron_tab=runs&cron_kind=run&cron_scope=${kata.projectUID}&cron_uid=${uid}`
    const drawer = page.getByRole('dialog', { name: 'Run' })
    const cost = drawer.getByRole('region', { name: 'Cost' })

    await page.emulateMedia({ colorScheme: 'dark' })
    await page.setViewportSize({ width: 1440, height: 960 })
    await page.goto(runRoute(completeRunUID))
    await expect(cost.getByText('$2.41', { exact: true })).toBeVisible()
    // The daemon built a session address the drawer accepts as safe.
    await expect(cost.getByRole('link', { name: 'Open in AgentsView' })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-complete')

    const refreshed = page.waitForRequest((request) =>
      request.url().includes(`/runs/${completeRunUID}/usage?refresh=true`),
    )
    await cost.getByRole('button', { name: 'Refresh cost' }).click()
    await refreshed
    await capture(page, testInfo, 'cost-desktop-refreshed')

    await page.goto(runRoute(estimatedRunUID))
    await expect(cost.getByText('~$1.21', { exact: true })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-estimated')

    await page.goto(runRoute(noSessionsRunUID))
    await expect(cost.getByText('no sessions', { exact: true })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-no-sessions')

    await page.goto(runRoute(partialRunUID))
    await expect(cost.getByText(/\$1\.20\+ \(1 of 2 sessions not priced\)$/)).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-partial')

    await page.goto(runRoute(unavailableRunUID))
    await expect(cost.getByText('unknown', { exact: true })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-unavailable')

    await page.goto(runRoute(overflowRunUID))
    await expect(cost.getByText('The total cost cannot be represented.')).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-error')

    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto(runRoute(completeRunUID))
    await expect(cost.getByText('$2.41', { exact: true })).toBeVisible()
    await noHorizontalOverflow(page, 390)
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

    // A read-only daemon still lets this tab mint its local session, so it is a
    // signed-in read-only reader and still sees the cost.
    await kata.restartReadonly()
    await page.goto(runRoute(completeRunUID))
    await expect(page.getByRole('status', { name: 'Active connection' })).toContainText('Read-only')
    await expect(cost.getByText('$2.41', { exact: true })).toBeVisible()
    await capture(page, testInfo, 'cost-desktop-read-only')

    // Without a static token, a session-less request is the anonymous reader.
    // The forwarding header makes the daemon refuse the loopback session mint
    // (directLoopbackSessionAllowed), so this fresh tab stays anonymous.
    await kata.restartReadonly({ tokenless: true })
    anonymousContext = await browser.newContext({
      colorScheme: 'dark',
      viewport: { width: 1440, height: 960 },
      extraHTTPHeaders: { 'X-Forwarded-For': '127.0.0.1' },
    })
    const anonymous = await anonymousContext.newPage()
    watch(anonymous)
    usageRequests.length = 0
    await anonymous.goto(runRoute(completeRunUID))
    await expect(anonymous.getByRole('status', { name: 'Active connection' })).toContainText(
      'Anonymous (read-only)',
    )
    const anonymousCost = anonymous
      .getByRole('dialog', { name: 'Run' })
      .getByRole('region', { name: 'Cost' })
    await expect(anonymousCost.getByText('Cost needs a signed-in session')).toBeVisible()
    expect(usageRequests).toEqual([])
    await capture(anonymous, testInfo, 'cost-desktop-anonymous')

    const traffic =
      (await Promise.all(browserTraffic)).join('\n') +
      (await page.content()) +
      (await anonymous.content())
    expect(traffic).not.toContain(kata.agentsViewTokens.local)
    expect(traffic).not.toContain(kata.agentsViewTokens.remote)
    await anonymousContext.close()
    anonymousContext = undefined

    await kata.agentsView({})
    await kata.launch(page)
    await page.goto(runRoute(completeRunUID))
    await expect(drawer.getByText('Sessions')).toBeVisible()
    await expect(drawer.getByRole('heading', { name: 'Cost' })).toHaveCount(0)
    expect(usageRequests).toEqual([])
    await capture(page, testInfo, 'cost-desktop-off')
  } finally {
    await anonymousContext?.close()
    await kata.agentsView({})
    await Promise.all([local.close(), remote.close()])
  }
})
