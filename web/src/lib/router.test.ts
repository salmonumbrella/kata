import { describe, expect, it } from 'vitest'

import { parseRoute, serializeRoute, screenNameForRoute } from './router'

const issueUID = '01HZNQ7VFPK1XGD8R5MABCD4EX'

describe('canonical Kata routes', () => {
  it('keeps Forge view, scope, and issue state independent', () => {
    const projectUID = '01HZNQ7VFPK1XGD8R5MABCD4EW'
    const route = parseRoute(
      new URL(
        `https://daemon.example/kata?view=all-open&scope=${projectUID}&issue=${issueUID}&label=ready`,
      ),
    )

    expect(route).toMatchObject({
      kind: 'kata',
      view: 'all-open',
      projectUID,
      issueUID,
      graph: false,
    })
    expect(serializeRoute(route)).toBe(
      `/kata?view=all-open&scope=${projectUID}&issue=${issueUID}&label=ready`,
    )
  })

  it('parses only the canonical route families', () => {
    expect(parseRoute(new URL(`https://daemon.example/kata?view=today&label=ready`))).toMatchObject(
      {
        kind: 'kata',
        view: 'today',
      },
    )
    expect(parseRoute(new URL(`https://daemon.example/kata?scope=${issueUID}`))).toMatchObject({
      kind: 'kata',
      projectUID: issueUID,
    })
    expect(
      parseRoute(new URL(`https://daemon.example/kata?issue=${issueUID}&graph=1`)),
    ).toMatchObject({
      kind: 'kata',
      issueUID,
      graph: true,
    })
  })

  it('parses and serializes the Delegated view', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=delegated'))

    expect(route).toMatchObject({ kind: 'kata', view: 'delegated' })
    expect(serializeRoute(route)).toBe('/kata?view=delegated')
  })

  it('parses and serializes the Scheduled view', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=scheduled'))

    expect(route).toMatchObject({ kind: 'kata', view: 'scheduled' })
    expect(serializeRoute(route)).toBe('/kata?view=scheduled')
  })

  it.each(['credentials', 'settings'])(
    'parses %s as a daemon route without carrying issue workspace state',
    (view) => {
      const route = parseRoute(
        new URL(`https://daemon.example/kata?view=${view}&issue=${issueUID}&label=ready`),
      )
      expect(route).toEqual({
        kind: 'kata',
        view,
        graph: false,
        filters: { status: [], owner: [], label: [], relationship: [] },
      })
      expect(serializeRoute(route)).toBe(`/kata?view=${view}`)
    },
  )

  it('admits the Cron view without carrying issue workspace state', () => {
    const route = parseRoute(
      new URL(`https://daemon.example/kata?view=cron&issue=${issueUID}&label=ready`),
    )
    expect(route.kind).toBe('kata')
    const shape = route as { view?: unknown; issueUID?: unknown }
    expect(shape.view).toBe('cron')
    expect(shape.issueUID).toBeUndefined()
    const serialized = new URL(serializeRoute(route), 'https://daemon.example')
    expect(serialized.searchParams.get('view')).toBe('cron')
    expect(serialized.searchParams.has('issue')).toBe(false)
    expect(serialized.searchParams.has('label')).toBe(false)
  })

  it('round-trips Cron tabs, filters and selection in a stable order', () => {
    const projectA = '01J00000000000000000000002'
    const projectB = '01J00000000000000000000003'
    const runUID = '01K00000000000000000000001'
    const route = parseRoute(
      new URL(
        `https://daemon.example/kata?view=cron&cron_tab=runs&cron_project=${projectB}` +
          `&cron_project=${projectA}&cron_project=not-a-uid&cron_status=failed` +
          `&cron_status=bogus&cron_status=cancelled&cron_kind=run&cron_scope=${projectA}` +
          `&cron_uid=${runUID}`,
      ),
    )

    expect(route).toEqual({
      kind: 'kata',
      view: 'cron',
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
      cron: {
        tab: 'runs',
        projectUIDs: [projectA, projectB],
        statuses: ['cancelled', 'failed'],
        selection: { kind: 'run', projectUID: projectA, uid: runUID },
      },
    })
    expect(serializeRoute(route)).toBe(
      `/kata?view=cron&cron_tab=runs&cron_project=${projectA}&cron_project=${projectB}` +
        `&cron_status=cancelled&cron_status=failed&cron_kind=run&cron_scope=${projectA}` +
        `&cron_uid=${runUID}`,
    )
    expect(parseRoute(new URL(serializeRoute(route), 'https://daemon.example'))).toEqual(route)
  })

  it.each([
    ['an unknown tab', 'cron_tab=history'],
    ['statuses outside the Runs tab', 'cron_status=failed'],
    ['an unknown selection kind', `cron_kind=step&cron_scope=${issueUID}&cron_uid=${issueUID}`],
    ['a selection without its project', `cron_kind=run&cron_uid=${issueUID}`],
    ['a selection with an invalid UID', `cron_kind=job&cron_scope=${issueUID}&cron_uid=abc4`],
  ])('drops %s from a Cron route', (_name, query) => {
    const route = parseRoute(new URL(`https://daemon.example/kata?view=cron&${query}`))

    expect(route).toMatchObject({
      kind: 'kata',
      view: 'cron',
      cron: { tab: 'jobs', projectUIDs: [], statuses: [] },
    })
    expect(serializeRoute(route)).toBe('/kata?view=cron')
  })

  it('keeps invalid issue UIDs routed and gives short refs one search action', () => {
    expect(parseRoute(new URL('https://daemon.example/kata?issue=abc4'))).toEqual({
      kind: 'route-error',
      path: '/kata?issue=abc4',
      reason: 'issue_uid',
      searchRef: 'abc4',
    })
    expect(
      parseRoute(new URL('https://daemon.example/kata?issue=not-a-valid-full-uid-value')),
    ).toEqual({
      kind: 'route-error',
      path: '/kata?issue=not-a-valid-full-uid-value',
      reason: 'issue_uid',
    })
  })

  it('serializes shareable filters deterministically', () => {
    const route = parseRoute(
      new URL(
        `https://daemon.example/kata?scope=${issueUID}&label=urgent&status=open&label=backend&owner=user-a`,
      ),
    )
    expect(serializeRoute(route)).toBe(
      `/kata?scope=${issueUID}&label=backend&label=urgent&owner=user-a&status=open`,
    )
  })

  it('parses and serializes a browser application mounted below a path', () => {
    const routePath = '/tools/tasks/'
    const route = parseRoute(
      new URL(`https://daemon.example${routePath}?view=today&label=ready`),
      routePath,
    )

    expect(route).toMatchObject({ kind: 'kata', view: 'today' })
    expect(serializeRoute(route, routePath)).toBe('/tools/tasks/?view=today&label=ready')
  })
})

describe('screen names', () => {
  it.each([
    ['', 'inbox'],
    ['?view=all-open', 'issues'],
    ['?view=today', 'today'],
    ['?view=delegated', 'delegated'],
    ['?view=scheduled', 'scheduled'],
    ['?view=logbook', 'logbook'],
    [`?scope=${issueUID}`, 'issues'],
    [`?issue=${issueUID}`, 'issue'],
    [`?issue=${issueUID}&graph=1`, 'graph'],
    [`?view=credentials&issue=${issueUID}`, 'credentials'],
    [`?view=settings&issue=${issueUID}`, 'settings'],
    [`?view=cron&issue=${issueUID}`, 'cron'],
    ['?view=unknown', undefined],
  ])('maps %s to %s', (query, screen) => {
    expect(screenNameForRoute(parseRoute(new URL(`https://daemon.example/kata${query}`)))).toBe(
      screen,
    )
  })
})
