// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { JsonObject } from '../protocol/types'
import { LocalReplica } from '../sync/localReplica'
import { SessionIndexAdapter, type SessionSummary } from '../sync/sessionIndexAdapter'
import { SessionIndexRepository as SyncRepository } from '../sync/sessionIndexRepository'
import { useSession, useSessionIndex } from '../hooks/useSessionIndex'
import { SessionIndexRepository } from './sessionIndex'

const summary = (id: string, overrides: Partial<SessionSummary> = {}): SessionSummary => ({
  session_id: id,
  project_id: 'project_a',
  parent_session_id: null,
  display_name: id,
  archived: false,
  status: 'idle',
  run_id: null,
  resource_revision: '1',
  updated_at: '2025-01-01T00:00:00Z',
  has_unread_result: false,
  ...overrides,
})

function snapshot(replica: LocalReplica, sessions: SessionSummary[]): void {
  replica.applySnapshot({ type: 'session_index', id: 'project_a' }, new SessionIndexAdapter('project_a'), { sessions: sessions.map((value) => value as unknown as JsonObject) }, {
    streamEpoch: 'epoch_1', sequence: '1' as never, resourceRevision: '1', generation: 1,
  })
}

describe('domain Session Index repository and hooks', () => {
  it('transitions loading → ready → stale → ready without exposing sync metadata', () => {
    const replica = new LocalReplica()
    const repository = new SessionIndexRepository(new SyncRepository(replica))
    const { result } = renderHook(() => useSessionIndex(repository, 'project_a'))
    expect(result.current.status).toBe('loading')
    act(() => snapshot(replica, [summary('a')]))
    expect(result.current.status).toBe('ready')
    act(() => replica.markStale({ type: 'session_index', id: 'project_a' }, undefined, 2))
    expect(result.current.status).toBe('stale')
    act(() => snapshot(replica, [summary('a', { resource_revision: '2' })]))
    expect(result.current.status).toBe('ready')
    expect(result.current).not.toHaveProperty('sequence')
    expect(result.current).not.toHaveProperty('blob')
  })

  it('keeps a per-session hook from rerendering when only B changes', () => {
    const replica = new LocalReplica()
    const repository = new SessionIndexRepository(new SyncRepository(replica))
    act(() => snapshot(replica, [summary('a'), summary('b')]))
    let renders = 0
    const { result } = renderHook(() => {
      renders += 1
      return useSession(repository, 'project_a', 'a')
    })
    const before = result.current
    const beforeRenders = renders
    act(() => replica.applyChange({ type: 'session_index', id: 'project_a' }, new SessionIndexAdapter('project_a'), [{
      op: 'upsert', key: 'b', value: summary('b', { status: 'completed', run_id: 'run_b', has_unread_result: true, resource_revision: '2' }) as unknown as JsonObject,
    }], { streamEpoch: 'epoch_1', sequence: '2' as never, resourceRevision: '2', generation: 1 }))
    expect(result.current).toBe(before)
    expect(result.current.summary).toBe(before.summary)
    expect(result.current.summary).toMatchObject({ session_id: 'a' })
    expect(renders).toBe(beforeRenders)
  })
})
