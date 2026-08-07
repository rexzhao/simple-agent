import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { LocalReplica } from './localReplica'
import { BlobClient } from './blobClient'
import { SyncRuntime, SyncSubscriptionError, type RuntimeTransport } from './runtime'
import { CommandFacade } from './commandFacade'
import { SessionIndexRepository } from './sessionIndexRepository'
import { SessionIndexAdapter } from './sessionIndexAdapter'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'

class FakeTransport implements RuntimeTransport {
  isReady = true
  connectionGeneration = 1
  serverEpoch: string | undefined = 'server_1'
  sent: ProtocolMessage[] = []
  failNextSends = 0
  startCalls = 0
  stopCalls = 0
  startMakesReady = true
  private messageListeners = new Set<(message: ProtocolMessage, generation: number) => void>()
  private readyListeners = new Set<(event: TransportReadyEvent) => void>()
  private closeListeners = new Set<(event: TransportCloseEvent) => void>()
  start(): void { this.startCalls += 1; if (this.startMakesReady) this.isReady = true }
  stop(): void { this.stopCalls += 1; this.isReady = false }
  send(message: ProtocolMessage): void {
    if (this.failNextSends > 0) { this.failNextSends -= 1; throw new Error('fake send failure') }
    this.sent.push(message)
  }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messageListeners.add(listener); return () => this.messageListeners.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.readyListeners.add(listener); return () => this.readyListeners.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.closeListeners.add(listener); return () => this.closeListeners.delete(listener) }
  emit(message: unknown, generation = this.connectionGeneration): void {
    const decoded = typeof message === 'string' ? decodeMessage(message) : message as ProtocolMessage
    for (const listener of [...this.messageListeners]) listener(decoded, generation)
  }
  emitReady(epoch = this.serverEpoch ?? 'server_1'): void {
    this.serverEpoch = epoch
    for (const listener of [...this.readyListeners]) listener({ generation: this.connectionGeneration, serverEpoch: epoch, connectionID: `connection_${this.connectionGeneration}`, heartbeatIntervalMS: 1000, maxMessageBytes: 1024 * 1024 })
  }
  emitClose(willRetry = true, generation = this.connectionGeneration): void {
    this.isReady = false
    for (const listener of [...this.closeListeners]) listener({ generation, willRetry })
  }
  last(type: ProtocolMessage['type']): ProtocolMessage {
    const message = [...this.sent].reverse().find((candidate) => candidate.type === type)
    if (!message) throw new Error(`missing ${type}`)
    return message
  }
}

