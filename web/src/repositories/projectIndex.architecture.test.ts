import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { LocalReplica } from '../sync/localReplica'
import { ProjectIndexAdapter, type ProjectSummary } from '../sync/projectIndexAdapter'
import { ProjectIndexStore } from '../sync/projectIndexStore'
import { ProjectIndexRepository } from './projectIndex'
import { SyncReadError } from '../sync/errors'

const project = (id: string): ProjectSummary => ({
  id, root: `/workspace/${id}`, display_name: id, archived: false,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
})

describe('domain-facing Project Index repository', () => {
  it('exposes loading, ready, and stale states without losing the last data', () => {
    const replica = new LocalReplica()
    const store = new ProjectIndexStore(replica)
    const repository = new ProjectIndexRepository(store)
    const resource = { type: 'project_index' as const, id: 'server' }
    const adapter = new ProjectIndexAdapter()

    expect(repository.getSnapshot()).toMatchObject({ status: 'loading', summaries: [] })
    replica.applySnapshot(resource, adapter, { projects: [project('project_a')] }, {
      streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1,
    })
    const ready = repository.getSnapshot()
    expect(ready.status).toBe('ready')
    expect(ready.summaries.map((summary) => summary.id)).toEqual(['project_a'])

    replica.markStale(resource)
    const stale = repository.getSnapshot()
    expect(stale.status).toBe('stale')
    expect(stale.summaries.map((summary) => summary.id)).toEqual(['project_a'])
    expect(stale.summaries[0]).toBe(ready.summaries[0])

    replica.markError(resource, new SyncReadError('server', 'secret protocol details'))
    expect(repository.getSnapshot()).toMatchObject({
      status: 'stale',
      error: { code: 'unavailable', message: 'Project list is temporarily unavailable' },
    })
  })

  it('keeps unchanged keyed references stable while another project changes', () => {
    const replica = new LocalReplica()
    const store = new ProjectIndexStore(replica)
    const repository = new ProjectIndexRepository(store)
    const resource = { type: 'project_index' as const, id: 'server' }
    const adapter = new ProjectIndexAdapter()
    replica.applySnapshot(resource, adapter, { projects: [project('project_a'), project('project_b')] }, {
      streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1,
    })
    const before = repository.getSnapshot()
    const projectA = repository.getByID('project_a')
    replica.applyChange(resource, adapter, [{
      op: 'upsert', key: 'project_b', value: { ...project('project_b'), display_name: 'Renamed', updated_at: '2025-01-02T00:00:00Z' },
    }], { streamEpoch: 'epoch', sequence: '1' as never, resourceRevision: '1', generation: 1 })
    expect(repository.getSnapshot()).not.toBe(before)
    expect(repository.getByID('project_a')).toBe(projectA)
  })

  it('publishes background upserts/removes immediately and ignores unrelated resources', () => {
    const replica = new LocalReplica()
    const store = new ProjectIndexStore(replica)
    const repository = new ProjectIndexRepository(store)
    const resource = { type: 'project_index' as const, id: 'server' }
    const adapter = new ProjectIndexAdapter()
    replica.applySnapshot(resource, adapter, { projects: [project('project_a')] }, {
      streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1,
    })
    const beforeUnrelatedChange = repository.getSnapshot()
    let notifications = 0
    const release = repository.subscribe(() => { notifications += 1 })

    replica.markStale({ type: 'provider_settings', id: 'server' })
    expect(repository.getSnapshot()).toBe(beforeUnrelatedChange)
    expect(notifications).toBe(0)

    replica.applyChange(resource, adapter, [{
      op: 'upsert', key: 'project_b', value: { ...project('project_b') },
    }], { streamEpoch: 'epoch', sequence: '1' as never, resourceRevision: '1', generation: 1 })
    expect(repository.getSnapshot().summaries.map((summary) => summary.id)).toEqual(['project_a', 'project_b'])
    expect(notifications).toBe(1)

    replica.applyChange(resource, adapter, [{ op: 'remove', key: 'project_a' }], {
      streamEpoch: 'epoch', sequence: '2' as never, resourceRevision: '2', generation: 1,
    })
    expect(repository.getSnapshot().summaries.map((summary) => summary.id)).toEqual(['project_b'])
    expect(notifications).toBe(2)
    release()
  })

  it('waits on repository publications with bounded cancellation instead of polling', async () => {
    const replica = new LocalReplica()
    const store = new ProjectIndexStore(replica)
    const repository = new ProjectIndexRepository(store)
    const resource = { type: 'project_index' as const, id: 'server' }
    const adapter = new ProjectIndexAdapter()
    const pending = repository.waitForProject('project_a', true, { timeoutMS: 100 })
    replica.applySnapshot(resource, adapter, { projects: [project('project_a')] }, {
      streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1,
    })
    await expect(pending).resolves.toMatchObject({ status: 'ready' })

    const controller = new AbortController()
    const cancelled = repository.waitForProject('project_b', true, { signal: controller.signal, timeoutMS: 1000 })
    controller.abort()
    await expect(cancelled).rejects.toMatchObject({ code: 'cancelled' })
    await expect(repository.waitForProject('project_b', true, { timeoutMS: 1 })).rejects.toMatchObject({ code: 'timeout' })
  })

  it('keeps the page-facing module free of transport and lifecycle details', () => {
    const source = readFileSync(new URL('./projectIndex.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"][^'"]*(?:api|sync|protocol|transport|blob|lifecycle)/)
    expect(source).not.toMatch(/\b(?:ResourceKey|Sequence|BlobDescriptor|WebSocketTransport|SyncRuntime|fetch|WebSocket)\b/)
  })

  it('keeps project mutations out of the legacy REST/reload path', () => {
    const source = readFileSync(new URL('../App.tsx', import.meta.url), 'utf8')
    expect(source).not.toMatch(/\b(?:setProjects|loadProjects)\b/)
    expect(source).not.toMatch(/api\.(?:createProject|renameProject|archiveProject|restoreProject|deleteProject)\s*\(/)
    expect(source).not.toMatch(/api\.projects\([^)]*\).*set/i)
    expect(source).not.toContain('legacyProjectEnumerationRef')
    expect(source).not.toContain('api.projects(')
    expect(source).toContain('projectCommands.createProject')
    expect(source).toContain('projectCommands.renameProject')
    expect(source).toContain('projectCommands.archiveProject')
    expect(source).toContain('projectCommands.restoreProject')
    expect(source).toContain('projectCommands.deleteProject')
  })
})
