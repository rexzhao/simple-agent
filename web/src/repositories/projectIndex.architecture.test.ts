import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { LocalReplica } from '../sync/localReplica'
import { ProjectIndexAdapter, type ProjectSummary } from '../sync/projectIndexAdapter'
import { ProjectIndexStore } from '../sync/projectIndexStore'
import { ProjectIndexRepository } from './projectIndex'

const project = (id: string): ProjectSummary => ({
  id, root: `/workspace/${id}`, display_name: id, archived: false,
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
})

describe('domain-facing Project Index repository', () => {
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

  it('keeps the page-facing module free of transport and lifecycle details', () => {
    const source = readFileSync(new URL('./projectIndex.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"][^'"]*(?:api|sync|protocol|transport|blob|lifecycle)/)
    expect(source).not.toMatch(/\b(?:ResourceKey|Sequence|BlobDescriptor|WebSocketTransport|SyncRuntime|fetch|WebSocket)\b/)
  })
})
