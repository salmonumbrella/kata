import { orvalFetch } from '../api/client'
import type {
  InstanceResponseBody,
  MoveTreeRequest,
  UISnapshotResponseBody,
} from '../api/generated'
import type { KataMoveDestination, KataMoveOptions } from '../kata/types'
import { normalizeKataUISnapshot } from '../kata/projection'
import { createULID } from '../ulid'

interface ReadResponse<T> {
  status: number
  data: T
  headers: Headers
}
export async function loadMoveDestination(fetcher: typeof fetch): Promise<KataMoveDestination> {
  const identity = await orvalFetch<ReadResponse<InstanceResponseBody>>(
    '/api/v1/instance',
    { method: 'GET', redirect: 'error' },
    fetcher,
  )
  if (identity.status !== 200 || !/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(identity.data.instance_uid))
    throw new Error('Destination authority is unavailable')
  const snapshot = await orvalFetch<ReadResponse<UISnapshotResponseBody>>(
    '/api/v1/ui/snapshot?view=inbox&limit=1',
    { method: 'GET', redirect: 'error' },
    fetcher,
  )
  if (snapshot.status !== 200 || !snapshot.data.capabilities?.writable)
    throw new Error('Destination is read-only or unavailable')
  return {
    authorityUID: identity.data.instance_uid,
    projects: [...normalizeKataUISnapshot(snapshot.data).projects],
  }
}

export function buildMoveTreeRequest(input: {
  sourceAuthorityUID: string
  sourceProjectUID: string
  issueUID: string
  destinationProjectUID: string
  actor: string
  options?: KataMoveOptions
}): MoveTreeRequest {
  const options = input.options ?? {}
  return {
    actor: input.actor,
    actor_authority_uid: input.sourceAuthorityUID,
    source_authority_uid: input.sourceAuthorityUID,
    source_project_uid: input.sourceProjectUID,
    issue_uid: input.issueUID,
    destination_authority_uid: options.destinationAuthorityUID ?? input.sourceAuthorityUID,
    destination_project_uid: input.destinationProjectUID,
    request_uid: createULID(),
    selection: options.issueOnly ? 'issue-only' : 'subtree',
    move_series: options.moveSeries ?? false,
    provider_disposition: options.detachProvider ? 'detach' : 'preserve',
  }
}
