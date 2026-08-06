import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { BlobClient } from './blobClient'
import { LocalReplica } from './localReplica'
import { SyncRuntime, type RuntimeTransport } from './runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'
import type { SessionContentState } from '../domain/sessionContent'
import { SessionContentRepository } from './sessionContentRepository'

class FakeTransport implements RuntimeTransport {
  isReady = true
  connectionGeneration = 1
  serverEpoch: string | undefined = 'server_1'
  sent: ProtocolMessage[] = []
  private messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private ready = new Set<(event: TransportReadyEvent) => void>()
  private closed = new Set<(event: TransportCloseEvent) => void>()
  start(): void { this.isReady = true }
  stop(): void { this.isReady = false }
  send(message: ProtocolMessage): void { this.sent.push(message) }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.closed.add(listener); return () => this.closed.delete(listener) }
  emit(value: unknown, generation = this.connectionGeneration): void {
    const message = typeof value === 'string' ? decodeMessage(value) : value as ProtocolMessage
    for (const listener of [...this.messages]) listener(message, generation)
  }
  emitReady(epoch = this.serverEpoch ?? 'server_1'): void {
    this.serverEpoch = epoch
    for (const listener of [...this.ready]) listener({ generation: this.connectionGeneration, serverEpoch: epoch, connectionID: `connection_${this.connectionGeneration}`, heartbeatIntervalMS: 1000, maxMessageBytes: 1024 * 1024 })
  }
  emitClose(): void {
    this.isReady = false
    for (const listener of [...this.closed]) listener({ generation: this.connectionGeneration, willRetry: true })
  }
  lastSubscribe(): Extract<ProtocolMessage, { type: 'subscribe' }> {
    const message = [...this.sent].reverse().find((candidate) => candidate.type === 'subscribe')
    if (!message || message.type !== 'subscribe') throw new Error('missing subscribe')
    return message
  }
}

const protocol = (value: unknown): ProtocolMessage => decodeMessage(JSON.stringify(value))

const metadata = (id: string, overrides: Record<string, unknown> = {}) => ({
  id, version: 2, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
  archived: false, last_used_at: '2025-01-01T00:00:00Z', has_unread_result: false, status: 'running',
  running_run_id: 'run_a', running_turn_id: 'turn_a', show_reasoning: true, full_access: false,
  debug: { request_bodies: false }, context: {}, save_tool_results: false, ...overrides,
})

