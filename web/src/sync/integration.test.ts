import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { CommandFacade } from './commandFacade'
import { SyncRuntime } from './runtime'
import { SessionIndexRepository } from './sessionIndexRepository'
import { WebSocketTransport, type SocketLike } from './transport'

class ManualTimers {
  private next = 1
  readonly timeouts = new Map<number, () => void>()
  readonly intervals = new Map<number, () => void>()
  setTimeout = (handler: () => void): number => { const id = this.next++; this.timeouts.set(id, handler); return id }
  clearTimeout = (id: number): void => { this.timeouts.delete(id) }
  setInterval = (handler: () => void): number => { const id = this.next++; this.intervals.set(id, handler); return id }
  clearInterval = (id: number): void => { this.intervals.delete(id) }
  runNextTimeout(): void {
    const entry = this.timeouts.entries().next().value as [number, () => void] | undefined
    if (!entry) throw new Error('missing timer')
    this.timeouts.delete(entry[0])
    entry[1]()
  }
  runLastTimeout(): void {
    const entries = [...this.timeouts.entries()]
    const entry = entries.at(-1)
    if (!entry) throw new Error('missing timer')
    this.timeouts.delete(entry[0])
    entry[1]()
  }
}

class IntegrationSocket implements SocketLike {
  readonly readyState = 1
  onopen: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  readonly sent: string[] = []
  closed = false
  throwOnSend = false
  constructor(private readonly onClosed: () => void) {}
  send(data: string): void {
    if (this.throwOnSend) throw new Error('integration send failure')
    this.sent.push(data)
  }
  close(): void {
    if (this.closed) return
    this.closed = true
    this.onClosed()
    this.onclose?.()
  }
  open(): void { this.onopen?.() }
  receive(data: unknown): void { this.onmessage?.({ data }) }
}

