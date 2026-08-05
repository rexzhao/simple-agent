import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { WebSocketTransport, type SocketLike } from './transport'

class FakeSocket implements SocketLike {
  readonly readyState = 1
  onopen: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  sent: string[] = []
  closed = false
  throwOnSend = false
  send(data: string): void { if (this.throwOnSend) throw new Error('send failed'); this.sent.push(data) }
  close(): void { this.closed = true; this.onclose?.() }
  open(): void { this.onopen?.() }
  receive(data: unknown): void { this.onmessage?.({ data }) }
  forceReceive(handler: ((event: { data: unknown }) => void) | null, data: unknown): void { handler?.({ data }) }
}

function frame(type: string, id: string, payload: unknown): string {
  return JSON.stringify({ version: 1, type, id, payload })
}

class ManualTimers {
  private next = 1
  readonly timers = new Map<number, () => void>()
  readonly intervals = new Map<number, () => void>()
  setTimeout = (handler: () => void): number => { const id = this.next++; this.timers.set(id, handler); return id }
  clearTimeout = (id: number): void => { this.timers.delete(id) }
  setInterval = (handler: () => void): number => { const id = this.next++; this.intervals.set(id, handler); return id }
  clearInterval = (id: number): void => { this.intervals.delete(id) }
  runNextTimeout(): void {
    const first = this.timers.entries().next().value as [number, () => void] | undefined
    if (!first) throw new Error('no timer')
    this.timers.delete(first[0])
    first[1]()
  }
}

async function settle(): Promise<void> {
  await Promise.resolve()
  await Promise.resolve()
}

