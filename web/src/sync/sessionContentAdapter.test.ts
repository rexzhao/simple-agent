import { describe, expect, it } from 'vitest'
import type { SubscriptionEventData } from '../protocol/types'
import { SessionContentAdapter } from './sessionContentAdapter'
import type { SessionContentState } from '../domain/sessionContent'

const metadata = (overrides: Record<string, unknown> = {}) => ({
  id: 'session_a', version: 2, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
  archived: false, last_used_at: '2025-01-01T00:00:00Z', has_unread_result: false, status: 'running',
  running_run_id: 'run_a', running_turn_id: 'turn_a', show_reasoning: true, full_access: false,
  debug: { request_bodies: false }, context: {}, save_tool_results: false, ...overrides,
})
const descriptor = (overrides: Record<string, unknown> = {}) => ({ limit: 50, align_turn: false, visible_only: true, has_more_before: false, has_more_after: false, ...overrides })
const item = (id = 'item_a', text = 'hello') => ({
  key: { turn_id: 'turn_a', agent_iteration: 1, item_id: id }, seq: 1, created_at: '2025-01-01T00:00:00Z',
  kind: 'message', visibility: 'visible', audience: 'user', message: { role: 'assistant', content: { inline: text } },
})
const snapshot = (overrides: Record<string, unknown> = {}) => ({
  schema_version: 1, session: metadata(), history: { items: [item()], descriptor: descriptor({ oldest_item_seq: '1', newest_item_seq: '1' }) },
  active_run: { run_id: 'run_a', session_id: 'session_a', turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch_a', run_cursor: '0', replay_available: false, recovery_required: false },
  compaction: { checkpoints: [], truncated: false }, ...overrides,
})
const context = { resource: { type: 'session_content' as const, id: 'session_a' }, resourceRevision: '1', generation: 1 }
const key = JSON.stringify(['turn_a', 1, 'item_a'])

function initial(): SessionContentState {
  return new SessionContentAdapter('session_a').decodeSnapshot(snapshot(), undefined, context)
}

function event(cursor: string, type: string, fields: Record<string, unknown> = {}): SubscriptionEventData {
  return { type, session_id: 'session_a', run_id: 'run_a', run_cursor: cursor, ...fields } as unknown as SubscriptionEventData
}

describe('SessionContentAdapter', () => {
  it('strictly validates durable snapshot identity and operation unions', () => {
    const adapter = new SessionContentAdapter('session_a')
    expect(() => adapter.decodeSnapshot(snapshot({ session: metadata({ id: 'session_b' }) }), undefined)).toThrow()
    expect(() => adapter.decodeSnapshot(snapshot({ future: true }), undefined)).toThrow()
    expect(() => adapter.applyChange(initial(), [{ op: 'future.op' }])).toThrow()
  })

  it('reduces assistant lifecycle snapshots by identity and monotonic message revision', () => {
    const adapter = new SessionContentAdapter('session_a')
    let state = adapter.applyTransient(initial(), event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'assistant.message.started', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '0' }), context)
    state = adapter.applyTransient(state, event('3', 'assistant.message.updated', {
      turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '1', content: 'hello world', reasoning: 'thinking', tool_calls: [],
    }), context)
    expect(state.transientRun?.messages[key]).toMatchObject({ revision: '1', status: 'streaming', message: { role: 'assistant', content: { inline: 'hello world' }, reasoning: { inline: 'thinking' } } })

    // A replayed older revision cannot replace the latest full snapshot.
    state = adapter.applyTransient(state, event('4', 'assistant.message.updated', {
      turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '1', content: 'stale', tool_calls: [],
    }), context)
    expect(state.transientRun?.messages[key].message.content?.inline).toBe('hello world')
    state = adapter.applyTransient(state, event('5', 'assistant.message.completed', {
      turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '2', content: 'hello world', reasoning: 'thinking', tool_calls: [],
    }), context)
    expect(state.transientRun?.messages[key].status).toBe('complete')
  })

  it('closes failed messages and accepts an omitted terminal snapshot', () => {
    const adapter = new SessionContentAdapter('session_a')
    let state = adapter.applyTransient(initial(), event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'assistant.message.started', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '0' }), context)
    state = adapter.applyTransient(state, event('3', 'assistant.message.updated', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '1', content: 'partial', tool_calls: [] }), context)
    state = adapter.applyTransient(state, event('4', 'assistant.message.failed', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '2', snapshot_omitted: true }), context)
    expect(state.transientRun?.messages[key]).toMatchObject({ revision: '2', status: 'incomplete', message: { content: { inline: 'partial' } } })
  })

  it('marks an omitted-only terminal lifecycle as having no message snapshot', () => {
    const adapter = new SessionContentAdapter('session_a')
    let state = adapter.applyTransient(initial(), event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'assistant.message.started', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '0' }), context)
    state = adapter.applyTransient(state, event('3', 'assistant.message.completed', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '1', snapshot_omitted: true }), context)
    expect(state.transientRun?.messages[key]).toMatchObject({ revision: '1', status: 'complete', snapshotAvailable: false })
  })

  it('rejects cursor gaps, malformed lifecycle shapes, and cross-run events', () => {
    const adapter = new SessionContentAdapter('session_a')
    const state = adapter.applyTransient(initial(), event('1', 'run.started', { status: 'running' }), context)
    expect(() => adapter.applyTransient(state, event('3', 'assistant.message.started', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '0' }), context)).toThrow()
    expect(() => adapter.applyTransient(state, event('2', 'assistant.message.updated', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '1' }), context)).toThrow()
    expect(() => adapter.applyTransient(state, event('2', 'assistant.message.started', { run_id: 'run_b', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', message_revision: '0' }), context)).toThrow()
  })

  it('keeps tools and prompt queue as independent run lifecycle state', () => {
    const adapter = new SessionContentAdapter('session_a')
    let state = adapter.applyTransient(initial(), event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'tool.requested', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'call_a', name: 'shell', arguments: '{}' }), context)
    state = adapter.applyTransient(state, event('3', 'tool.progress', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'call_a', name: 'shell', arguments_delta: 'x' }), context)
    state = adapter.applyTransient(state, event('4', 'tool.finished', { turn_id: 'turn_a', agent_iteration: 1, tool_call_id: 'call_a', name: 'shell', is_error: false, content: 'ok' }), context)
    state = adapter.applyTransient(state, event('5', 'run.prompt_queue', { prompts: [{ id: 'p1', content: 'next', steer: true }] }), context)
    expect(state.transientRun?.tools.call_a).toMatchObject({ status: 'finished', arguments: '{}x', content: 'ok' })
    expect(state.transientRun?.promptQueue).toEqual([{ id: 'p1', content: 'next', steer: true }])
  })

  it('retains a terminal overlay until verified durable settlement covers its item', () => {
    const adapter = new SessionContentAdapter('session_a')
    let state = adapter.applyTransient(initial(), event('1', 'run.started', { status: 'running' }), context)
    state = adapter.applyTransient(state, event('2', 'assistant.message.started', { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_b', message_revision: '0' }), context)
    state = adapter.applyTransient(state, event('3', 'run.settled', { status: 'committed', durable_settlement_watermark: {
      resource_revision: '2', run_cursor: '2', verified: true, covered_items: [{ turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_b', run_cursor: '2' }],
    } }), context)
    expect(state.transientRun).not.toBeNull()
    state = adapter.applyChange(state, [{ op: 'item.upsert', item: { ...item('item_b', 'durable'), seq: 2 } }, { op: 'history.window.descriptor.replace', descriptor: descriptor({ oldest_item_seq: '1', newest_item_seq: '2' }) }], { ...context, resourceRevision: '2' })
    expect(state.transientRun).toBeNull()
  })
})
