import { describe, expect, it, vi } from 'vitest'
import type { ChangeOperation, JsonObject, Sequence, SubscriptionEventData } from '../protocol/types'
import { LocalReplica } from './localReplica'
import { SessionContentAdapter } from './sessionContentAdapter'
import type { SessionContentState } from '../domain/sessionContent'

const metadata = (overrides: Record<string, unknown> = {}) => ({
  id: 'session_a', version: 2, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
  archived: false, last_used_at: '2025-01-01T00:00:00Z', has_unread_result: false, status: 'idle',
  show_reasoning: false, full_access: false, debug: { request_bodies: false }, context: {}, save_tool_results: false,
  ...overrides,
})
const descriptor = (overrides: Record<string, unknown> = {}) => ({
  limit: 50, align_turn: false, visible_only: true, has_more_before: false, has_more_after: false, ...overrides,
})
const item = (id = 'item_a', text = 'hello', overrides: Record<string, unknown> = {}) => ({
  key: { turn_id: 'turn_a', agent_iteration: 1, item_id: id }, seq: 1, created_at: '2025-01-01T00:00:00Z',
  kind: 'message', visibility: 'visible', audience: 'user',
  message: { role: 'assistant', content: { inline: text, content_type: 'text/plain' } }, ...overrides,
})
const snapshot = (overrides: Record<string, unknown> = {}) => ({
  schema_version: 1, session: metadata(), history: { items: [item()], descriptor: descriptor({ oldest_item_seq: '1', newest_item_seq: '1' }) }, active_run: null,
  compaction: { checkpoints: [], truncated: false }, ...overrides,
})
const applyMetadata = { streamEpoch: 'stream_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 }

function stateWithRunning(): SessionContentState {
  const adapter = new SessionContentAdapter('session_a')
  return adapter.decodeSnapshot(snapshot({
    session: metadata({ status: 'running', running_run_id: 'run_a', running_turn_id: 'turn_a' }),
    active_run: { run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch_a', run_cursor: '0', replay_available: false, recovery_required: false },
  }), undefined, { resource: { type: 'session_content', id: 'session_a' }, resourceRevision: '1', generation: 1 })
}

describe('SessionContentAdapter', () => {
  it('strictly applies a snapshot and every durable operation without partial state', () => {
    const resource = { type: 'session_content' as const, id: 'session_a' }
    const adapter = new SessionContentAdapter('session_a')
    const replica = new LocalReplica()
    replica.applySnapshot(resource, adapter, snapshot() as unknown as JsonObject, applyMetadata)
    const first = replica.get<SessionContentState>(resource).value!
    const changed = [
      { op: 'metadata.replace', metadata: metadata({ display_name: 'Renamed' }) },
      { op: 'item.upsert', item: item('item_b', 'world', { key: { turn_id: 'turn_b', agent_iteration: 1, item_id: 'item_b' }, seq: 2 }) },
      { op: 'item.remove', key: { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a' } },
      { op: 'history.window.replace', window: { items: [item('item_b', 'world', { key: { turn_id: 'turn_b', agent_iteration: 1, item_id: 'item_b' }, seq: 2 })], descriptor: descriptor({ oldest_item_seq: '2', newest_item_seq: '2' }) } },
      { op: 'history.window.descriptor.replace', descriptor: descriptor({ oldest_item_seq: '2', newest_item_seq: '2' }) },
      { op: 'compaction.replace', compaction: { checkpoints: [{ id: 'checkpoint_a', created_at: '2025-01-01T00:00:00Z', reason: 'test', phase: 'done', trigger: 'manual', summary_item_id: 'item_b', replacement_history: ['item_b'] }], truncated: false } },
    ]
    replica.applyChange(resource, adapter, changed as unknown as ChangeOperation[], { streamEpoch: 'stream_1', sequence: '2' as Sequence, resourceRevision: '2', generation: 1 })
    const value = replica.get<SessionContentState>(resource).value!
    expect(value.snapshot.session.display_name).toBe('Renamed')
    expect(value.snapshot.history.items.map((entry) => entry.key.item_id)).toEqual(['item_b'])
    expect(value.snapshot.compaction.checkpoints[0].id).toBe('checkpoint_a')
    expect(() => adapter.applyChange(first, [{ op: 'future.op', value: {} }])).toThrow()
    expect(replica.get<SessionContentState>(resource).value?.snapshot.session.display_name).toBe('Renamed')

    replica.applyChange(resource, adapter, [
      { op: 'metadata.replace', metadata: metadata({ status: 'running', running_run_id: 'run_a', running_turn_id: 'turn_a' }) },
      { op: 'active_run.replace', active_run: { run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true } },
    ] as unknown as ChangeOperation[], { streamEpoch: 'stream_1', sequence: '3' as Sequence, resourceRevision: '3', generation: 1 })
    expect(replica.get<SessionContentState>(resource).value?.snapshot.active_run?.run_id).toBe('run_a')
    replica.applyChange(resource, adapter, [
      { op: 'active_run.clear' },
      { op: 'metadata.replace', metadata: metadata({ status: 'idle' }) },
    ] as unknown as ChangeOperation[], { streamEpoch: 'stream_1', sequence: '4' as Sequence, resourceRevision: '4', generation: 1 })
    expect(replica.get<SessionContentState>(resource).value?.snapshot.active_run).toBeNull()
  })

  it('rejects identity, shape, revision, and cross-field errors', () => {
    const adapter = new SessionContentAdapter('session_a')
    expect(() => adapter.decodeSnapshot(snapshot({ session: metadata({ id: 'session_b' }) }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ future: true }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ history: { items: [item()], descriptor: descriptor({ oldest_item_seq: '2', newest_item_seq: '2' }) } }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ active_run: { run_id: 'run_a' } }), undefined)).toThrow()
    expect(() => adapter.applyChange(stateWithRunning(), [{ op: 'item.upsert', item: item('bad', 'x', { key: { turn_id: 'turn_a', agent_iteration: 1, item_id: 'bad' }, seq: 2 }) }, { op: 'future.op' }])).toThrow()
    expect(() => adapter.validateResourceRevision('')).toThrow()
    const blob = { id: 'blob_a', url: 'https://example.test/blob', content_type: 'application/octet-stream', size: 1, sha256: 'not-a-digest-but-a-required-string', etag: 'opaque-etag', expires_at: '2025-01-01T00:00:00Z' }
    expect(() => adapter.decodeSnapshot(snapshot({ history: { items: [item('blob', '', { message: { role: 'assistant', content: { blob } } })], descriptor: descriptor({ oldest_item_seq: '1', newest_item_seq: '1' }) } }), undefined)).not.toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ history: { items: [item('blob', '', { message: { role: 'assistant', content: { blob: { ...blob, size: Number.MAX_SAFE_INTEGER + 1 } } } })], descriptor: descriptor({ oldest_item_seq: '1', newest_item_seq: '1' }) } }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ session: metadata({ status: 'running', running_run_id: 'run_a', running_turn_id: 'turn_a' }), active_run: {
      run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true,
      run_epoch: 'epoch_a', run_cursor: '10', replay_available: true, replay_from_cursor: '8', replay_to_cursor: '11',
    } }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ session: metadata({ status: 'running', running_run_id: 'run_a', running_turn_id: 'turn_a' }), active_run: {
      run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true,
      run_epoch: 'epoch_a', run_cursor: '10', replay_available: true, replay_from_cursor: '9', replay_to_cursor: '8',
    } }), undefined)).toThrow()
    // The Go schema validates optional range fields individually when replay
    // is false; do not reject a forward-compatible descriptor for that alone.
    expect(() => adapter.decodeSnapshot(snapshot({ session: metadata({ status: 'running', running_run_id: 'run_a', running_turn_id: 'turn_a' }), active_run: {
      run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true,
      run_epoch: 'epoch_a', run_cursor: '10', replay_available: false, replay_from_cursor: '9', replay_to_cursor: '8',
    } }), undefined)).not.toThrow()
  })

  it('keeps cursor identity isolated and handles duplicate/gap/terminal rules', () => {
    const adapter = new SessionContentAdapter('session_a')
    const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
    let state = stateWithRunning()
    const event = (cursor: string, type: string, fields: Record<string, unknown> = {}) => ({ type, session_id: 'session_a', run_id: 'run_a', run_cursor: cursor, turn_id: 'turn_a', agent_iteration: 1, ...fields }) as unknown as SubscriptionEventData
    state = adapter.applyTransient(state, event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('1', 'run.started', { status: 'running' }), context)
    expect(() => adapter.applyTransient(state, event('1', 'text.delta', { delta: 'malformed duplicate' }), context)).toThrow()
    expect(() => adapter.applyTransient(state, event('1', 'run.started', { status: 'finished' }), context)).toThrow()
    expect(state.transientRun?.runCursor).toBe('1')
    expect(() => adapter.applyTransient(state, event('3', 'text.delta', { item_id: 'item_a', delta: 'gap' }), context)).toThrow()
    expect(() => adapter.applyTransient(state, event('2', 'text.delta', { item_id: 'item_a', delta: 'tail' }), { ...context, resourceRevision: '2' })).not.toThrow()
    expect(() => adapter.applyTransient(state, event('2', 'text.delta', { item_id: 'item_a', delta: 'other' }), context)).not.toThrow()
    expect(() => adapter.applyTransient(state, event('2', 'text.delta', { item_id: 'item_a', delta: 'other', run_id: 'run_b' }), context)).toThrow()
    expect(() => adapter.applyChange(state, [{ op: 'active_run.replace', active_run: { run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch_b' } }], context)).toThrow()
  })

  it('reduces text, reasoning, tools, prompts, and verified settlement conservatively', () => {
    const adapter = new SessionContentAdapter('session_a')
    const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
    let state = stateWithRunning()
    const event = (cursor: string, type: string, fields: Record<string, unknown> = {}) => ({ type, session_id: 'session_a', run_id: 'run_a', run_cursor: cursor, ...fields }) as unknown as SubscriptionEventData
    state = adapter.applyTransient(state, event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'text.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'hello', durable_text_length: 0 }), context)
    state = adapter.applyTransient(state, event('3', 'reasoning.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'thinking' }), context)
    state = adapter.applyTransient(state, event('4', 'tool.requested', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'tool_a', name: 'shell', arguments: '{}' }), context)
    state = adapter.applyTransient(state, event('5', 'tool.progress', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'tool_a', name: 'shell', arguments_delta: 'x' }), context)
    state = adapter.applyTransient(state, event('6', 'reasoning.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_b', delta: 'next thought' }), context)
    state = adapter.applyTransient(state, event('7', 'tool.finished', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'tool_a', name: 'shell', is_error: false, content: 'ok' }), context)
    state = adapter.applyTransient(state, event('8', 'run.prompt_queue', { prompts: [{ id: 'p1', content: 'next', steer: false }] }), context)
    state = adapter.applyTransient(state, event('9', 'run.prompt_appended', { prompts: ['later'] }), context)
    expect(state.transientRun?.text[JSON.stringify(['turn_a', 1, 'item_a'])].text).toBe('hello')
    expect(state.transientRun?.tools.tool_a.status).toBe('finished')
    expect(state.transientRun?.promptQueue[0].id).toBe('p1')
    state = adapter.applyTransient(state, event('10', 'turn.failed', { turn_id: 'turn_a', code: 'model_http_error', message: '429: slow down and try again' }), context)
    expect(state.turnFailure).toEqual({ turnID: 'turn_a', code: 'model_http_error', message: '429: slow down and try again' })
    state = adapter.applyTransient(state, event('11', 'run.settled', { status: 'committed', durable_settlement_watermark: { resource_revision: '10', run_cursor: '10', verified: false, covered_items: [] } }), { ...context, resourceRevision: '8' })
    expect(state.transientRun?.status).toBe('committed')
    expect(state.transientRun?.recoveryRequired).toBe(true)
    expect(state.transientRun?.stepOrder).toEqual([
      { kind: 'reasoning', key: JSON.stringify(['turn_a', 1, 'item_a']) },
      { kind: 'tool', key: 'tool_a' },
      { kind: 'reasoning', key: JSON.stringify(['turn_a', 1, 'item_b']) },
    ])
    state = adapter.applyChange(state, [{ op: 'item.upsert', item: item('item_a', 'hello') }], { ...context, resourceRevision: '9' })
    expect(state.transientRun).not.toBeNull()
    expect(() => adapter.applyTransient(state, event('12', 'run.started', { status: 'running', run_id: 'run_a' }), { ...context, resourceRevision: '9' })).toThrow()
  })

  it('counts turn failure message length by Unicode code points', () => {
    const adapter = new SessionContentAdapter('session_a')
    const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
    const event = (cursor: string, message: string) => ({
      type: 'turn.failed', session_id: 'session_a', run_id: 'run_a', run_cursor: cursor,
      turn_id: 'turn_a', code: 'provider_error', message,
    }) as unknown as SubscriptionEventData

    const state = stateWithRunning()
    expect(() => adapter.applyTransient(state, event('1', '🦄'.repeat(600)), context)).not.toThrow()
    expect(() => adapter.applyTransient(state, event('1', '🦄'.repeat(601)), context)).toThrow('turn.failed.message is too long')
  })

  it('records reasoning event timing without resetting on later deltas', () => {
    vi.useFakeTimers()
    try {
      const adapter = new SessionContentAdapter('session_a')
      const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
      const event = (cursor: string, type: string, fields: Record<string, unknown> = {}) => ({ type, session_id: 'session_a', run_id: 'run_a', run_cursor: cursor, ...fields }) as unknown as SubscriptionEventData
      const apply = (state: SessionContentState, cursor: string, type: string, fields: Record<string, unknown> = {}): SessionContentState => {
        return adapter.applyTransient(state, event(cursor, type, fields), { ...context, eventTimestamp: new Date().toISOString() })
      }
      vi.setSystemTime(new Date('2025-01-01T00:00:00.000Z'))
      let state = apply(stateWithRunning(), '1', 'run.started', { status: 'running' })
      vi.advanceTimersByTime(1000)
      state = apply(state, '2', 'reasoning.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'reasoning_a', delta: 'first' })
      vi.advanceTimersByTime(2000)
      state = apply(state, '3', 'reasoning.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'reasoning_a', delta: ' second' })
      expect(state.transientRun?.reasoningTimings?.[JSON.stringify(['turn_a', 1, 'reasoning_a'])]).toEqual({ startedAt: '2025-01-01T00:00:01.000Z' })
      vi.advanceTimersByTime(1500)
      state = apply(state, '4', 'tool.requested', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'tool_a', name: 'shell' })
      expect(state.transientRun?.reasoningTimings?.[JSON.stringify(['turn_a', 1, 'reasoning_a'])]).toEqual({
        startedAt: '2025-01-01T00:00:01.000Z', endedAt: '2025-01-01T00:00:04.500Z',
      })
      vi.advanceTimersByTime(500)
      state = apply(state, '5', 'reasoning.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'reasoning_b', delta: 'next' })
      vi.advanceTimersByTime(1000)
      state = apply(state, '6', 'run.settled', {
        status: 'committed',
        durable_settlement_watermark: { resource_revision: '1', run_cursor: '6', verified: false, covered_items: [] },
      })
      expect(state.transientRun?.reasoningTimings?.[JSON.stringify(['turn_a', 1, 'reasoning_b'])]).toEqual({
        startedAt: '2025-01-01T00:00:05.000Z', endedAt: '2025-01-01T00:00:06.000Z',
      })
    } finally {
      vi.useRealTimers()
    }
  })

  it('closes an earlier reasoning identity when another starts without an intervening tool or output', () => {
    const adapter = new SessionContentAdapter('session_a')
    const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
    const event = (cursor: string, itemID: string, delta: string) => ({
      type: 'reasoning.delta', session_id: 'session_a', run_id: 'run_a', run_cursor: cursor,
      turn_id: 'turn_a', agent_iteration: 1, item_id: itemID, delta,
    }) as unknown as SubscriptionEventData
    let state = adapter.applyTransient(stateWithRunning(), { type: 'run.started', session_id: 'session_a', run_id: 'run_a', run_cursor: '1', status: 'running' } as unknown as SubscriptionEventData, context)
    state = adapter.applyTransient(state, event('2', 'reasoning_a', 'first'), { ...context, eventTimestamp: '2025-01-01T00:00:01.000Z' })
    state = adapter.applyTransient(state, event('3', 'reasoning_b', 'second'), { ...context, eventTimestamp: '2025-01-01T00:00:02.000Z' })
    state = adapter.applyTransient(state, event('4', 'reasoning_b', ' delta'), { ...context, eventTimestamp: '2025-01-01T00:00:03.000Z' })
    expect(state.transientRun?.reasoningTimings?.[JSON.stringify(['turn_a', 1, 'reasoning_a'])]).toEqual({
      startedAt: '2025-01-01T00:00:01.000Z', endedAt: '2025-01-01T00:00:02.000Z',
    })
    expect(state.transientRun?.reasoningTimings?.[JSON.stringify(['turn_a', 1, 'reasoning_b'])]).toEqual({
      startedAt: '2025-01-01T00:00:02.000Z',
    })
  })

  it('only clears a verified settlement after both revision and covered item identity are durable', () => {
    const adapter = new SessionContentAdapter('session_a')
    const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
    let state = stateWithRunning()
    const event = (cursor: string, type: string, fields: Record<string, unknown> = {}) => ({ type, session_id: 'session_a', run_id: 'run_a', run_cursor: cursor, ...fields }) as unknown as SubscriptionEventData
    state = adapter.applyTransient(state, event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'text.delta', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'tail', durable_text_length: 5, durable_checkpointed: false }), context)
    state = adapter.applyTransient(state, event('3', 'run.settled', {
      status: 'committed', durable_settlement_watermark: {
        resource_revision: '2', run_cursor: '2', verified: true,
        covered_items: [{ turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_b', run_cursor: '2' }],
      },
    }), context)
    expect(state.transientRun).not.toBeNull()
    state = adapter.applyChange(state, [
      { op: 'item.upsert', item: item('item_b', 'durable', { key: { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_b' }, seq: 2 }) },
      { op: 'history.window.descriptor.replace', descriptor: descriptor({ oldest_item_seq: '1', newest_item_seq: '2' }) },
    ], { ...context, resourceRevision: '2' })
    expect(state.transientRun).toBeNull()
  })
})
