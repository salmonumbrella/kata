import { applicationRoutePath } from './applicationBase'

export const systemViews = [
  'inbox',
  'today',
  'delegated',
  'scheduled',
  'all-open',
  'logbook',
] as const

export type SystemView = (typeof systemViews)[number]
export type DaemonView = 'credentials' | 'settings'

export const cronTabs = ['jobs', 'runs', 'workflows'] as const
export type CronTab = (typeof cronTabs)[number]
export const cronRunStatuses = ['running', 'succeeded', 'failed', 'unknown', 'cancelled'] as const
export type CronRunStatus = (typeof cronRunStatuses)[number]

export interface CronSelection {
  kind: 'job' | 'run' | 'workflow'
  projectUID: string
  uid: string
}

// The Cron page's shareable state. Statuses filter loaded runs, so they only
// exist on the Runs tab.
export interface CronRoute {
  tab: CronTab
  projectUIDs: string[]
  statuses: CronRunStatus[]
  selection?: CronSelection | undefined
}

export interface ShareableFilters {
  status: string[]
  owner: string[]
  label: string[]
  relationship: string[]
  text?: string
}

interface RouteBase {
  filters: ShareableFilters
}

export type KataRoute =
  | (RouteBase & {
      kind: 'kata'
      view?: SystemView | DaemonView | 'cron'
      projectUID?: string
      issueUID?: string
      graph: boolean
      cron?: CronRoute
    })
  | {
      kind: 'route-error'
      path: string
      reason: 'path' | 'view' | 'project_uid' | 'issue_uid'
      searchRef?: string
    }

export function parseRoute(url: URL, routePath = applicationRoutePath()): KataRoute {
  const filters = parseFilters(url.searchParams)
  if (normalizeRoutePath(url.pathname) !== normalizeRoutePath(routePath)) {
    return { kind: 'route-error', path: url.pathname, reason: 'path' }
  }
  const view = url.searchParams.get('view')?.trim()
  if (view === 'credentials' || view === 'settings') {
    return {
      kind: 'kata',
      view,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
    }
  }
  if (view === 'cron') {
    return {
      kind: 'kata',
      view,
      graph: false,
      filters: { status: [], owner: [], label: [], relationship: [] },
      cron: parseCronRoute(url.searchParams),
    }
  }
  if (view && !isSystemView(view)) {
    return { kind: 'route-error', path: url.pathname + url.search, reason: 'view' }
  }
  const normalizedView = view && isSystemView(view) ? view : undefined
  const projectUID = url.searchParams.get('scope')?.trim()
  if (projectUID && !validUID(projectUID)) {
    return { kind: 'route-error', path: url.pathname + url.search, reason: 'project_uid' }
  }
  const issueUID = url.searchParams.get('issue')?.trim()
  if (issueUID && !validUID(issueUID)) {
    const routeError: Extract<KataRoute, { kind: 'route-error' }> = {
      kind: 'route-error',
      path: url.pathname + url.search,
      reason: 'issue_uid',
    }
    if (issueUID.length > 0 && issueUID.length < 26) routeError.searchRef = issueUID
    return routeError
  }
  return {
    kind: 'kata',
    ...(normalizedView ? { view: normalizedView } : {}),
    ...(projectUID ? { projectUID } : {}),
    ...(issueUID ? { issueUID } : {}),
    graph: Boolean(issueUID) && url.searchParams.get('graph') === '1',
    filters,
  }
}

export function serializeRoute(route: KataRoute, routePath = applicationRoutePath()): string {
  if (route.kind === 'route-error') return route.path
  const query = new URLSearchParams()
  if (route.view) query.set('view', route.view)
  if (route.view === 'cron') {
    writeCronRoute(query, route.cron ?? defaultCronRoute())
    return `${routePath}?${query.toString()}`
  }
  if (route.projectUID) query.set('scope', route.projectUID)
  if (route.issueUID) query.set('issue', route.issueUID)
  if (route.graph && route.issueUID) query.set('graph', '1')
  for (const [name, values] of [
    ['label', route.filters.label],
    ['owner', route.filters.owner],
    ['relationship', route.filters.relationship],
    ['status', route.filters.status],
  ] as const) {
    for (const value of sortedUnique(values)) query.append(name, value)
  }
  if (route.filters.text) query.set('text', route.filters.text)
  const encoded = query.toString()
  return encoded ? `${routePath}?${encoded}` : routePath
}

export function defaultCronRoute(): CronRoute {
  return { tab: 'jobs', projectUIDs: [], statuses: [] }
}

function parseCronRoute(query: URLSearchParams): CronRoute {
  const tab = query.get('cron_tab')
  const route: CronRoute = {
    tab: tab === 'runs' || tab === 'workflows' ? tab : 'jobs',
    projectUIDs: sortedUnique(query.getAll('cron_project')).filter(validUID),
    statuses: [],
  }
  if (route.tab === 'runs') {
    route.statuses = sortedUnique(query.getAll('cron_status')).filter(isCronRunStatus)
  }
  const kind = query.get('cron_kind')
  const projectUID = query.get('cron_scope') ?? ''
  const uid = query.get('cron_uid') ?? ''
  if (
    (kind === 'job' || kind === 'run' || kind === 'workflow') &&
    validUID(projectUID) &&
    validUID(uid)
  ) {
    route.selection = { kind, projectUID, uid }
  }
  return route
}

function writeCronRoute(query: URLSearchParams, route: CronRoute): void {
  if (route.tab !== 'jobs') query.set('cron_tab', route.tab)
  for (const projectUID of sortedUnique(route.projectUIDs)) query.append('cron_project', projectUID)
  if (route.tab === 'runs') {
    for (const status of sortedUnique(route.statuses)) query.append('cron_status', status)
  }
  if (route.selection) {
    query.set('cron_kind', route.selection.kind)
    query.set('cron_scope', route.selection.projectUID)
    query.set('cron_uid', route.selection.uid)
  }
}

function isCronRunStatus(value: string): value is CronRunStatus {
  return cronRunStatuses.includes(value as CronRunStatus)
}

function normalizeRoutePath(value: string): string {
  return value.length > 1 ? value.replace(/\/+$/, '') : value
}

function parseFilters(query: URLSearchParams): ShareableFilters {
  const filters: ShareableFilters = {
    status: sortedUnique(query.getAll('status')),
    owner: sortedUnique(query.getAll('owner')),
    label: sortedUnique(query.getAll('label')),
    relationship: sortedUnique(query.getAll('relationship')),
  }
  const text = query.get('text')?.trim()
  if (text) filters.text = text
  return filters
}

function sortedUnique(values: readonly string[]): string[] {
  return [...new Set(values.map((value) => value.trim()).filter(Boolean))].sort()
}

function isSystemView(value: string | undefined): value is SystemView {
  return systemViews.includes(value as SystemView)
}

function validUID(value: string): boolean {
  return /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(value)
}

export function screenNameForRoute(route: KataRoute): string | undefined {
  if (route.kind === 'route-error') return undefined
  if (route.view === 'credentials' || route.view === 'settings' || route.view === 'cron') {
    return route.view
  }
  if (route.issueUID) return route.graph ? 'graph' : 'issue'
  if (route.view) return route.view === 'all-open' ? 'issues' : route.view
  return route.projectUID ? 'issues' : 'inbox'
}
