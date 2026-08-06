import { describe, expect, it } from 'vitest'
import type { JsonObject } from '../protocol/types'
import { LocalReplica } from './localReplica'
import { ProjectIndexAdapter, type ProjectSummary } from './projectIndexAdapter'
import { ProjectIndexStore } from './projectIndexStore'

const summary = (id: string, overrides: Partial<ProjectSummary> = {}): ProjectSummary => ({
  id,
  root: `/workspace/${id}`,
  display_name: id,
  archived: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
  ...overrides,
})

const resource = { type: 'project_index' as const, id: 'server' }
const wire = (value: ProjectSummary): JsonObject => value as unknown as JsonObject

describe('ProjectIndexAdapter', () => {
  it('strictly validates the singleton snapshot and keeps stable ordering/reference identity', () => {
    const adapter = new ProjectIndexAdapter()
    const first = adapter.decodeSnapshot({ projects: [wire(summary('project_b')), wire(summary('project_a'))] }, undefined)
    expect(first.orderedIDs).toEqual(['project_a', 'project_b'])
    expect(() => adapter.decodeSnapshot({ projects: [{ ...summary('project_a'), extra: true }] }, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ projects: [summary('project_a'), summary('project_a')] }, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ projects: [{ ...summary('project_a'), id: 'project a' }] }, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ projects: [{ ...summary('project_a'), updated_at: 'not-a-date' }] }, undefined)).toThrow()

    const same = adapter.decodeSnapshot({ projects: [wire(summary('project_a')), wire(summary('project_b'))] }, first)
    expect(same).toBe(first)
    expect(same.summariesByID.project_a).toBe(first.summariesByID.project_a)
  })

  it('applies complete metadata upserts and keyed removes without array patches', () => {
    const adapter = new ProjectIndexAdapter()
    const first = adapter.decodeSnapshot({ projects: [wire(summary('project_a')), wire(summary('project_b'))] }, undefined)
    const renamed = adapter.applyChange(first, [{
      op: 'upsert', key: 'project_b', value: wire(summary('project_b', { display_name: 'Renamed', updated_at: '2025-01-02T00:00:00Z' })),
    }])
    expect(renamed.summariesByID.project_a).toBe(first.summariesByID.project_a)
    expect(renamed.summariesByID.project_b).not.toBe(first.summariesByID.project_b)
    expect(renamed.summariesByID.project_b.display_name).toBe('Renamed')
    const removed = adapter.applyChange(renamed, [{ op: 'remove', key: 'project_a' }])
    expect(removed.summariesByID.project_a).toBeUndefined()
    expect(removed.orderedIDs).toEqual(['project_b'])
    expect(() => adapter.applyChange(first, [{ op: 'remove', key: 'project_a', value: null }])).toThrow()
    expect(() => adapter.applyChange(first, [{ op: 'upsert', key: 'project_a', value: wire(summary('project_b')) }])).toThrow()
  })

  it('projects LocalReplica reads with stable models and keyed lookup', () => {
    const replica = new LocalReplica()
    const store = new ProjectIndexStore(replica)
    const adapter = new ProjectIndexAdapter()
    replica.applySnapshot(resource, adapter, { projects: [wire(summary('project_a')), wire(summary('project_b'))] }, {
      streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1,
    })
    const before = store.getSnapshot()
    const projectA = store.getByID('project_a')
    replica.applyChange(resource, adapter, [{
      op: 'upsert', key: 'project_b', value: wire(summary('project_b', { archived: true, updated_at: '2025-01-02T00:00:00Z' })),
    }], { streamEpoch: 'epoch', sequence: '1' as never, resourceRevision: '1', generation: 1 })
    const after = store.getSnapshot()
    expect(after).not.toBe(before)
    expect(store.getByID('project_a')).toBe(projectA)
    expect(after.active).toHaveLength(1)
    expect(after.archived).toHaveLength(1)
    expect(store.getSnapshot()).toBe(after)
  })
})
