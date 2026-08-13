import { describe, expect, it } from 'vitest'
import { LocalReplica } from './localReplica'
import { SessionContentAdapter } from './sessionContentAdapter'
import { SessionContentRepository as SyncSessionContentRepository } from './sessionContentRepository'
import { SessionContentObservationError, SessionContentRepository, selectSessionAvailability, selectSessionView } from '../repositories/sessionContent'
import type { Sequence, SubscriptionEventData } from '../protocol/types'
import { SyncReadError } from './errors'
import type { SessionContentHistoryWindow } from '../domain/sessionContent'

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

  it('keeps durable content when a terminal lifecycle omitted its only snapshot', () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    const resource = { type: 'session_content' as const, id: 'session_a' }
    const initial = snapshot(resource.id) as any
    initial.session.status = 'running'
    initial.session.running_run_id = 'run_a'
    initial.session.running_turn_id = 'turn_a'
    initial.history.items = [{
      key: { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_large' }, seq: 1,
      created_at: '2025-01-01T00:00:00Z', kind: 'message', visibility: 'visible', audience: 'user', status: 'completed',
      message: { role: 'assistant', content: { inline: 'durable large response' } },
    }]
    initial.history.descriptor = { ...initial.history.descriptor, oldest_item_seq: '1', newest_item_seq: '1' }
    initial.active_run = { run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch_a', run_cursor: '0', replay_available: false, recovery_required: false }
    const adapter = new SessionContentAdapter(resource.id)
    replica.applySnapshot(resource, adapter, initial, { streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 })
    replica.applyTransient(resource, adapter, { type: 'run.started', session_id: 'session_a', run_id: 'run_a', run_cursor: '1', status: 'running' } as SubscriptionEventData, 1)
    replica.applyTransient(resource, adapter, { type: 'assistant.message.started', session_id: 'session_a', run_id: 'run_a', run_cursor: '2', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_large', message_revision: '0' } as SubscriptionEventData, 1)
    replica.applyTransient(resource, adapter, { type: 'assistant.message.completed', session_id: 'session_a', run_id: 'run_a', run_cursor: '3', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_large', message_revision: '1', snapshot_omitted: true } as SubscriptionEventData, 1)

    expect(repository.get(resource.id).history.items[0].message?.content?.inline).toBe('durable large response')
  })

  it('observes terminal and durable run identities without requiring an active row', async () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    const resource = { type: 'session_content' as const, id: 'session_run_authority' }
    const current = snapshot(resource.id) as any
    current.session.last_run_id = 'run_terminal'
    current.session.latest_run_id = 'run_terminal'
    replica.applySnapshot(resource, new SessionContentAdapter(resource.id), current, {
      streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1,
    })

    expect(repository.hasObservedRun(resource.id, 'run_terminal')).toBe(true)
    expect(repository.hasObservedRun(resource.id, 'other_session_run')).toBe(false)
    const pending = repository.waitForRunObserved(resource.id, 'run_next', { timeoutMS: 500 })
    replica.applyChange(resource, new SessionContentAdapter(resource.id), [{
      op: 'metadata.replace',
      metadata: { ...current.session, current_run_id: 'run_next', last_run_id: 'run_terminal', latest_run_id: 'run_terminal' },
    }], { streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1 })
    await expect(pending).resolves.toMatchObject({ session: { current_run_id: 'run_next' } })
  })

  it('merges an older command page by stable identity and ignores a late old generation', async () => {
    const replica = new LocalReplica()
    let resolvePage: ((page: SessionContentHistoryWindow) => void) | undefined
    let readerCall = 0
    let lateResolve: ((page: SessionContentHistoryWindow) => void) | undefined
    const reader = (_sessionID: string): Promise<SessionContentHistoryWindow> => new Promise((resolve) => {
      if (readerCall++ === 0) resolvePage = resolve
      else lateResolve = resolve
    })
    const syncRepository = new SyncSessionContentRepository(replica, { historyReader: reader })
    const repository = new SessionContentRepository(syncRepository)
    const initial = snapshot('session_a') as any
    initial.history = {
      items: [{
        key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'item_20' }, seq: 20,
        created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user',
      }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    }
    replica.applySnapshot({ type: 'session_content', id: 'session_a' }, new SessionContentAdapter('session_a'), initial, {
      streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1,
    })
    const pending = repository.readHistory('session_a', { cursor: 20, direction: 'before', limit: 20 })
    expect(syncRepository.historyState('session_a').loading).toBe(true)
    resolvePage!({
      items: [
        { key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'item_10' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' },
        { key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'item_20' }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user' },
      ],
      descriptor: { limit: 20, before_item_seq: '20', oldest_item_seq: '10', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await pending
    expect(repository.get('session_a').history.items.map((item) => item.seq)).toEqual([10, 20])
    expect(repository.get('session_a').history.descriptor.has_more_before).toBe(false)

    const latePage = repository.readHistory('session_a', { cursor: 10, direction: 'before', limit: 20 })
    const replacement = snapshot('session_a')
    replacement.history = { items: [], descriptor: { limit: 20, align_turn: false, visible_only: true, has_more_before: false, has_more_after: false } }
    replica.applySnapshot({ type: 'session_content', id: 'session_a' }, new SessionContentAdapter('session_a'), replacement, {
      streamEpoch: 'stream_2', sequence: '1' as Sequence, resourceRevision: '2', generation: 2,
    })
    lateResolve!({
      items: [{ key: { turn_id: 'turn_late', agent_iteration: 1, item_id: 'item_late' }, seq: 1, created_at: '2025-01-01T00:00:01Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '1', newest_item_seq: '1', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await latePage
    expect(repository.get('session_a').history.items).toHaveLength(0)
  })

  it('keeps the older range while live authority advances the latest descriptor', async () => {
    const replica = new LocalReplica()
    let resolvePage: ((page: SessionContentHistoryWindow) => void) | undefined
    const syncRepository = new SyncSessionContentRepository(replica, {
      historyReader: () => new Promise((resolve) => { resolvePage = resolve }),
    })
    const repository = new SessionContentRepository(syncRepository)
    const resource = { type: 'session_content' as const, id: 'session_live_window' }
    const initial = snapshot(resource.id) as any
    initial.history = {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_20' }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    }
    const adapter = new SessionContentAdapter(resource.id)
    replica.applySnapshot(resource, adapter, initial, { streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 })
    const pending = repository.readHistory(resource.id, { cursor: 20, direction: 'before', limit: 20 })
    resolvePage!({
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_10' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, before_item_seq: '20', oldest_item_seq: '10', newest_item_seq: '10', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await pending

    // The provider publishes item upserts and its descriptor as one atomic
    // authority change. The retained older page remains valid, but the live
    // latest boundary must move from 20 to 30.
    replica.applyChange(resource, adapter, [
      { op: 'item.upsert', item: { key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_30' }, seq: 30, created_at: '2025-01-01T00:00:30Z', kind: 'message', visibility: 'visible', audience: 'user' } },
      { op: 'history.window.descriptor.replace', descriptor: { limit: 20, after_item_seq: '19', oldest_item_seq: '20', newest_item_seq: '30', align_turn: false, visible_only: true, has_more_before: true, has_more_after: true } },
    ], { streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1 })

    const view = repository.get(resource.id)
    expect(view.history.items.map((item) => item.seq)).toEqual([10, 20, 30])
    expect(view.history.descriptor).toMatchObject({
      oldest_item_seq: '10', newest_item_seq: '30',
      has_more_before: false, has_more_after: true,
    })
  })

  it('drops retained older items when the authority replaces the history window', async () => {
    const replica = new LocalReplica()
    let resolvePage: ((page: SessionContentHistoryWindow) => void) | undefined
    const syncRepository = new SyncSessionContentRepository(replica, {
      historyReader: () => new Promise((resolve) => { resolvePage = resolve }),
    })
    const repository = new SessionContentRepository(syncRepository)
    const resource = { type: 'session_content' as const, id: 'session_reset_window' }
    const initial = snapshot(resource.id) as any
    initial.history = {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_20' }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    }
    const adapter = new SessionContentAdapter(resource.id)
    replica.applySnapshot(resource, adapter, initial, { streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 })
    const pending = repository.readHistory(resource.id, { cursor: 20, direction: 'before', limit: 20 })
    resolvePage!({
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_10' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, before_item_seq: '20', oldest_item_seq: '10', newest_item_seq: '10', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await pending
    expect(repository.get(resource.id).history.items.map((item) => item.seq)).toEqual([10, 20])

    const replacement = {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'summary_50' }, seq: 50, created_at: '2025-01-01T00:00:50Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '50', newest_item_seq: '50', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    }
    replica.applyChange(resource, adapter, [{ op: 'history.window.replace', window: replacement }], {
      streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1,
    })
    expect(repository.get(resource.id).history.items.map((item) => item.seq)).toEqual([50])
    expect(repository.get(resource.id).history.descriptor.oldest_item_seq).toBe('50')
  })

  it('invalidates retained history on compaction without dropping ordinary live upserts', async () => {
    const replica = new LocalReplica()
    const resolves: Array<(page: SessionContentHistoryWindow) => void> = []
    const syncRepository = new SyncSessionContentRepository(replica, {
      historyReader: () => new Promise((resolve) => { resolves.push(resolve) }),
    })
    const repository = new SessionContentRepository(syncRepository)
    const resource = { type: 'session_content' as const, id: 'session_compaction_window' }
    const initial = snapshot(resource.id) as any
    initial.history = {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_20' }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    }
    const adapter = new SessionContentAdapter(resource.id)
    replica.applySnapshot(resource, adapter, initial, { streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 })
    const pending = repository.readHistory(resource.id, { cursor: 20, direction: 'before', limit: 20 })
    resolves[0]({
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'item_10' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, before_item_seq: '20', oldest_item_seq: '10', newest_item_seq: '10', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await pending

    replica.applyChange(resource, adapter, [{ op: 'compaction.replace', compaction: {
      checkpoints: [{ id: 'checkpoint_1', created_at: '2025-01-01T00:01:00Z', reason: 'manual', phase: 'completed', trigger: 'manual', summary_item_id: 'summary_50', replacement_history: [] }],
      truncated: false,
    } }], { streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1 })
    expect(repository.get(resource.id).history.items.map((item) => item.seq)).toEqual([20])
  })

  it('keeps a same-generation durable history window but drops it on runtime eviction', async () => {
    const replica = new LocalReplica()
    const resolves: Array<(page: SessionContentHistoryWindow) => void> = []
    const syncRepository = new SyncSessionContentRepository(replica, {
      historyReader: () => new Promise((resolve) => { resolves.push(resolve) }),
    })
    const repository = new SessionContentRepository(syncRepository)
    const initial = snapshot('session_a') as any
    initial.history = {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'new' }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    }
    const resource = { type: 'session_content' as const, id: 'session_a' }
    replica.applySnapshot(resource, new SessionContentAdapter('session_a'), initial, {
      streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1,
    })
    const pending = repository.readHistory('session_a', { cursor: 20, direction: 'before', limit: 20 })
    expect(repository.get('session_a').historyState.loading).toBe(true)
    resolves[0]({
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'old' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, before_item_seq: '20', oldest_item_seq: '10', newest_item_seq: '10', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    })
    await pending
    expect(repository.get('session_a').history.items.map((item) => item.key.item_id)).toEqual(['old', 'new'])

    // A released resource may retain its durable replica window, but an
    // actual runtime eviction must also invalidate the repository-owned page
    // cache. The page/blob result below deliberately ignores AbortSignal.
    const late = repository.readHistory('session_a', { cursor: 10, direction: 'before', limit: 20 })
    replica.evict(resource)
    resolves[1]({
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'late-old' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '10', newest_item_seq: '10', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await late
    expect(repository.get('session_a').history.items).toHaveLength(0)
    expect(repository.get('session_a').historyState).toEqual({ loading: false, version: 0 })
  })

  it('publishes history loading/success as external-store snapshot identities and isolates sessions', async () => {
    const replica = new LocalReplica()
    let resolvePage: ((page: SessionContentHistoryWindow) => void) | undefined
    const reader = () => new Promise<SessionContentHistoryWindow>((resolve) => { resolvePage = resolve })
    const syncRepository = new SyncSessionContentRepository(replica, { historyReader: reader })
    const repository = new SessionContentRepository(syncRepository)
    const initial = snapshot('session_a') as any
    initial.history = {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'new' }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    }
    replica.applySnapshot({ type: 'session_content', id: 'session_a' }, new SessionContentAdapter('session_a'), initial, { streamEpoch: 'epoch', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 })
    seed(replica, 'session_b')
    const before = repository.get('session_a')
    const bBefore = repository.get('session_b')
    const observed: Array<{ view: ReturnType<typeof repository.get>; state: ReturnType<typeof repository.historyState> }> = []
    let bNotifications = 0
    const stopA = repository.observe('session_a', () => observed.push({ view: repository.get('session_a'), state: repository.historyState('session_a') }))
    const stopB = repository.observe('session_b', () => { bNotifications += 1 })
    const pending = repository.readHistory('session_a', { cursor: 20, direction: 'before', limit: 20 })
    expect(observed[0].view).not.toBe(before)
    expect(observed[0].state).toMatchObject({ loading: true, version: 1 })
    expect(observed[0].view.historyState).toMatchObject({ loading: true, version: 1 })
    expect(repository.get('session_b')).toBe(bBefore)
    expect(bNotifications).toBe(0)
    resolvePage!({
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: 'old' }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
      descriptor: { limit: 20, oldest_item_seq: '10', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
    })
    await pending
    expect(observed.at(-1)?.view).not.toBe(observed[0].view)
    expect(observed.at(-1)?.state).toMatchObject({ loading: false, version: 2 })
    expect(observed.at(-1)?.view.history.items.map((item) => item.seq)).toEqual([10, 20])
    stopA(); stopB()
  })

  it('uses safe typed timeout and cancellation observation errors and cleans up', async () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    seed(replica, 'session_observation')
    await expect(repository.waitFor('session_observation', (view) => view.session?.full_access === true, { timeoutMS: 1 })).rejects.toMatchObject({ name: 'SessionContentObservationError', code: 'timeout' })
    const controller = new AbortController()
    const cancelled = repository.waitFor('session_observation', () => false, { signal: controller.signal, timeoutMS: 500 })
    controller.abort()
    await expect(cancelled).rejects.toBeInstanceOf(SessionContentObservationError)
    expect(() => repository.waitFor('session_observation', () => false, { timeoutMS: 0 })).toThrow(/timeout must be positive/)
  })

  it('waits for content authority instead of treating a command acknowledgement as a local write', async () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    const current = snapshot('session_authority')
    seed(replica, 'session_authority')
    const pending = repository.waitFor('session_authority', (view) => view.session?.full_access === true, { timeoutMS: 500 })
    // A successful command result is deliberately not represented here. The
    // predicate remains false until the resource publishes metadata.replace.
    expect(repository.get('session_authority').session?.full_access).toBe(false)
    const next = { ...current, session: { ...current.session, full_access: true, updated_at: '2025-01-01T00:00:01Z' } }
    replica.applyChange({ type: 'session_content', id: 'session_authority' }, new SessionContentAdapter('session_authority'), [
      { op: 'metadata.replace', metadata: next.session },
    ], { streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1 })
    await expect(pending).resolves.toMatchObject({ session: { full_access: true } })
  })

  it('keeps full access, debug, and compaction behind separate content authority barriers', async () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionContentRepository(replica)
    const repository = new SessionContentRepository(syncRepository)
    const resource = { type: 'session_content' as const, id: 'session_authority' }
    const adapter = new SessionContentAdapter('session_authority')
    seed(replica, 'session_authority')
    const current = snapshot('session_authority')

    const fullAccess = repository.waitFor('session_authority', (view) => view.session?.full_access === true, { timeoutMS: 500 })
    expect(repository.get('session_authority').session?.full_access).toBe(false)
    replica.applyChange(resource, adapter, [{ op: 'metadata.replace', metadata: { ...current.session, full_access: true } }], {
      streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1,
    })
    await expect(fullAccess).resolves.toMatchObject({ session: { full_access: true } })

    const debug = repository.waitFor('session_authority', (view) => view.session?.debug.request_bodies === true, { timeoutMS: 500 })
    replica.applyChange(resource, adapter, [{ op: 'metadata.replace', metadata: { ...current.session, full_access: true, debug: { request_bodies: true } } }], {
      streamEpoch: 'stream_1', sequence: '3' as Sequence, resourceRevision: '3', generation: 1,
    })
    await expect(debug).resolves.toMatchObject({ session: { debug: { request_bodies: true } } })

    const compaction = repository.waitFor('session_authority', (view) => view.compaction.checkpoints.some((checkpoint) => checkpoint.id === 'compact_1'), { timeoutMS: 500 })
    replica.applyChange(resource, adapter, [{ op: 'compaction.replace', compaction: {
      checkpoints: [{ id: 'compact_1', created_at: '2025-01-01T00:00:01Z', reason: 'manual', phase: 'completed', trigger: 'manual', summary_item_id: 'summary_1', replacement_history: [] }],
      truncated: false,
    } }], {
      streamEpoch: 'stream_1', sequence: '4' as Sequence, resourceRevision: '4', generation: 1,
    })
    await expect(compaction).resolves.toMatchObject({ compaction: { checkpoints: [{ id: 'compact_1' }] } })
  })
})