const summary = (id: string, projectID: string, overrides: Record<string, unknown> = {}) => ({
  session_id: id,
  project_id: projectID,
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

const message = (value: unknown): ProtocolMessage => decodeMessage(JSON.stringify(value))

function subscribed(subscriptionID: string, resourceID: string, epoch = 'stream_1', sequence = '1'): ProtocolMessage {
  return message({ version: 1, type: 'subscribed', id: 'subscribed_1', payload: { subscription_id: subscriptionID, resource: { type: 'session_index', id: resourceID }, stream_epoch: epoch, sequence } })
}

function snapshotMessage(subscriptionID: string, resourceID: string, content: unknown, sequence = '1', epoch = 'stream_1', revision = '1'): ProtocolMessage {
  return message({ version: 1, type: 'snapshot', id: `snapshot_${sequence}`, payload: { subscription_id: subscriptionID, resource: { type: 'session_index', id: resourceID }, stream_epoch: epoch, sequence, resource_revision: revision, content } })
}

function changeMessage(subscriptionID: string, resourceID: string, operations: unknown[], previous = '1', sequence = '2', epoch = 'stream_1', revision = '2'): ProtocolMessage {
  return message({ version: 1, type: 'change', id: `change_${sequence}`, payload: { subscription_id: subscriptionID, resource: { type: 'session_index', id: resourceID }, stream_epoch: epoch, sequence, previous_sequence: previous, resource_revision: revision, operations } })
}

const projectSummary = (id: string, overrides: Record<string, unknown> = {}) => ({
  id,
  root: `/workspace/${id}`,
  display_name: id,
  archived: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
  ...overrides,
})

function projectSubscribed(subscriptionID: string, epoch = 'stream_1', sequence = '0'): ProtocolMessage {
  return message({ version: 1, type: 'subscribed', id: `project-subscribed-${sequence}`, payload: { subscription_id: subscriptionID, resource: { type: 'project_index', id: 'server' }, stream_epoch: epoch, sequence } })
}

function projectSnapshotMessage(subscriptionID: string, content: unknown, sequence = '0', epoch = 'stream_1', revision = '0'): ProtocolMessage {
  return message({ version: 1, type: 'snapshot', id: `project-snapshot-${sequence}`, payload: { subscription_id: subscriptionID, resource: { type: 'project_index', id: 'server' }, stream_epoch: epoch, sequence, resource_revision: revision, content } })
}

function projectChangeMessage(subscriptionID: string, operations: unknown[], previous = '0', sequence = '1', epoch = 'stream_1', revision = '1'): ProtocolMessage {
  return message({ version: 1, type: 'change', id: `project-change-${sequence}`, payload: { subscription_id: subscriptionID, resource: { type: 'project_index', id: 'server' }, stream_epoch: epoch, sequence, previous_sequence: previous, resource_revision: revision, operations } })
}

describe('SyncRuntime snapshot barrier and continuity', () => {
  it('queues project_index changes behind a Blob snapshot and applies them in order', async () => {
    const transport = new FakeTransport()
    const blobResult: { resolve?: (value: unknown) => void } = {}
    const blobClient = { getJSON: () => new Promise<unknown>((resolve) => { blobResult.resolve = resolve }) } as unknown as BlobClient
    const runtime = new SyncRuntime({ transport, blobClient })
    runtime.subscribe({ type: 'project_index', id: 'server' })
    runtime.start()
    const subscribe = transport.last('subscribe')
    if (subscribe.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(projectSubscribed(subscribe.payload.subscription_id))
    transport.emit(projectSnapshotMessage(subscribe.payload.subscription_id, { blob: { id: 'project-blob', url: '/api/blobs/project-blob', content_type: 'application/json', size: 1, sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z' } }))
    transport.emit(projectChangeMessage(subscribe.payload.subscription_id, [{
      op: 'upsert', key: 'project_b', value: projectSummary('project_b', { display_name: 'B renamed', updated_at: '2025-01-02T00:00:00Z' }),
    }]))
    expect(runtime.replica.get({ type: 'project_index', id: 'server' }).value).toBeUndefined()
    blobResult.resolve?.({ projects: [projectSummary('project_a')] })
    await Promise.resolve()
    await Promise.resolve()
    const value = runtime.replica.get<{ orderedIDs: readonly string[]; summariesByID: Record<string, { display_name: string }> }>({ type: 'project_index', id: 'server' }).value
    expect(value?.orderedIDs).toEqual(['project_a', 'project_b'])
    expect(value?.summariesByID.project_b.display_name).toBe('B renamed')
    expect(transport.sent.filter((candidate) => candidate.type === 'ack')).toHaveLength(1)
  })

  it('does not let an old-generation Blob snapshot contaminate a reconnect', async () => {
    const transport = new FakeTransport()
    const blobResult: { resolve?: (value: unknown) => void; reject?: (reason?: unknown) => void } = {}
    const blobClient = { getJSON: () => new Promise<unknown>((resolve, reject) => { blobResult.resolve = resolve; blobResult.reject = reject }) } as unknown as BlobClient
    const runtime = new SyncRuntime({ transport, blobClient })
    runtime.subscribe({ type: 'project_index', id: 'server' })
    runtime.start()
    const first = transport.last('subscribe')
    if (first.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(projectSubscribed(first.payload.subscription_id, 'stream_1', '0'))
    transport.emit(projectSnapshotMessage(first.payload.subscription_id, { blob: { id: 'old-project-blob', url: '/blob', content_type: 'application/json', size: 1, sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z' } }))
    transport.emit(projectChangeMessage(first.payload.subscription_id, [{ op: 'upsert', key: 'old_project', value: projectSummary('old_project') }]))

    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_2')
    const reconnect = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    if (reconnect?.type !== 'subscribe') throw new Error('missing reconnect subscribe')
    blobResult.reject?.(new Error('old Blob failed after reconnect'))
    await Promise.resolve()
    await Promise.resolve()
    expect(runtime.replica.get({ type: 'project_index', id: 'server' }).initialized).toBe(false)

    transport.emit(projectSubscribed(reconnect.payload.subscription_id, 'stream_2', '0'))
    transport.emit(projectSnapshotMessage(reconnect.payload.subscription_id, { inline: { projects: [projectSummary('new_project')] } }, '0', 'stream_2', '0'))
    await Promise.resolve()
    await Promise.resolve()
    const value = runtime.replica.get<{ orderedIDs: readonly string[] }>({ type: 'project_index', id: 'server' }).value
    expect(value?.orderedIDs).toEqual(['new_project'])
  })

  it('queues changes behind a blob snapshot and ACKs only after the queued change applies', async () => {
    const transport = new FakeTransport()
    const blobResult: { resolve?: (value: unknown) => void } = {}
    const blobClient = { getJSON: () => new Promise<unknown>((resolve) => { blobResult.resolve = resolve }) } as unknown as BlobClient
    const runtime = new SyncRuntime({ transport, blobClient, maxQueuedChanges: 2, maxQueuedBytes: 50_000 })
    const repository = new SessionIndexRepository(runtime.replica)
    const observed: string[][] = []
    const unsubscribeReplica = runtime.replica.subscribe((resource) => {
      const value = runtime.replica.get<{ orderedIDs: readonly string[] }>(resource).value
      if (value) observed.push([...value.orderedIDs])
    })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const subscribe = transport.last('subscribe')
    if (subscribe.type !== 'subscribe') throw new Error('wrong subscribe')
    const subscriptionID = subscribe.payload.subscription_id
    transport.emit(subscribed(subscriptionID, 'project_a'))
    transport.emit(snapshotMessage(subscriptionID, 'project_a', { blob: { id: 'blob_1', url: '/api/blobs/blob_1', content_type: 'application/json', size: 1, sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z' } }))
    transport.emit(changeMessage(subscriptionID, 'project_a', [{ op: 'upsert', key: 'b', value: summary('b', 'project_a', { status: 'completed', run_id: 'run_b', has_unread_result: true, resource_revision: '2' }) }]))
    expect(transport.sent.filter((candidate) => candidate.type === 'ack')).toHaveLength(0)

    blobResult.resolve?.({ sessions: [summary('a', 'project_a')] })
    await Promise.resolve()
    await Promise.resolve()
    const model = repository.getProjectReadModel('project_a')
    expect(model.status).toBe('ready')
    expect(model.summaries.map((item) => item.session_id)).toEqual(['a', 'b'])
    const acks = transport.sent.filter((candidate) => candidate.type === 'ack')
    expect(acks).toHaveLength(1)
    if (acks[0].type === 'ack') expect(acks[0].payload.sequence).toBe('2')
    expect(observed).toEqual([['a', 'b']])
    unsubscribeReplica()
  })

  it('keeps the old replica when a queued barrier operation is invalid and sends no ACK', async () => {
    const transport = new FakeTransport()
    const blobResult: { resolve?: (value: unknown) => void } = {}
    const blobClient = { getJSON: () => new Promise<unknown>((resolve) => { blobResult.resolve = resolve }) } as unknown as BlobClient
    const replica = new LocalReplica()
    replica.applySnapshot({ type: 'session_index', id: 'project_a' }, new SessionIndexAdapter('project_a'), { sessions: [summary('old', 'project_a')] }, {
      streamEpoch: 'stream_0', sequence: '1' as never, resourceRevision: '1', generation: 1,
    })
    const runtime = new SyncRuntime({ transport, blobClient, replica })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const first = transport.last('subscribe')
    if (first.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(first.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(first.payload.subscription_id, 'project_a', { blob: { id: 'blob_1', url: '/api/blobs/blob_1', content_type: 'application/json', size: 1, sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z' } }))
    transport.emit(changeMessage(first.payload.subscription_id, 'project_a', [{ op: 'upsert', key: 'b', value: summary('b', 'project_a', { resource_revision: '2' }) }], '1', '2'))
    transport.emit(changeMessage(first.payload.subscription_id, 'project_a', [
      { op: 'upsert', key: 'c', value: summary('c', 'project_a', { resource_revision: '3' }) },
      { op: 'partial', key: 'c', value: {} },
    ], '2', '3'))
    blobResult.resolve?.({ sessions: [summary('a', 'project_a')] })
    await Promise.resolve()
    await Promise.resolve()
    const value = runtime.replica.get<{ orderedIDs: readonly string[] }>({ type: 'session_index', id: 'project_a' }).value
    expect(value?.orderedIDs).toEqual(['old'])
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).metadata.readState).toBe('stale')
    expect(transport.sent.filter((candidate) => candidate.type === 'ack')).toHaveLength(0)
  })

  it('does not partially apply invalid operations and requests a fresh generation', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const firstSubscribe = transport.last('subscribe')
    if (firstSubscribe.type !== 'subscribe') throw new Error('wrong subscribe')
    const firstID = firstSubscribe.payload.subscription_id
    transport.emit(subscribed(firstID, 'project_a'))
    transport.emit(snapshotMessage(firstID, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.sent = []
    transport.emit(changeMessage(firstID, 'project_a', [
      { op: 'upsert', key: 'a', value: summary('a', 'project_a', { display_name: 'new', resource_revision: '2' }) },
      { op: 'partial', key: 'a', value: {} },
    ]))
    const repository = new SessionIndexRepository(runtime.replica)
    expect(repository.getSummary('project_a', 'a')?.display_name).toBe('a')
    expect(transport.sent.some((candidate) => candidate.type === 'unsubscribe')).toBe(true)
    const nextSubscribe = transport.sent.find((candidate) => candidate.type === 'subscribe')
    expect(nextSubscribe).toBeDefined()
    if (nextSubscribe?.type === 'subscribe') expect(nextSubscribe.payload.subscription_id).not.toBe(firstID)
  })

  it('resumes with a token, ignores retired subscription messages, and resyncs on a gap', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const initial = transport.last('subscribe')
    if (initial.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(initial.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(initial.payload.subscription_id, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.sent = []
    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_1')
    const resumed = transport.last('subscribe')
    if (resumed.type !== 'subscribe') throw new Error('wrong resume subscribe')
    expect(resumed.payload.resume).toEqual({ stream_epoch: 'stream_1', sequence: '1' })
    transport.emit(subscribed(resumed.payload.subscription_id, 'project_a'))
    transport.emit(changeMessage(resumed.payload.subscription_id, 'project_a', [{ op: 'remove', key: 'a' }], '1', '2'))
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).initialized).toBe(true)
    transport.emit(snapshotMessage(initial.payload.subscription_id, 'project_a', { inline: { sessions: [summary('old', 'project_a')] } }), 1)
    expect(runtime.replica.get<{ summariesByID: Record<string, unknown> }>({ type: 'session_index', id: 'project_a' }).value?.summariesByID.old).toBeUndefined()

    transport.emit(changeMessage(resumed.payload.subscription_id, 'project_a', [{ op: 'remove', key: 'missing' }], '9', '10'))
    const fresh = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(fresh).toBeDefined()
    if (fresh?.type === 'subscribe') expect(fresh.payload.resume).toBeUndefined()
  })

  it('keeps replay stale until subscribed.sequence, and rejects mismatch/ahead barriers without applying', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const initial = transport.last('subscribe')
    if (initial.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(initial.payload.subscription_id, 'project_a', 'stream_1', '1'))
    transport.emit(snapshotMessage(initial.payload.subscription_id, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.sent = []
    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_1')
    const resumed = transport.last('subscribe')
    if (resumed.type !== 'subscribe') throw new Error('wrong resume subscribe')
    transport.emit(subscribed(resumed.payload.subscription_id, 'project_a', 'stream_1', '3'))
    transport.emit(changeMessage(resumed.payload.subscription_id, 'project_a', [{ op: 'upsert', key: 'a', value: summary('a', 'project_a', { display_name: 'replayed-2', resource_revision: '2' }) }], '1', '2'))
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).metadata.readState).toBe('stale')
    transport.emit(changeMessage(resumed.payload.subscription_id, 'project_a', [{ op: 'upsert', key: 'a', value: summary('a', 'project_a', { display_name: 'replayed-3', resource_revision: '3' }) }], '2', '3'))
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).metadata.readState).toBe('ready')
    expect(runtime.replica.get<{ summariesByID: Record<string, { display_name: string }> }>({ type: 'session_index', id: 'project_a' }).value?.summariesByID.a.display_name).toBe('replayed-3')

    transport.sent = []
    transport.emitClose()
    transport.connectionGeneration = 3
    transport.isReady = true
    transport.emitReady('server_1')
    const ahead = transport.last('subscribe')
    if (ahead.type !== 'subscribe') throw new Error('wrong ahead subscribe')
    transport.emit(subscribed(ahead.payload.subscription_id, 'project_a', 'stream_1', '2'))
    expect(transport.sent.some((candidate) => candidate.type === 'unsubscribe')).toBe(true)
  })

  it('rejects pre-subscribed and barrier-mismatched snapshots without touching existing data', async () => {
    const transport = new FakeTransport()
    const replica = new LocalReplica()
    const resource = { type: 'session_index' as const, id: 'project_a' }
    replica.applySnapshot(resource, new SessionIndexAdapter('project_a'), { sessions: [summary('old', 'project_a')] }, {
      streamEpoch: 'stream_old', sequence: '7' as never, resourceRevision: '7', generation: 1,
    })
    const runtime = new SyncRuntime({ transport, replica })
    runtime.subscribe(resource)
    runtime.start()
    const first = transport.last('subscribe')
    if (first.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(snapshotMessage(first.payload.subscription_id, 'project_a', { inline: { sessions: [summary('bad', 'project_a')] } }))
    const second = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(second).toBeDefined()
    expect(replica.get<{ orderedIDs: readonly string[] }>(resource).value?.orderedIDs).toEqual(['old'])
    if (second?.type !== 'subscribe') throw new Error('missing resync subscribe')
    transport.emit(subscribed(second.payload.subscription_id, 'project_a', 'stream_1', '2'))
    transport.emit(snapshotMessage(second.payload.subscription_id, 'project_a', { inline: { sessions: [summary('bad', 'project_a')] } }, '1'))
    const third = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(third).toBeDefined()
    expect(replica.get<{ orderedIDs: readonly string[] }>(resource).value?.orderedIDs).toEqual(['old'])
  })

  it('bounds both queued change count and bytes and resyncs without skipping sequence', () => {
    const makeRuntime = (options: { maxQueuedChanges?: number; maxQueuedBytes?: number }) => {
      const transport = new FakeTransport()
      const blobClient = { getJSON: () => new Promise<unknown>(() => {}) } as unknown as BlobClient
      const runtime = new SyncRuntime({ transport, blobClient, ...options })
      runtime.subscribe({ type: 'session_index', id: 'project_a' })
      runtime.start()
      const subscription = transport.last('subscribe')
      if (subscription.type !== 'subscribe') throw new Error('wrong subscribe')
      transport.emit(subscribed(subscription.payload.subscription_id, 'project_a'))
      transport.emit(snapshotMessage(subscription.payload.subscription_id, 'project_a', { blob: {
        id: 'blob_1', url: '/api/blobs/blob_1', content_type: 'application/json', size: 1,
        sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z',
      } }))
      return { runtime, transport, subscriptionID: subscription.payload.subscription_id }
    }

    const byCount = makeRuntime({ maxQueuedChanges: 1, maxQueuedBytes: 50_000 })
    transportChange(byCount.transport, byCount.subscriptionID, 2)
    transportChange(byCount.transport, byCount.subscriptionID, 3, '2')
    expect(byCount.transport.sent.filter((candidate) => candidate.type === 'ack')).toHaveLength(0)
    expect(byCount.transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(2)
    // With no prior value, the typed read model remains loading rather than
    // manufacturing stale data; the subscription is nevertheless already in
    // a fresh generation and no sequence was skipped.
    expect(byCount.runtime.replica.get({ type: 'session_index', id: 'project_a' }).metadata.readState).toBe('loading')

    const byBytes = makeRuntime({ maxQueuedChanges: 10, maxQueuedBytes: 1 })
    transportChange(byBytes.transport, byBytes.subscriptionID, 2)
    expect(byBytes.transport.sent.filter((candidate) => candidate.type === 'ack')).toHaveLength(0)
    expect(byBytes.transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(2)
  })

  it('clears resume on a server epoch change and handles too-old resync', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const initial = transport.last('subscribe')
    if (initial.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(initial.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(initial.payload.subscription_id, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.emit(message({ version: 1, type: 'resync_required', id: 'resync_1', payload: {
      subscription_id: initial.payload.subscription_id, resource: { type: 'session_index', id: 'project_a' }, reason: 'too_old',
    }}))
    const afterTooOld = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(afterTooOld?.type).toBe('subscribe')
    if (afterTooOld?.type !== 'subscribe') throw new Error('missing too-old subscribe')
    expect(afterTooOld.payload.resume).toBeUndefined()

    transport.emit(subscribed(afterTooOld.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(afterTooOld.payload.subscription_id, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_2')
    const afterEpoch = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(afterEpoch?.type).toBe('subscribe')
    if (afterEpoch?.type === 'subscribe') expect(afterEpoch.payload.resume).toBeUndefined()
  })

  it('does not apply a replay when the subscribed stream epoch disagrees with the resume token', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport })
    const resource = { type: 'session_index' as const, id: 'project_a' }
    runtime.subscribe(resource)
    runtime.start()
    const initial = transport.last('subscribe')
    if (initial.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(initial.payload.subscription_id, 'project_a', 'stream_1', '1'))
    transport.emit(snapshotMessage(initial.payload.subscription_id, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.emitClose()
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_1')
    const resumed = transport.last('subscribe')
    if (resumed.type !== 'subscribe') throw new Error('wrong resume subscribe')
    transport.emit(subscribed(resumed.payload.subscription_id, 'project_a', 'stream_2', '2'))
    transport.emit(changeMessage(resumed.payload.subscription_id, 'project_a', [{ op: 'remove', key: 'a' }], '1', '2', 'stream_2'))
    expect(runtime.replica.get<{ orderedIDs: readonly string[] }>(resource).value?.orderedIDs).toEqual(['a'])
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(3)
  })

  it('boundedly resubscribes on subscription errors and reaches a typed terminal error', () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport, maxResyncAttempts: 1 })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const first = transport.last('subscribe')
    if (first.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(message({ version: 1, type: 'error', id: 'error_1', payload: {
      code: 'subscribe_denied', message: 'denied', request_id: first.id,
    }}))
    const second = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(second).toBeDefined()
    if (second?.type !== 'subscribe') throw new Error('missing bounded resubscribe')
    transport.emit(message({ version: 1, type: 'error', id: 'error_2', payload: {
      code: 'subscribe_denied', message: 'denied again', request_id: second.id,
    }}))
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(2)
    const record = runtime.replica.get({ type: 'session_index', id: 'project_a' })
    expect(record.metadata.readState).toBe('error')
    expect(record.metadata.error).toMatchObject({ code: 'server' })
  })

  it('publishes terminal error before the first socket is ready and wakes a terminated transport on retry', () => {
    const transport = new FakeTransport()
    transport.isReady = false
    transport.startMakesReady = false
    const runtime = new SyncRuntime({ transport })
    const resource = { type: 'project_index' as const, id: 'server' }
    runtime.subscribe(resource)
    runtime.start()
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(0)

    // No subscription was ever assigned a socket generation, matching a
    // first-handshake reconnect exhaustion from WebSocketTransport.
    transport.emitClose(false)
    expect(runtime.replica.get(resource).metadata.readState).toBe('error')
    expect(runtime.replica.get(resource).metadata.error).toMatchObject({ code: 'transport' })

    const startsBeforeRetry = transport.startCalls
    const stopsBeforeRetry = transport.stopCalls
    runtime.retry(resource)
    expect(transport.startCalls).toBe(startsBeforeRetry + 1)
    expect(transport.stopCalls).toBe(stopsBeforeRetry)
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(0)

    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_2')
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(1)
    runtime.stop()
  })

  it('keeps shared-transport waiting subscriptions recoverable while preserving resource terminal errors', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport, maxResyncAttempts: 1 })
    const projectResource = { type: 'project_index' as const, id: 'server' }
    const sessionResource = { type: 'session_index' as const, id: 'project_a' }
    const erroredSessionResource = { type: 'session_index' as const, id: 'project_b' }
    runtime.subscribe(projectResource)
    runtime.subscribe(sessionResource)
    runtime.subscribe(erroredSessionResource)
    runtime.start()

    const subscribeFor = (resource: { type: string; id: string }) => [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === resource.type && candidate.payload.resource.id === resource.id)
    const initialProject = subscribeFor(projectResource)
    const initialSession = subscribeFor(sessionResource)
    const initialErroredSession = subscribeFor(erroredSessionResource)
    if (initialProject?.type !== 'subscribe' || initialSession?.type !== 'subscribe' || initialErroredSession?.type !== 'subscribe') throw new Error('missing initial subscriptions')

    transport.emit(projectSubscribed(initialProject.payload.subscription_id, 'stream_1', '0'))
    transport.emit(projectSnapshotMessage(initialProject.payload.subscription_id, { inline: { projects: [projectSummary('project_a')] } }))
    transport.emit(subscribed(initialSession.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(initialSession.payload.subscription_id, 'project_a', { inline: { sessions: [summary('session_a', 'project_a')] } }))
    transport.emit(subscribed(initialErroredSession.payload.subscription_id, 'project_b'))
    transport.emit(snapshotMessage(initialErroredSession.payload.subscription_id, 'project_b', { inline: { sessions: [summary('session_b', 'project_b')] } }))
    await Promise.resolve()

    const subscriptionError = (requestID: string, id: string) => message({ version: 1, type: 'error', id, payload: { code: 'subscribe_denied', message: 'denied', request_id: requestID } })
    transport.emit(subscriptionError(initialErroredSession.id, 'session-b-error-1'))
    const erroredSessionResubscribe = subscribeFor(erroredSessionResource)
    if (erroredSessionResubscribe?.type !== 'subscribe') throw new Error('missing resource error resubscribe')
    transport.emit(subscriptionError(erroredSessionResubscribe.id, 'session-b-error-2'))
    expect(runtime.replica.get(erroredSessionResource).metadata.readState).toBe('stale')
    expect(runtime.replica.get(erroredSessionResource).metadata.error).toMatchObject({ code: 'server' })

    const projectSubscribeCountBeforeClose = transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'project_index').length
    const sessionSubscribeCountBeforeClose = transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index' && candidate.payload.resource.id === 'project_a').length
    const erroredSessionSubscribeCountBeforeClose = transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index' && candidate.payload.resource.id === 'project_b').length
    transport.emitClose(false)
    expect(runtime.replica.get(projectResource).metadata.readState).toBe('stale')
    expect(runtime.replica.get(sessionResource).metadata.readState).toBe('stale')
    expect(runtime.replica.get(erroredSessionResource).metadata.readState).toBe('stale')

    transport.startMakesReady = false
    runtime.retry(projectResource)
    expect(transport.startCalls).toBe(2)
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe').length).toBe(
      projectSubscribeCountBeforeClose + sessionSubscribeCountBeforeClose + erroredSessionSubscribeCountBeforeClose,
    )

    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_2')
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'project_index')).toHaveLength(projectSubscribeCountBeforeClose + 1)
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index' && candidate.payload.resource.id === 'project_a')).toHaveLength(sessionSubscribeCountBeforeClose + 1)
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index' && candidate.payload.resource.id === 'project_b')).toHaveLength(erroredSessionSubscribeCountBeforeClose)
    runtime.stop()
  })

  it('retries one terminal resource on the shared socket without disturbing another resource or a pending command', async () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport, maxResyncAttempts: 1 })
    const projectResource = { type: 'project_index' as const, id: 'server' }
    const sessionResource = { type: 'session_index' as const, id: 'project_a' }
    runtime.subscribe(projectResource)
    runtime.subscribe(sessionResource)
    runtime.start()
    const projectSubscribe = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'project_index')
    const sessionSubscribe = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index')
    if (projectSubscribe?.type !== 'subscribe' || sessionSubscribe?.type !== 'subscribe') throw new Error('missing initial subscriptions')

    const subscriptionError = (requestID: string, id: string) => message({ version: 1, type: 'error', id, payload: { code: 'subscribe_denied', message: 'denied', request_id: requestID } })
    transport.emit(subscriptionError(projectSubscribe.id, 'project-error-1'))
    const projectResubscribe = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'project_index')
    if (projectResubscribe?.type !== 'subscribe') throw new Error('missing bounded project resubscribe')
    transport.emit(subscriptionError(projectResubscribe.id, 'project-error-2'))
    expect(runtime.replica.get(projectResource).metadata.readState).toBe('error')
    const sessionIDBeforeRetry = sessionSubscribe.payload.subscription_id
    const startCalls = transport.startCalls
    const stopCalls = transport.stopCalls
    const sessionMessagesBeforeRetry = transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index').length

    const commands = new CommandFacade({ transport, operationIDGenerator: () => 'operation_pending', requestIDGenerator: () => 'request_pending' })
    commands.start()
    const pending = commands.createProject('/workspace/pending', 'Pending', { operationID: 'operation_pending' })
    const command = transport.last('command')
    if (command.type !== 'command') throw new Error('missing pending command')
    runtime.retry(projectResource)

    expect(transport.startCalls).toBe(startCalls)
    expect(transport.stopCalls).toBe(stopCalls)
    const retriedProject = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'project_index')
    if (retriedProject?.type !== 'subscribe') throw new Error('missing isolated retry subscription')
    expect(retriedProject.payload.subscription_id).not.toBe(projectResubscribe.payload.subscription_id)
    const latestSessionSubscribe = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index')
    if (latestSessionSubscribe?.type !== 'subscribe') throw new Error('missing unchanged session subscription')
    expect(latestSessionSubscribe.payload.subscription_id).toBe(sessionIDBeforeRetry)
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index')).toHaveLength(sessionMessagesBeforeRetry)

    transport.emit(message({ version: 1, type: 'command_result', id: 'command-result-pending', payload: {
      request_id: command.payload.request_id, status: 'succeeded', result: { operation_id: 'operation_pending', project_id: 'project_pending', created: true },
    }}))
    await expect(pending).resolves.toEqual({ operation_id: 'operation_pending', project_id: 'project_pending', created: true })

    // A send failure during the same resource's retry remains isolated too:
    // the command on this shared socket must still be able to receive its
    // result, while the target resource alone returns to terminal error.
    const projectAfterRetry = retriedProject
    transport.emit(subscriptionError(projectAfterRetry.id, 'project-error-3'))
    const projectResubscribeAfterRetry = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'project_index')
    if (projectResubscribeAfterRetry?.type !== 'subscribe') throw new Error('missing second bounded project resubscribe')
    transport.emit(subscriptionError(projectResubscribeAfterRetry.id, 'project-error-4'))
    expect(runtime.replica.get(projectResource).metadata.readState).toBe('error')

    const commandsAfterRetryFailure = new CommandFacade({ transport, operationIDGenerator: () => 'operation_pending_2', requestIDGenerator: () => 'request_pending_2' })
    commandsAfterRetryFailure.start()
    const pendingAfterRetryFailure = commandsAfterRetryFailure.createProject('/workspace/pending-2', 'Pending 2', { operationID: 'operation_pending_2' })
    const commandAfterRetryFailure = transport.sent.filter((candidate) => candidate.type === 'command').at(-1)
    if (commandAfterRetryFailure?.type !== 'command') throw new Error('missing second pending command')
    transport.failNextSends = 2
    runtime.retry(projectResource)
    expect(transport.startCalls).toBe(startCalls)
    expect(transport.stopCalls).toBe(stopCalls)
    expect(runtime.replica.get(projectResource).metadata.readState).toBe('error')
    const latestSessionAfterRetryFailure = [...transport.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.type === 'session_index')
    if (latestSessionAfterRetryFailure?.type !== 'subscribe') throw new Error('missing unchanged session subscription after retry failure')
    expect(latestSessionAfterRetryFailure.payload.subscription_id).toBe(sessionIDBeforeRetry)
    transport.emit(message({ version: 1, type: 'command_result', id: 'command-result-pending-2', payload: {
      request_id: commandAfterRetryFailure.payload.request_id, status: 'succeeded', result: { operation_id: 'operation_pending_2', project_id: 'project_pending_2', created: true },
    }}))
    await expect(pendingAfterRetryFailure).resolves.toEqual({ operation_id: 'operation_pending_2', project_id: 'project_pending_2', created: true })
    commandsAfterRetryFailure.stop()
    commands.stop()
    runtime.stop()
  })

  it('resyncs overlapping snapshots and recovers from ACK send failure with a resume', async () => {
    const transport = new FakeTransport()
    const blobResult: { resolve?: (value: unknown) => void } = {}
    const blobClient = { getJSON: () => new Promise<unknown>((resolve) => { blobResult.resolve = resolve }) } as unknown as BlobClient
    const runtime = new SyncRuntime({ transport, blobClient })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    const first = transport.last('subscribe')
    if (first.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(first.payload.subscription_id, 'project_a'))
    const blob = { blob: { id: 'blob_1', url: '/blob', content_type: 'application/json', size: 1, sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z' } }
    transport.emit(snapshotMessage(first.payload.subscription_id, 'project_a', blob))
    transport.emit(snapshotMessage(first.payload.subscription_id, 'project_a', blob, '1'))
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(2)
    blobResult.resolve?.({ sessions: [summary('never-applied', 'project_a')] })
    await Promise.resolve()
    await Promise.resolve()
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).value).toBeUndefined()

    const recovered = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    if (recovered?.type !== 'subscribe') throw new Error('missing recovery subscribe')
    transport.emit(subscribed(recovered.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(recovered.payload.subscription_id, 'project_a', { inline: { sessions: [summary('a', 'project_a')] } }))
    await Promise.resolve()
    transport.failNextSends = 1
    transport.emit(changeMessage(recovered.payload.subscription_id, 'project_a', [{ op: 'upsert', key: 'a', value: summary('a', 'project_a', { display_name: 'changed', resource_revision: '2' }) }]))
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).metadata.readState).toBe('stale')
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady('server_1')
    const resumed = transport.sent.filter((candidate) => candidate.type === 'subscribe').at(-1)
    expect(resumed?.type).toBe('subscribe')
    if (resumed?.type === 'subscribe') expect(resumed.payload.resume).toEqual({ stream_epoch: 'stream_1', sequence: '2' })
  })

  it('makes repeated runtime start/stop idempotent and aborts an in-flight snapshot', async () => {
    const transport = new FakeTransport()
    let aborted = false
    const blobClient = {
      getJSON: (_descriptor: unknown, options: { signal?: AbortSignal }) => new Promise<unknown>((_resolve, reject) => {
        options.signal?.addEventListener('abort', () => { aborted = true; reject(new Error('aborted')) }, { once: true })
      }),
    } as unknown as BlobClient
    const runtime = new SyncRuntime({ transport, blobClient })
    runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    runtime.start()
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(1)
    const first = transport.last('subscribe')
    if (first.type !== 'subscribe') throw new Error('wrong subscribe')
    transport.emit(subscribed(first.payload.subscription_id, 'project_a'))
    transport.emit(snapshotMessage(first.payload.subscription_id, 'project_a', { blob: {
      id: 'blob_1', url: '/blob', content_type: 'application/json', size: 1, sha256: 'a', etag: '"a"', expires_at: '2099-01-01T00:00:00Z',
    }}))
    runtime.stop()
    runtime.stop()
    expect(aborted).toBe(true)
    transport.isReady = true
    runtime.start()
    expect(transport.sent.filter((candidate) => candidate.type === 'subscribe')).toHaveLength(2)
    runtime.stop()
  })

  it('bounds active resources, validates canonical IDs, and evicts released replicas', () => {
    const transport = new FakeTransport()
    const runtime = new SyncRuntime({ transport, maxSubscriptions: 1, maxRetainedResources: 1 })
    const releaseA = runtime.subscribe({ type: 'session_index', id: 'project_a' })
    const releaseASecond = runtime.subscribe({ type: 'session_index', id: 'project_a' })
    expect(() => runtime.subscribe({ type: 'session_index', id: ' project_b' })).toThrowError(SyncSubscriptionError)
    expect(runtime.replica.get({ type: 'session_index', id: 'project_b' }).initialized).toBe(false)
    releaseA()
    releaseA()
    releaseASecond()
    releaseASecond()
    expect(runtime.replica.get({ type: 'session_index', id: 'project_a' }).initialized).toBe(false)
    const releaseB = runtime.subscribe({ type: 'session_index', id: 'project_b' })
    releaseB()

    const retained = new SyncRuntime({ transport: new FakeTransport(), maxRetainedResources: 1 })
    const retainA = retained.subscribe({ type: 'session_index', id: 'project_a' }, { retainOnRelease: true })
    retainA()
    expect(retained.replica.get({ type: 'session_index', id: 'project_a' }).metadata.readState).toBe('loading')
    const retainB = retained.subscribe({ type: 'session_index', id: 'project_b' }, { retainOnRelease: true })
    retainB()
    expect(retained.replica.get({ type: 'session_index', id: 'project_a' }).initialized).toBe(false)
  })
})

function transportChange(transport: FakeTransport, subscriptionID: string, sequence: number, previous = '1'): void {
  transport.emit(changeMessage(subscriptionID, 'project_a', [{ op: 'remove', key: `missing_${sequence}` }], previous, String(sequence)))
}
