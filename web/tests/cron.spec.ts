import type { Page, TestInfo } from '@playwright/test'

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

function cron(kata: KataFixture, args: string[], body: unknown): Record<string, unknown> {
  const output = kata.cli(['cron', ...args, '--json'], JSON.stringify(body))
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
  // Fifty older runs fill the first page, so two remain behind Load older.
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
  const loadOlder = page.getByRole('button', { name: 'Load older runs · example-project' })
  await expect(loadOlder).toBeVisible()
  await capture(page, testInfo, 'cron-desktop-runs')
  // The daemon's real next_before_uid cursor reaches the two oldest runs.
  await loadOlder.click()
  await expect(runs).toHaveCount(52)

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
  await expect(loadOlder).toBeVisible()
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
