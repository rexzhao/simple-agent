import { describe, expect, it } from 'vitest'
import type { JsonObject, Sequence } from '../protocol/types'
import { LocalReplica } from './localReplica'
import { SessionIndexAdapter, type SessionSummary } from './sessionIndexAdapter'
import { SessionIndexRepository } from './sessionIndexRepository'
import { createCurrentProjectSignal, SessionIndexInterestPolicy } from './interestPolicy'

const baseSummary = (overrides: Partial<SessionSummary> = {}): SessionSummary => ({
  session_id: 'session_a',
  project_id: 'project_a',
  parent_session_id: null,
  display_name: 'Alpha',
  archived: false,
  status: 'idle',
  run_id: null,
  resource_revision: '1',
  updated_at: '2025-01-01T00:00:00Z',
  has_unread_result: false,
  ...overrides,
})

function wire(summary: SessionSummary): JsonObject {
  return summary as unknown as JsonObject
}

const snapshot = (...summaries: SessionSummary[]) => ({ sessions: summaries.map(wire) })

function applySnapshot(replica: LocalReplica, projectID: string, data: unknown): void {
  const adapter = new SessionIndexAdapter(projectID)
  replica.applySnapshot(
    { type: 'session_index', id: projectID },
    adapter,
    data,
    { streamEpoch: 'epoch_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 },
  )
}

describe('SessionIndexAdapter', () => {
  it('validates full summaries, nullable fields, replacement, and stable ordering', () => {
    const adapter = new SessionIndexAdapter('project_a')
    const idle = baseSummary({ session_id: 'a', display_name: '', parent_session_id: null, run_id: null })
    const completed = baseSummary({
      session_id: 'b',
      display_name: 'Beta',
      status: 'completed',
      run_id: 'run_b',
      parent_session_id: 'a',
      resource_revision: '0002',
      has_unread_result: true,
    })
    const value = adapter.decodeSnapshot(snapshot(completed, idle), undefined)
    expect(value.orderedIDs).toEqual(['a', 'b'])
    expect(value.summariesByID.b.parent_session_id).toBe('a')
    expect(value.summariesByID.b.resource_revision).toBe('0002')
    expect(() => adapter.decodeSnapshot(snapshot({ ...completed, run_id: null }), undefined)).toThrow()

    const same = adapter.decodeSnapshot(snapshot(idle, completed), value)
    expect(same).toBe(value)
    expect(same.summariesByID.a).toBe(value.summariesByID.a)
    expect(same.summariesByID.b).toBe(value.summariesByID.b)

    const opaqueRevisionChange = adapter.decodeSnapshot(snapshot({ ...idle, resource_revision: '01' }, { ...completed, resource_revision: '2' }), value)
    expect(opaqueRevisionChange).not.toBe(value)
    expect(opaqueRevisionChange.summariesByID.b.resource_revision).toBe('2')
    expect(opaqueRevisionChange.summariesByID.b).not.toBe(value.summariesByID.b)

    expect(() => adapter.decodeSnapshot({ sessions: [{ ...wire(idle), run_id: 42 }] }, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ sessions: [{ ...wire(idle), project_id: 'other' }] }, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ sessions: [{ ...wire(idle), future: true }] }, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ ...idle, session_id: '   ' }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ ...idle, project_id: '   ' }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ ...idle, parent_session_id: '   ' }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ ...idle, run_id: '   ' }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ ...idle, display_name: '\ud800' }), undefined)).toThrow()
    for (const status of ['queued', 'running', 'completed', 'failed', 'interrupted'] as const) {
      expect(() => adapter.decodeSnapshot(snapshot({ ...idle, status, run_id: null }), undefined)).toThrow()
      expect(() => adapter.decodeSnapshot(snapshot({ ...idle, status, run_id: 'run_ok' }), undefined)).not.toThrow()
    }
    expect(() => adapter.decodeSnapshot(snapshot({ ...idle, status: 'idle', run_id: 'run_bad' }), undefined)).toThrow()
  })

  it('accepts only complete keyed upsert/remove operations and commits atomically', () => {
    const adapter = new SessionIndexAdapter('project_a')
    const first = adapter.decodeSnapshot(snapshot(baseSummary()), undefined)
    const updated = baseSummary({ status: 'completed', run_id: 'run_1', has_unread_result: true, resource_revision: '2' })
    const changed = adapter.applyChange(first, [{ op: 'upsert', key: 'session_a', value: wire(updated) }])
    expect(changed.summariesByID.session_a.status).toBe('completed')
    expect(changed.summariesByID.session_a).not.toBe(first.summariesByID.session_a)

    const removed = adapter.applyChange(changed, [{ op: 'remove', key: 'session_a' }])
    expect(removed.orderedIDs).toEqual([])
    expect(() => adapter.applyChange(changed, [{ op: 'upsert', key: 'session_a', value: { session_id: 'session_a' } }])).toThrow()
    expect(() => adapter.applyChange(changed, [{ op: 'remove', key: 'session_a', value: null }])).toThrow()
    expect(() => adapter.applyChange(changed, [{ op: 'metadata.patch', key: 'session_a', value: {} }])).toThrow()
  })
})

