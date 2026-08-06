import { describe, expect, it } from 'vitest'
import { LocalReplica } from './localReplica'
import { SessionContentAdapter } from './sessionContentAdapter'
import { SessionContentRepository as SyncSessionContentRepository } from './sessionContentRepository'
import { SessionContentRepository, selectSessionAvailability, selectSessionView } from '../repositories/sessionContent'
import type { Sequence, SubscriptionEventData } from '../protocol/types'
import { SyncReadError } from './errors'

const snapshot = (id: string) => ({
  schema_version: 1,
  session: {
    id, version: 2, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
    archived: false, last_used_at: '2025-01-01T00:00:00Z', has_unread_result: false, status: 'idle',
    show_reasoning: false, full_access: false, debug: { request_bodies: false }, context: {}, save_tool_results: false,
  },
  history: { items: [], descriptor: { limit: 20, align_turn: false, visible_only: true, has_more_before: false, has_more_after: false } },
  active_run: null,
  compaction: { checkpoints: [], truncated: false },
})

function seed(replica: LocalReplica, id: string, revision = '1'): void {
  const resource = { type: 'session_content' as const, id }
  replica.applySnapshot(resource, new SessionContentAdapter(id), snapshot(id), {
    streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: revision, generation: 1,
  })
}

describe('session-content repository selectors', () => {
  it('keeps session A references stable when only session B changes', () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    seed(replica, 'session_a')
    seed(replica, 'session_b')
    const before = selectSessionView(repository, 'session_a')
    const beforeHistory = before.history
    let notifications = 0
    const unsubscribe = repository.observe('session_a', () => { notifications += 1 })

    replica.markStale({ type: 'session_content', id: 'session_b' }, new SyncReadError('transport', 'offline'), 1)
    const after = selectSessionView(repository, 'session_a')
    expect(after).toBe(before)
    expect(after.history).toBe(beforeHistory)
    expect(notifications).toBe(0)
    unsubscribe()
  })

  it('reports availability without erasing durable data on a stale read', () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    seed(replica, 'session_a')
    const ready = repository.get('session_a')
    expect(selectSessionAvailability(repository, 'session_a').status).toBe('ready')
    replica.markStale({ type: 'session_content', id: 'session_a' }, new SyncReadError('sequence_gap', 'recovery'), 2)
    const stale = repository.get('session_a')
    expect(stale).not.toBe(ready)
    expect(stale.availability.status).toBe('stale')
    expect(stale.session?.id).toBe('session_a')
    expect(stale.history).toBe(ready.history)
    expect(stale.error?.code).toBe('sequence_gap')
  })

  it('selects the domain run state without exposing replica transport metadata', () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    seed(replica, 'session_a')
    const adapter = new SessionContentAdapter('session_a')
    replica.applyTransient({ type: 'session_content', id: 'session_a' }, adapter, {
      type: 'run.started', session_id: 'session_a', run_id: 'run_a', run_cursor: '1', status: 'running',
    } as unknown as SubscriptionEventData, 1)
    const view = repository.get('session_a')
    expect(view.runState?.runCursor).toBe('1')
    expect(view.runState?.runID).toBe('run_a')
    expect(Object.keys(view)).not.toContain('subscription_id')
    expect(Object.keys(view)).not.toContain('sequence')
    expect(Object.keys(view)).not.toContain('generation')
  })
})