describe('WebSocketTransport', () => {
  it('uses bearer ticket acquisition, strict JSON hello/welcome, ping-pong, and epoch-aware reconnect', async () => {
    const timers = new ManualTimers()
    const sockets: FakeSocket[] = []
    const fetchCalls: { url: string; init?: RequestInit }[] = []
    const ready: string[] = []
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      fetchCalls.push({ url: String(input), init })
      return { ok: true, json: async () => ({ ticket: 'short-lived-ticket' }) } as Response
    }
    const transport = new WebSocketTransport({
      capabilityToken: () => 'long-lived-secret',
      fetcher,
      websocketFactory: (url) => {
        expect(url).toContain('ticket=short-lived-ticket')
        expect(url).not.toContain('long-lived-secret')
        const socket = new FakeSocket()
        sockets.push(socket)
        return socket
      },
      reconnectBaseMS: 1,
      reconnectMaxMS: 1,
      maxReconnectAttempts: 2,
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout,
      setInterval: timers.setInterval,
      clearInterval: timers.clearInterval,
    })
    transport.onReady((event) => ready.push(event.serverEpoch))
    transport.start()
    await settle()
    expect(fetchCalls).toHaveLength(1)
    expect(fetchCalls[0].init?.headers).toEqual({ Authorization: 'Bearer long-lived-secret' })
    expect(sockets).toHaveLength(1)
    sockets[0].open()
    const hello = decodeMessage(sockets[0].sent[0])
    expect(hello.type).toBe('hello')
    sockets[0].receive(frame('welcome', 'welcome_1', { selected_version: 1, connection_id: 'conn_1', server_epoch: 'epoch_1', heartbeat_interval_ms: 1000, max_message_bytes: 1024 }))
    expect(transport.isReady).toBe(true)
    expect(ready).toEqual(['epoch_1'])
    timers.intervals.values().next().value?.()
    expect(decodeMessage(sockets[0].sent.at(-1) ?? '').type).toBe('ping')
    sockets[0].receive(frame('ping', 'ping_server', {}))
    expect(decodeMessage(sockets[0].sent.at(-1) ?? '').type).toBe('pong')

    sockets[0].close()
    expect(transport.isReady).toBe(false)
    timers.runNextTimeout()
    await settle()
    expect(sockets).toHaveLength(2)
    sockets[1].open()
    sockets[1].receive(frame('welcome', 'welcome_2', { selected_version: 1, connection_id: 'conn_2', server_epoch: 'epoch_2', heartbeat_interval_ms: 1000, max_message_bytes: 1024 }))
    expect(ready).toEqual(['epoch_1', 'epoch_2'])
    expect(transport.serverEpoch).toBe('epoch_2')

    transport.stop()
    expect(transport.state).toBe('closed')
    expect(timers.intervals.size).toBe(0)
    expect(timers.timers.size).toBe(0)
  })

  it('cancels ticket fetch, supports repeated start/stop, and times out an incomplete handshake', async () => {
    const timers = new ManualTimers()
    const sockets: FakeSocket[] = []
    let fetchAborted = false
    let fetchCount = 0
    const transport = new WebSocketTransport({
      capabilityToken: () => 'capability',
      fetcher: async (_input, init) => {
        fetchCount += 1
        if (fetchCount === 1) {
          return new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => { fetchAborted = true; reject(new DOMException('aborted', 'AbortError')) }, { once: true })
          })
        }
        return { ok: true, json: async () => ({ ticket: `ticket_${fetchCount}` }) } as Response
      },
      websocketFactory: () => { const socket = new FakeSocket(); sockets.push(socket); return socket },
      handshakeTimeoutMS: 5,
      reconnectBaseMS: 1,
      reconnectMaxMS: 1,
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout,
      setInterval: timers.setInterval,
      clearInterval: timers.clearInterval,
    })
    transport.start()
    await settle()
    transport.stop()
    await settle()
    expect(fetchAborted).toBe(true)
    expect(timers.timers.size).toBe(0)

    transport.start()
    await settle()
    expect(sockets).toHaveLength(1)
    sockets[0].open()
    expect(timers.timers.size).toBe(1)
    timers.runNextTimeout()
    expect(sockets[0].closed).toBe(true)
    expect(transport.state).toBe('reconnecting')
    transport.stop()
    expect(timers.timers.size).toBe(0)
    expect(timers.intervals.size).toBe(0)
  })

  it('ignores an old socket, reaches a terminal reconnect bound, and isolates observers', async () => {
    const timers = new ManualTimers()
    const sockets: FakeSocket[] = []
    const transport = new WebSocketTransport({
      capabilityToken: () => 'capability',
      fetcher: async () => ({ ok: true, json: async () => ({ ticket: 'ticket' }) } as Response),
      websocketFactory: () => { const socket = new FakeSocket(); sockets.push(socket); return socket },
      maxReconnectAttempts: 1,
      reconnectBaseMS: 1,
      reconnectMaxMS: 1,
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout,
      setInterval: timers.setInterval,
      clearInterval: timers.clearInterval,
    })
    const seen: string[] = []
    transport.onMessage(() => { throw new Error('observer failure') })
    transport.onMessage((message) => seen.push(message.type))
    transport.onReady(() => { throw new Error('ready observer failure') })
    transport.start()
    await settle()
    const oldSocket = sockets[0]
    const oldMessage = oldSocket.onmessage
    const oldClose = oldSocket.onclose
    oldSocket.close()
    oldSocket.forceReceive(oldMessage, frame('welcome', 'old', { selected_version: 1, connection_id: 'old', server_epoch: 'old', heartbeat_interval_ms: 1000, max_message_bytes: 1024 }))
    oldClose?.()
    timers.runNextTimeout()
    await settle()
    expect(sockets).toHaveLength(2)
    sockets[1].open()
    sockets[1].receive(frame('welcome', 'welcome_1', { selected_version: 1, connection_id: 'conn', server_epoch: 'epoch', heartbeat_interval_ms: 1000, max_message_bytes: 1024 }))
    expect(seen).toEqual(['welcome'])

    // A listener exception cannot prevent another listener or pong handling.
    sockets[1].receive(frame('ping', 'ping_1', {}))
    expect(seen).toEqual(['welcome', 'ping'])
    expect(decodeMessage(sockets[1].sent.at(-1) ?? '').type).toBe('pong')

    sockets[1].close()
    timers.runNextTimeout()
    await settle()
    expect(sockets).toHaveLength(3)
    sockets[2].close()
    expect(transport.state).toBe('error')
    expect(timers.timers.size).toBe(0)
    transport.stop()
  })

  it('closes on binary/oversize input and routes send failure into reconnect', async () => {
    const timers = new ManualTimers()
    const sockets: FakeSocket[] = []
    const transport = new WebSocketTransport({
      capabilityToken: () => 'capability',
      fetcher: async () => ({ ok: true, json: async () => ({ ticket: 'ticket' }) } as Response),
      websocketFactory: () => { const socket = new FakeSocket(); sockets.push(socket); return socket },
      reconnectBaseMS: 1,
      reconnectMaxMS: 1,
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout,
      setInterval: timers.setInterval,
      clearInterval: timers.clearInterval,
    })
    transport.start()
    await settle()
    sockets[0].open()
    sockets[0].receive(frame('welcome', 'welcome_1', { selected_version: 1, connection_id: 'conn', server_epoch: 'epoch', heartbeat_interval_ms: 1, max_message_bytes: 32 }))
    sockets[0].receive(new Uint8Array([1, 2, 3]))
    expect(sockets[0].closed).toBe(true)
    timers.runNextTimeout()
    await settle()
    sockets[1].open()
    sockets[1].receive(frame('welcome', 'welcome_2', { selected_version: 1, connection_id: 'conn2', server_epoch: 'epoch', heartbeat_interval_ms: 1, max_message_bytes: 32 }))
    sockets[1].receive('x'.repeat(100))
    expect(sockets[1].closed).toBe(true)
    timers.runNextTimeout()
    await settle()
    sockets[2].open()
    sockets[2].receive(frame('welcome', 'welcome_3', { selected_version: 1, connection_id: 'conn3', server_epoch: 'epoch', heartbeat_interval_ms: 1, max_message_bytes: 1024 }))
    sockets[2].throwOnSend = true
    timers.intervals.values().next().value?.()
    expect(sockets[2].closed).toBe(true)
    transport.stop()
    expect(timers.timers.size).toBe(0)
    expect(timers.intervals.size).toBe(0)
  })
})