describe('SessionIndexRepository and interest policy', () => {
  it('does not notify on metadata-only reconnect no-ops', () => {
    const replica = new LocalReplica()
    const resource = { type: 'session_index' as const, id: 'project_a' }
    let notifications = 0
    replica.subscribe(() => { notifications += 1 })
    applySnapshot(replica, 'project_a', snapshot(baseSummary()))
    expect(notifications).toBe(1)
    replica.markReady(resource, { streamEpoch: 'epoch_1', sequence: '1' as Sequence, generation: 1 })
    expect(notifications).toBe(1)
    replica.markStale(resource, undefined, 2)
    expect(notifications).toBe(2)
    replica.markStale(resource, undefined, 3)
    expect(notifications).toBe(2)
  })

  it('keeps read models stable per project and preserves entity references', () => {
    const replica = new LocalReplica()
    const repository = new SessionIndexRepository(replica)
    applySnapshot(replica, 'project_a', snapshot(baseSummary({ session_id: 'a' }), baseSummary({ session_id: 'b', display_name: 'Bravo' })))
    const aBefore = repository.getProjectReadModel('project_a')
    const entityA = repository.selectByID('project_a', 'a')
    const bBefore = repository.selectByID('project_a', 'b')

    replica.applyChange(
      { type: 'session_index', id: 'project_a' },
      new SessionIndexAdapter('project_a'),
      [{ op: 'upsert', key: 'b', value: wire(baseSummary({ session_id: 'b', display_name: 'Bravo', status: 'completed', run_id: 'run_b', has_unread_result: true, resource_revision: '2' })) }],
      { streamEpoch: 'epoch_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1 },
    )
    const aAfter = repository.getProjectReadModel('project_a')
    const bAfter = repository.selectByID('project_a', 'b')
    expect(aAfter).not.toBe(aBefore)
    expect(repository.selectByID('project_a', 'a')).toBe(entityA)
    expect(bAfter).not.toBe(bBefore)
    expect(bAfter).toMatchObject({ status: 'completed', has_unread_result: true })

    replica.markStale({ type: 'session_index', id: 'project_a' }, undefined, 2)
    expect(repository.getProjectReadModel('project_a')).toMatchObject({ status: 'stale' })
    expect(repository.selectByID('project_a', 'a')).toBe(entityA)
    const empty = new SessionIndexRepository(new LocalReplica()).getProjectReadModel('missing')
    expect(empty.status).toBe('loading')
  })

  it('keeps the current project subscription alive without page subscribers', () => {
    const signal = createCurrentProjectSignal('project_a')
    const active: string[] = []
    const released: string[] = []
    const runtime = {
      subscribe: (resource: { type: 'session_index'; id: string }) => {
        active.push(resource.id)
        return () => released.push(resource.id)
      },
    }
    const policy = new SessionIndexInterestPolicy(runtime, signal)
    policy.start()
    policy.start()
    expect(active).toEqual(['project_a'])
    signal.set('project_b')
    expect(released).toEqual(['project_a'])
    expect(active).toEqual(['project_a', 'project_b'])
    signal.set('project_b')
    policy.stop()
    expect(released).toEqual(['project_a', 'project_b'])
  })
})
