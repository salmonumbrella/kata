import { afterEach, expect, it, vi } from 'vitest'
import { buildMoveTreeRequest, loadMoveDestination } from './destination'
import { createCredentialedFetch, setGeneratedFetch } from '../api/client'
import { createDaemonFetch } from '../daemons/client'
import { moveIssueTree } from '../api/generated'

afterEach(() => setGeneratedFetch(createCredentialedFetch()))
it('reads a configured destination through the broker and posts UID-only selection to the source', async () => {
  const sourceUID = '01AAAAAAAAAAAAAAAAAAAAAAAA'
  const destinationUID = '01BBBBBBBBBBBBBBBBBBBBBBBB'
  const projectUID = '01CCCCCCCCCCCCCCCCCCCCCCCC'
  let posted: Record<string, unknown> | undefined
  const upstream = vi.fn<typeof fetch>(async (input, init) => {
    const request = new Request(input, init)
    const path = new URL(request.url).pathname
    const daemon = request.headers.get('X-Kata-Web-Daemon')
    expect(request.headers.get('Authorization')).toBeNull()
    expect(request.headers.get('X-Kata-Web-Session')).toBe('session')
    if (path.endsWith('/instance')) {
      expect(daemon).toBe('destination-service')
      return Response.json({ instance_uid: destinationUID })
    }
    if (path.endsWith('/ui/snapshot')) {
      expect(daemon).toBe('destination-service')
      return Response.json({
        capabilities: { writable: true },
        catalog: [
          {
            project: { id: 4, uid: projectUID, name: 'target-project', metadata: {} },
            stats: { Open: 3 },
          },
        ],
        collection: [],
        collection_links: [],
        cursor: 1,
      })
    }
    expect(path).toBe('/api/v1/ui/proxy/api/v1/issue-moves')
    expect(daemon).toBe('source-service')
    expect(request.headers.get('X-Kata-CSRF')).toBe('csrf')
    posted = (await request.json()) as Record<string, unknown>
    return Response.json({ preview: {} })
  })
  const credentials = () => ({ session: 'session', csrf: 'csrf' })
  const destination = await loadMoveDestination(
    createCredentialedFetch(
      credentials,
      createDaemonFetch(() => 'destination-service', upstream),
    ),
  )
  expect(destination.projects[0]?.name).toBe('target-project')
  expect(destination.projects[0]?.open_count).toBe(3)
  setGeneratedFetch(
    createCredentialedFetch(
      credentials,
      createDaemonFetch(() => 'source-service', upstream),
    ),
  )
  const request = buildMoveTreeRequest({
    sourceAuthorityUID: sourceUID,
    sourceProjectUID: '01DDDDDDDDDDDDDDDDDDDDDDDD',
    issueUID: '01EEEEEEEEEEEEEEEEEEEEEEEE',
    destinationProjectUID: projectUID,
    actor: 'tester',
    options: { destinationAuthorityUID: destination.authorityUID, issueOnly: true },
  })
  await moveIssueTree({ request, dry_run: true })
  expect(posted).toEqual({
    request: expect.objectContaining({
      source_authority_uid: sourceUID,
      destination_authority_uid: destinationUID,
      destination_project_uid: projectUID,
      selection: 'issue-only',
    }),
    dry_run: true,
  })
  expect(JSON.stringify(posted)).not.toContain('token')
})