const summary = (id: string, overrides: Record<string, unknown> = {}) => ({
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

function frame(type: string, id: string, payload: unknown): string {
  return JSON.stringify({ version: 1, type, id, payload })
}

function decodeSent(socket: IntegrationSocket, type: ProtocolMessage['type']): ProtocolMessage {
  const encoded = [...socket.sent].reverse().find((value) => JSON.parse(value).type === type)
  if (!encoded) throw new Error(`missing ${type}`)
  return decodeMessage(encoded)
}

async function settle(): Promise<void> {
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()
}

describe('C2 real transport/runtime/command composition', () => {
  it('runs snapshot/change/ACK, subscription-send recovery, command-send recovery, and clean stop on one tab socket at a time', async () => {
    const timers = new ManualTimers()
    const sockets: IntegrationSocket[] = []
    let activeSockets = 0
    const fetcher = async (_input: RequestInfo | URL): Promise<Response> => ({
      ok: true,
      json: async () => ({ ticket: `ticket_${sockets.length + 1}` }),
    } as Response)
    const transport = new WebSocketTransport({
      capabilityToken: () => 'capability',
      fetcher,
      websocketFactory: () => {
        activeSockets += 1
        const socket = new IntegrationSocket(() => { activeSockets -= 1 })
        sockets.push(socket)
        return socket
      },
      reconnectBaseMS: 1,
      reconnectMaxMS: 1,
      maxReconnectAttempts: 3,
      handshakeTimeoutMS: 10,
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout,
      setInterval: timers.setInterval,
      clearInterval: timers.clearInterval,
    })
    const runtime = new SyncRuntime({ transport })
    const commands = new CommandFacade({ transport, timeoutMS: 1000, setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout })
    const repository = new SessionIndexRepository(runtime.replica)
    const release = runtime.subscribe({ type: 'session_index', id: 'project_a' })
    runtime.start()
    commands.start()
    await settle()
    expect(sockets).toHaveLength(1)
    expect(activeSockets).toBe(1)

    const first = sockets[0]
    first.open()
    const hello = decodeSent(first, 'hello')
    expect(hello.type).toBe('hello')
    first.receive(frame('welcome', 'welcome_1', {
      selected_version: 1, connection_id: 'connection_1', server_epoch: 'epoch_1', heartbeat_interval_ms: 1000, max_message_bytes: 64 * 1024,
    }))
    const subscribe = decodeSent(first, 'subscribe')
    if (subscribe.type !== 'subscribe') throw new Error('wrong subscribe')
    const firstSubscriptionID = subscribe.payload.subscription_id
    first.receive(frame('subscribed', 'subscribed_1', {
      subscription_id: firstSubscriptionID, resource: { type: 'session_index', id: 'project_a' }, stream_epoch: 'stream_1', sequence: '1',
    }))
    first.receive(frame('snapshot', 'snapshot_1', {
      subscription_id: firstSubscriptionID, resource: { type: 'session_index', id: 'project_a' }, stream_epoch: 'stream_1', sequence: '1', resource_revision: '1',
      content: { inline: { sessions: [summary('a')] } },
    }))
    await settle()
    expect(repository.getSummary('project_a', 'a')?.status).toBe('idle')
    expect(decodeSent(first, 'ack').type).toBe('ack')

    // A real public send failure while requesting a resource resync closes the
    // transport; the runtime then resubscribes on the next ready socket.
    first.throwOnSend = true
    first.receive(frame('resync_required', 'resync_1', {
      subscription_id: firstSubscriptionID, resource: { type: 'session_index', id: 'project_a' }, reason: 'too_old',
    }))
    expect(first.closed).toBe(true)
    expect(activeSockets).toBe(0)
    timers.runLastTimeout()
    await settle()
    expect(sockets).toHaveLength(2)
    expect(activeSockets).toBe(1)
    const second = sockets[1]
    second.open()
    second.receive(frame('welcome', 'welcome_2', {
      selected_version: 1, connection_id: 'connection_2', server_epoch: 'epoch_1', heartbeat_interval_ms: 1000, max_message_bytes: 64 * 1024,
    }))
    const freshSubscribe = decodeSent(second, 'subscribe')
    if (freshSubscribe.type !== 'subscribe') throw new Error('wrong fresh subscribe')
    expect(freshSubscribe.payload.resume).toBeUndefined()
    second.receive(frame('subscribed', 'subscribed_2', {
      subscription_id: freshSubscribe.payload.subscription_id, resource: { type: 'session_index', id: 'project_a' }, stream_epoch: 'stream_1', sequence: '1',
    }))
    second.receive(frame('snapshot', 'snapshot_2', {
      subscription_id: freshSubscribe.payload.subscription_id, resource: { type: 'session_index', id: 'project_a' }, stream_epoch: 'stream_1', sequence: '1', resource_revision: '1',
      content: { inline: { sessions: [summary('a')] } },
    }))
    await settle()

    second.receive(frame('change', 'change_2', {
      subscription_id: freshSubscribe.payload.subscription_id, resource: { type: 'session_index', id: 'project_a' }, stream_epoch: 'stream_1', sequence: '2', previous_sequence: '1', resource_revision: '2',
      operations: [{ op: 'upsert', key: 'b', value: summary('b', { status: 'completed', run_id: 'run_b', has_unread_result: true, resource_revision: '2' }) }],
    }))
    expect(repository.getSummary('project_a', 'b')).toMatchObject({ status: 'completed', has_unread_result: true })
    const changeAck = decodeSent(second, 'ack')
    if (changeAck.type === 'ack') expect(changeAck.payload.sequence).toBe('2')

    // A command sent through the same public transport send path also
    // recovers after the socket closes and is resent on the next generation.
    second.throwOnSend = true
    const pending = commands.markRead('a', 'run_a', 'project_a')
    expect(second.closed).toBe(true)
    expect(activeSockets).toBe(0)
    timers.runLastTimeout()
    await settle()
    expect(sockets).toHaveLength(3)
    const third = sockets[2]
    third.open()
    third.receive(frame('welcome', 'welcome_3', {
      selected_version: 1, connection_id: 'connection_3', server_epoch: 'epoch_1', heartbeat_interval_ms: 1000, max_message_bytes: 64 * 1024,
    }))
    const resumedSubscribe = decodeSent(third, 'subscribe')
    const resentCommand = decodeSent(third, 'command')
    if (resumedSubscribe.type !== 'subscribe' || resentCommand.type !== 'command') throw new Error('missing reconnect frames')
    expect(resumedSubscribe.payload.resume).toEqual({ stream_epoch: 'stream_1', sequence: '2' })
    third.receive(frame('subscribed', 'subscribed_3', {
      subscription_id: resumedSubscribe.payload.subscription_id, resource: { type: 'session_index', id: 'project_a' }, stream_epoch: 'stream_1', sequence: '2',
    }))
    third.receive(frame('command_result', 'command_result_3', {
      request_id: resentCommand.payload.request_id, status: 'succeeded', result: { session_id: 'a', run_id: 'run_a', marked_read: true },
    }))
    await expect(pending).resolves.toEqual({ session_id: 'a', run_id: 'run_a', marked_read: true })
    expect(activeSockets).toBe(1)

    const stopped = commands.markRead('a', 'run_a2')
    commands.stop()
    await expect(stopped).rejects.toMatchObject({ code: 'stopped' })
    release()
    runtime.stop()
    repository.dispose()
    transport.stop()
    expect(activeSockets).toBe(0)
    expect(timers.timeouts.size).toBe(0)
    expect(timers.intervals.size).toBe(0)
    expect(third.onopen).toBeNull()
    expect(third.onmessage).toBeNull()
    expect(third.onclose).toBeNull()
  })
})