const item = (content: string) => ({
  key: { turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a' }, seq: 1,
  created_at: '2025-01-01T00:00:00Z', kind: 'message', visibility: 'visible', audience: 'user',
  message: { role: 'assistant', content: { inline: content, content_type: 'text/plain' } },
})

const content = (id: string, text = 'base', activeOverrides: Record<string, unknown> = {}) => ({
  schema_version: 1, session: metadata(id),
  history: { items: [item(text)], descriptor: { limit: 20, align_turn: false, visible_only: true, oldest_item_seq: '1', newest_item_seq: '1', has_more_before: false, has_more_after: false } },
  active_run: { run_id: 'run_a', session_id: id, turn_id: 'turn_a', started_at: '2025-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch_a', run_cursor: '0', replay_available: false, recovery_required: false, ...activeOverrides },
  compaction: { checkpoints: [], truncated: false },
})

function subscribed(id: string, sessionID: string, sequence = '1'): ProtocolMessage {
  return protocol({ version: 1, type: 'subscribed', id: `subscribed_${id}`, payload: { subscription_id: id, resource: { type: 'session_content', id: sessionID }, stream_epoch: 'stream_1', sequence } })
}

function snapshot(id: string, sessionID: string, value: unknown, sequence = '1', revision = '1'): ProtocolMessage {
  return protocol({ version: 1, type: 'snapshot', id: `snapshot_${id}`, payload: { subscription_id: id, resource: { type: 'session_content', id: sessionID }, stream_epoch: 'stream_1', sequence, resource_revision: revision, content: { inline: value } } })
}

function event(id: string, sessionID: string, runCursor: string, fields: Record<string, unknown>): ProtocolMessage {
  return protocol({ version: 1, type: 'subscription_event', id: `event_${runCursor}`, payload: { subscription_id: id, resource: { type: 'session_content', id: sessionID }, event: { session_id: sessionID, run_id: 'run_a', run_cursor: runCursor, ...fields } } })
}

function change(id: string, sessionID: string, value: unknown, previous = '1', sequence = '2', revision = '2'): ProtocolMessage {
  return protocol({ version: 1, type: 'change', id: `change_${sequence}`, payload: { subscription_id: id, resource: { type: 'session_content', id: sessionID }, stream_epoch: 'stream_1', sequence, previous_sequence: previous, resource_revision: revision, operations: [{ op: 'item.upsert', item: value }] } })
}

async function settleSnapshot(): Promise<void> {
  await Promise.resolve()
  await Promise.resolve()
}

describe('session-content transient runtime path', () => {
  it('routes transient events without ACK, merges keyed text, and clears the tail when durable content covers it', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.start()
    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, 'session_a'))
    transport.emit(snapshot(first.payload.subscription_id, 'session_a', content('session_a')))
    await settleSnapshot()

    const ackCount = transport.sent.filter((message) => message.type === 'ack').length
    transport.emit(event(first.payload.subscription_id, 'session_a', '1', { type: 'run.started', status: 'running' }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '2', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'tail', durable_text_length: 4, durable_checkpointed: false }))
    const stateAfterText = runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value!
    expect(stateAfterText.transientRun?.text[JSON.stringify(['turn_a', 1, 'item_a'])].text).toBe('tail')
    expect(transport.sent.filter((message) => message.type === 'ack')).toHaveLength(ackCount)

    const repository = new SessionContentRepository(runtime.replica)
    expect(repository.get('session_a').history.items[0].message?.content?.inline).toBe('basetail')
    transport.emit(event(first.payload.subscription_id, 'session_a', '3', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: '!', durable_text_length: 9, durable_checkpointed: true }))
    expect(repository.get('session_a').history.items[0].message?.content?.inline).toBe('basetail!')

    transport.emit(change(first.payload.subscription_id, 'session_a', item('basetail!')))
    const afterDurable = runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value!
    expect(afterDurable.transientRun?.text[JSON.stringify(['turn_a', 1, 'item_a'])]).toBeUndefined()
    expect(repository.get('session_a').history.items[0].message?.content?.inline).toBe('basetail!')
  })

  it('keeps the run cursor resource-local, ignores old subscriptions, and resubscribes with an independent active-run resume', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.subscribe({ type: 'session_content', id: 'session_b' }, { retainOnRelease: true })
    runtime.start()
    const a = [...transport.sent].filter((message) => message.type === 'subscribe' && message.payload.resource.id === 'session_a').at(-1)
    const b = [...transport.sent].filter((message) => message.type === 'subscribe' && message.payload.resource.id === 'session_b').at(-1)
    if (!a || a.type !== 'subscribe' || !b || b.type !== 'subscribe') throw new Error('missing initial subscribes')
    transport.emit(subscribed(a.payload.subscription_id, 'session_a'))
    transport.emit(snapshot(a.payload.subscription_id, 'session_a', content('session_a')))
    transport.emit(subscribed(b.payload.subscription_id, 'session_b'))
    transport.emit(snapshot(b.payload.subscription_id, 'session_b', content('session_b')))
    await settleSnapshot()
    transport.emit(event(a.payload.subscription_id, 'session_a', '1', { type: 'run.started', status: 'running' }))
    transport.emit(event(a.payload.subscription_id, 'session_a', '2', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'A', durable_text_length: 4, durable_checkpointed: false }))
    expect(runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_b' }).value?.transientRun).toBeNull()

    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_1')
    const resumed = [...transport.sent].reverse().find((message) => message.type === 'subscribe' && message.payload.resource.id === 'session_a')
    if (!resumed || resumed.type !== 'subscribe') throw new Error('missing resumed session A subscribe')
    expect(resumed.payload.resume).toEqual({ stream_epoch: 'stream_1', sequence: '1' })
    expect(resumed.payload.active_run_resume).toEqual({ run_epoch: 'epoch_a', run_id: 'run_a', run_cursor: '2' })

    transport.emit(event(a.payload.subscription_id, 'session_a', '3', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'old', durable_text_length: 0, durable_checkpointed: false }), 1)
    expect(runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value?.transientRun?.runCursor).toBe('2')
  })

  it('resyncs on a transient gap, drops only the overlay, and queues a Blob snapshot event atomically', async () => {
    const transport = new FakeTransport()
    let resolveBlob: ((value: unknown) => void) | undefined
    const blobClient = { getJSON: () => new Promise<unknown>((resolve) => { resolveBlob = resolve }) } as unknown as BlobClient
    const runtime = new SyncRuntime({ transport, blobClient })
    runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.start()
    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, 'session_a'))
    transport.emit(protocol({ version: 1, type: 'snapshot', id: 'blob_snapshot', payload: { subscription_id: first.payload.subscription_id, resource: { type: 'session_content', id: 'session_a' }, stream_epoch: 'stream_1', sequence: '1', resource_revision: '1', content: { blob: { id: 'blob_1', url: '/blob/1', content_type: 'application/json', size: 1, sha256: 'a'.repeat(64), etag: 'a', expires_at: '2099-01-01T00:00:00Z' } } } }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '1', { type: 'run.started', status: 'running' }))
    // Event is queued while the immutable Blob snapshot is pending.
    resolveBlob?.(content('session_a'))
    await settleSnapshot()
    expect(runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value?.transientRun?.runCursor).toBe('1')

    const observations: Array<{ readState: string | undefined; hasOverlay: boolean }> = []
    const unsubscribe = runtime.replica.subscribe((resource) => {
      if (resource.id !== 'session_a') return
      const record = runtime.replica.get<SessionContentState>(resource)
      observations.push({ readState: record.metadata.readState, hasOverlay: record.value?.transientRun !== null && record.value?.transientRun !== undefined })
    })
    transport.emit(event(first.payload.subscription_id, 'session_a', '3', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'gap', durable_text_length: 0, durable_checkpointed: false }))
    unsubscribe()
    const fresh = transport.lastSubscribe()
    expect(fresh.payload.resume).toBeUndefined()
    expect(fresh.payload.active_run_resume).toBeUndefined()
    expect(runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value?.transientRun).toBeNull()
    expect(observations).toHaveLength(1)
    expect(observations[0]).toEqual({ readState: 'stale', hasOverlay: false })
  })

  it('replays a fresh run from the retained-window predecessor, not the snapshot latest cursor', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.start()
    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, 'session_a'))
    transport.emit(snapshot(first.payload.subscription_id, 'session_a', content('session_a', 'base', {
      run_cursor: '5', replay_available: true, replay_from_cursor: '1', replay_to_cursor: '5',
    })))
    await settleSnapshot()
    // The snapshot advertises the server's latest cursor (5), but the client
    // has applied none of the replay yet. Cursor 1 must therefore be accepted.
    transport.emit(event(first.payload.subscription_id, 'session_a', '1', { type: 'run.started', status: 'running' }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '2', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'replayed', durable_text_length: 4, durable_checkpointed: false }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '3', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: '!', durable_text_length: 4, durable_checkpointed: false }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '4', { type: 'run.prompt_appended', prompts: ['prompt'] }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '5', { type: 'run.prompt_queue', prompts: [] }))
    const state = runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value!
    expect(state.transientRun?.runCursor).toBe('5')
    expect(state.transientRun?.text[JSON.stringify(['turn_a', 1, 'item_a'])].text).toBe('replayed!')
    expect(state.transientRun?.appendedPrompts).toEqual(['prompt'])

    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_1')
    expect(transport.lastSubscribe().payload.active_run_resume).toEqual({ run_epoch: 'epoch_a', run_id: 'run_a', run_cursor: '5' })
  })

  it('starts a late-join replay at first_cursor minus one for a sliding window using safe decimal arithmetic', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.start()
    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, 'session_a'))
    transport.emit(snapshot(first.payload.subscription_id, 'session_a', content('session_a', 'base', {
      run_cursor: '9007199254740993', replay_available: true, replay_from_cursor: '9007199254740991', replay_to_cursor: '9007199254740993',
    })))
    await settleSnapshot()
    transport.emit(event(first.payload.subscription_id, 'session_a', '9007199254740991', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'a', durable_text_length: 4, durable_checkpointed: false }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '9007199254740992', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'b', durable_text_length: 4, durable_checkpointed: false }))
    transport.emit(event(first.payload.subscription_id, 'session_a', '9007199254740993', { type: 'text.delta', turn_id: 'turn_a', agent_iteration: 1, item_id: 'item_a', delta: 'c', durable_text_length: 4, durable_checkpointed: false }))
    const state = runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value!
    expect(state.transientRun?.runCursor).toBe('9007199254740993')
    expect(state.transientRun?.text[JSON.stringify(['turn_a', 1, 'item_a'])].text).toBe('abc')
  })

  it('atomically clears the overlay when the server epoch changes', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.start()
    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, 'session_a'))
    transport.emit(snapshot(first.payload.subscription_id, 'session_a', content('session_a')))
    await settleSnapshot()
    transport.emit(event(first.payload.subscription_id, 'session_a', '1', { type: 'run.started', status: 'running' }))
    const observations: Array<{ readState: string | undefined; hasOverlay: boolean }> = []
    const unsubscribe = runtime.replica.subscribe((resource) => {
      if (resource.id !== 'session_a') return
      const record = runtime.replica.get<SessionContentState>(resource)
      observations.push({ readState: record.metadata.readState, hasOverlay: record.value?.transientRun !== null && record.value?.transientRun !== undefined })
    })
    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_2')
    unsubscribe()
    expect(observations.at(-1)).toEqual({ readState: 'stale', hasOverlay: false })
    expect(observations.some((entry) => entry.readState === 'ready' && !entry.hasOverlay)).toBe(false)
  })

  it('unsubscribes by clearing transient state while retaining durable content', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    const release = runtime.subscribe({ type: 'session_content', id: 'session_a' }, { retainOnRelease: true })
    runtime.start()
    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, 'session_a'))
    transport.emit(snapshot(first.payload.subscription_id, 'session_a', content('session_a')))
    await settleSnapshot()
    transport.emit(event(first.payload.subscription_id, 'session_a', '1', { type: 'run.started', status: 'running' }))
    expect(runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' }).value?.transientRun).not.toBeNull()
    release()
    const retained = runtime.replica.get<SessionContentState>({ type: 'session_content', id: 'session_a' })
    expect(retained.initialized).toBe(true)
    expect(retained.value?.snapshot.session.id).toBe('session_a')
    expect(retained.value?.transientRun).toBeNull()
  })
})
