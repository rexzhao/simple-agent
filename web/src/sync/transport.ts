import { decodeMessage } from '../protocol/decode'
import { encodeMessage as encodeProtocolMessage } from '../protocol/encode'
import { ProtocolDecodeError } from '../protocol/errors'
import type {
  HelloMessage,
  MessageType,
  PingMessage,
  PongMessage,
  ProtocolMessage,
  WelcomeMessage,
} from '../protocol/types'
import { SyncReadError } from './errors'

export type TransportState = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'closed' | 'error'

export interface SocketLike {
  readonly readyState?: number
  onopen: (() => void) | null
  onmessage: ((event: { data: unknown }) => void) | null
  onerror: (() => void) | null
  onclose: (() => void) | null
  send(data: string): void
  close(code?: number, reason?: string): void
}

export interface TransportReadyEvent {
  generation: number
  serverEpoch: string
  previousServerEpoch?: string
  connectionID: string
  heartbeatIntervalMS: number
  maxMessageBytes: number
}

export interface TransportCloseEvent {
  generation: number
  willRetry: boolean
}

export interface WebSocketTransportOptions {
  ticketPath?: string
  websocketPath?: string
  clientID?: string
  capabilityToken?: () => string
  fetcher?: typeof globalThis.fetch
  websocketFactory?: (url: string) => SocketLike
  baseURL?: string
  maxReconnectAttempts?: number
  reconnectBaseMS?: number
  reconnectMaxMS?: number
  handshakeTimeoutMS?: number
  setTimeout?: (handler: () => void, timeout: number) => ReturnType<typeof globalThis.setTimeout>
  clearTimeout?: (handle: ReturnType<typeof globalThis.setTimeout>) => void
  setInterval?: (handler: () => void, timeout: number) => ReturnType<typeof globalThis.setInterval>
  clearInterval?: (handle: ReturnType<typeof globalThis.setInterval>) => void
}

type MessageListener = (message: ProtocolMessage, connectionGeneration: number) => void
type ReadyListener = (event: TransportReadyEvent) => void
type CloseListener = (event: TransportCloseEvent) => void
type StateListener = (state: TransportState) => void

let localID = 0

function defaultCapabilityToken(): string {
  if (typeof window === 'undefined') return ''
  const hash = new URLSearchParams(window.location.hash.slice(1))
  const fromHash = hash.get('token')
  if (fromHash) {
    window.sessionStorage.setItem('sai-capability-token', fromHash)
    window.history.replaceState(null, '', window.location.pathname + window.location.search)
    return fromHash
  }
  return window.sessionStorage.getItem('sai-capability-token') ?? ''
}

function defaultClientID(): string {
  const randomUUID = globalThis.crypto?.randomUUID?.bind(globalThis.crypto)
  if (randomUUID) return `tab_${randomUUID()}`
  localID += 1
  return `tab_${localID}`
}

function defaultWebSocketURL(pathname: string, baseURL?: string): string {
  if (baseURL) {
    const url = new URL(pathname, baseURL)
    if (url.protocol === 'http:') url.protocol = 'ws:'
    if (url.protocol === 'https:') url.protocol = 'wss:'
    return url.toString()
  }
  if (typeof window !== 'undefined') {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    return `${protocol}//${window.location.host}${pathname}`
  }
  return pathname
}

function makeEnvelope<T extends MessageType>(type: T, id: string, payload: unknown): { version: 1; type: T; id: string; payload: unknown } {
  return { version: 1, type, id, payload }
}

function makeID(prefix: string): string {
  localID += 1
  return `${prefix}_${localID}`
}

function safeSocketFactory(url: string): SocketLike {
  if (typeof WebSocket === 'undefined') throw new SyncReadError('transport', 'WebSocket is unavailable')
  return new WebSocket(url) as unknown as SocketLike
}

/**
 * The only browser WebSocket client used by the C2 modules. It owns ticket
 * acquisition, the one live socket, protocol decoding, heartbeat and bounded
 * reconnect scheduling. It never puts the long-lived capability in the WS
 * URL and never logs ticket-bearing URLs.
 */
export class WebSocketTransport {
  private readonly ticketPath: string
  private readonly websocketPath: string
  private readonly clientID: string
  private readonly capabilityToken: () => string
  private readonly fetcher: typeof globalThis.fetch
  private readonly websocketFactory: (url: string) => SocketLike
  private readonly baseURL?: string
  private readonly maxReconnectAttempts: number
  private readonly reconnectBaseMS: number
  private readonly reconnectMaxMS: number
  private readonly handshakeTimeoutMS: number
  private readonly setTimer: NonNullable<WebSocketTransportOptions['setTimeout']>
  private readonly clearTimer: NonNullable<WebSocketTransportOptions['clearTimeout']>
  private readonly setTicker: NonNullable<WebSocketTransportOptions['setInterval']>
  private readonly clearTicker: NonNullable<WebSocketTransportOptions['clearInterval']>

  private stateValue: TransportState = 'idle'
  private running = false
  private socket: SocketLike | null = null
  private socketGeneration = 0
  private attempt = 0
  private reconnectTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private handshakeTimer: ReturnType<typeof globalThis.setTimeout> | null = null
  private heartbeatTimer: ReturnType<typeof globalThis.setInterval> | null = null
  private ticketAbort: AbortController | null = null
  private ready = false
  private serverEpochValue: string | undefined
  private heartbeatIntervalMS = 15_000
  private maxMessageBytes = 256 * 1024
  private messageListeners = new Set<MessageListener>()
  private readyListeners = new Set<ReadyListener>()
  private closeListeners = new Set<CloseListener>()
  private stateListeners = new Set<StateListener>()

  constructor(options: WebSocketTransportOptions = {}) {
    this.ticketPath = options.ticketPath ?? '/api/ws-ticket'
    this.websocketPath = options.websocketPath ?? '/api/ws'
    this.clientID = options.clientID ?? defaultClientID()
    this.capabilityToken = options.capabilityToken ?? defaultCapabilityToken
    this.fetcher = options.fetcher ?? globalThis.fetch.bind(globalThis)
    this.websocketFactory = options.websocketFactory ?? safeSocketFactory
    this.baseURL = options.baseURL
    this.maxReconnectAttempts = options.maxReconnectAttempts ?? 8
    this.reconnectBaseMS = options.reconnectBaseMS ?? 250
    this.reconnectMaxMS = options.reconnectMaxMS ?? 8_000
    this.handshakeTimeoutMS = options.handshakeTimeoutMS ?? 10_000
    this.setTimer = options.setTimeout ?? ((handler, timeout) => globalThis.setTimeout(handler, timeout))
    this.clearTimer = options.clearTimeout ?? ((handle) => globalThis.clearTimeout(handle))
    this.setTicker = options.setInterval ?? ((handler, timeout) => globalThis.setInterval(handler, timeout))
    this.clearTicker = options.clearInterval ?? ((handle) => globalThis.clearInterval(handle))
    if (this.maxReconnectAttempts < 0 || this.reconnectBaseMS < 0 || this.reconnectMaxMS < 0 || this.handshakeTimeoutMS <= 0) {
      throw new Error('invalid WebSocket transport bounds')
    }
  }

  get state(): TransportState { return this.stateValue }
  get isReady(): boolean { return this.ready }
  get serverEpoch(): string | undefined { return this.serverEpochValue }
  get connectionGeneration(): number { return this.socketGeneration }

  onMessage(listener: MessageListener): () => void {
    this.messageListeners.add(listener)
    return () => this.messageListeners.delete(listener)
  }

  onReady(listener: ReadyListener): () => void {
    this.readyListeners.add(listener)
    return () => this.readyListeners.delete(listener)
  }

  onClose(listener: CloseListener): () => void {
    this.closeListeners.add(listener)
    return () => this.closeListeners.delete(listener)
  }

  onStateChange(listener: StateListener): () => void {
    this.stateListeners.add(listener)
    return () => this.stateListeners.delete(listener)
  }

  start(): void {
    if (this.running) return
    this.running = true
    this.attempt = 0
    this.setState('connecting')
    void this.connect()
  }

  stop(): void {
    if (!this.running && this.stateValue === 'closed') return
    this.running = false
    this.ready = false
    this.clearReconnectTimer()
    this.clearHandshakeTimer()
    this.clearHeartbeatTimer()
    this.ticketAbort?.abort()
    this.ticketAbort = null
    const socket = this.socket
    this.socket = null
    this.detachSocket(socket)
    try { socket?.close(1000, 'client stopped') } catch { /* socket is already closed */ }
    this.setState('closed')
  }

  send(message: ProtocolMessage): void {
    const socket = this.socket
    if (!this.ready || !socket) throw new SyncReadError('transport', 'WebSocket is not ready')
    let encoded: string
    try {
      encoded = encodeProtocolMessage(message)
    } catch {
      throw new SyncReadError('protocol', 'message could not be encoded')
    }
    const frameBytes = typeof TextEncoder === 'undefined' ? encoded.length : new TextEncoder().encode(encoded).byteLength
    if (frameBytes > this.maxMessageBytes) throw new SyncReadError('protocol', 'message exceeds the server limit')
    try {
      socket.send(encoded)
    } catch {
      this.failSocketSend(socket)
      throw new SyncReadError('transport', 'WebSocket send failed')
    }
  }

  private async connect(): Promise<void> {
    if (!this.running) return
    const generation = ++this.socketGeneration
    this.ready = false
    // Negotiated limits belong to the previous socket.  A new handshake must
    // be decodable before a server can advertise its next limit; otherwise a
    // reconnect after a small max_message_bytes negotiation could reject the
    // welcome frame itself.
    this.maxMessageBytes = 256 * 1024
    this.heartbeatIntervalMS = 15_000
    this.setState(this.attempt === 0 ? 'connecting' : 'reconnecting')
    const controller = new AbortController()
    this.ticketAbort?.abort()
    this.ticketAbort = controller
    let ticket: string
    try {
      const response = await this.fetchTicket(controller.signal)
      ticket = response
    } catch (reason) {
      if (!this.running || controller.signal.aborted) return
      this.connectFailed(generation, reason)
      return
    } finally {
      if (this.ticketAbort === controller) this.ticketAbort = null
    }
    if (!this.running || generation !== this.socketGeneration) return
    const url = this.websocketURL(ticket)
    let socket: SocketLike
    try {
      socket = this.websocketFactory(url)
    } catch (reason) {
      this.connectFailed(generation, reason)
      return
    }
    this.socket = socket
    socket.onopen = () => {
      if (!this.running || generation !== this.socketGeneration || this.socket !== socket) return
      this.clearHandshakeTimer()
      this.handshakeTimer = this.setTimer(() => {
        if (this.socket === socket && !this.ready) {
          try { socket.close(1002, 'handshake timeout') } catch { /* no-op */ }
        }
      }, this.handshakeTimeoutMS)
      const hello: HelloMessage = {
        ...makeEnvelope('hello', makeID('hello'), {
          supported_versions: [1],
          client_id: this.clientID,
        }),
      } as HelloMessage
      this.sendRaw(hello)
    }
    socket.onmessage = (event) => this.receive(socket, generation, event.data)
    socket.onerror = () => {
      if (this.socket === socket && this.running) {
        this.setState('reconnecting')
        try { socket.close(1011, 'socket error') } catch { /* close callback owns recovery */ }
      }
    }
    socket.onclose = () => this.closed(socket, generation)
  }

  private async fetchTicket(signal: AbortSignal): Promise<string> {
    const capability = this.capabilityToken()
    if (!capability) throw new SyncReadError('transport', 'capability token is unavailable')
    let response: Response
    try {
      response = await this.fetcher(this.ticketURL(), {
        method: 'POST',
        headers: { Authorization: `Bearer ${capability}` },
        signal,
      })
    } catch {
      throw new SyncReadError('transport', 'WebSocket ticket request failed')
    }
    if (!response.ok) throw new SyncReadError('transport', 'WebSocket ticket request was rejected')
    let body: unknown
    try { body = await response.json() } catch { throw new SyncReadError('protocol', 'WebSocket ticket response was invalid') }
    if (!body || typeof body !== 'object' || Array.isArray(body)) throw new SyncReadError('protocol', 'WebSocket ticket response was invalid')
    const ticket = (body as { ticket?: unknown }).ticket
    if (typeof ticket !== 'string' || ticket.trim() === '') throw new SyncReadError('protocol', 'WebSocket ticket response was invalid')
    return ticket
  }

  private ticketURL(): string {
    if (!this.baseURL) return this.ticketPath
    return new URL(this.ticketPath, this.baseURL).toString()
  }

  private websocketURL(ticket: string): string {
    const separator = this.websocketPath.includes('?') ? '&' : '?'
    return `${defaultWebSocketURL(this.websocketPath, this.baseHTTPURL())}${separator}ticket=${encodeURIComponent(ticket)}`
  }

  private baseHTTPURL(): string | undefined {
    return this.baseURL
  }

  private receive(socket: SocketLike, generation: number, data: unknown): void {
    if (!this.running || this.socket !== socket || generation !== this.socketGeneration) return
    if (typeof data !== 'string') {
      this.protocolFailure(socket)
      return
    }
    const frameBytes = typeof TextEncoder === 'undefined' ? data.length : new TextEncoder().encode(data).byteLength
    if (frameBytes > this.maxMessageBytes) {
      this.protocolFailure(socket)
      return
    }
    let message: ProtocolMessage
    try {
      message = decodeMessage(data)
    } catch (reason) {
      this.protocolFailure(socket, reason)
      return
    }
    if (message.type === 'ping') {
      const pong: PongMessage = {
        ...makeEnvelope('pong', makeID('pong'), {}),
      } as PongMessage
      this.sendRaw(pong)
    }
    if (message.type === 'welcome') {
      if (!this.acceptWelcome(message as WelcomeMessage, generation)) return
    }
    this.notify(this.messageListeners, (listener) => listener(message, generation))
  }

  private acceptWelcome(message: WelcomeMessage, generation: number): boolean {
    if (generation !== this.socketGeneration || !this.socket) return false
    if (this.ready) {
      this.protocolFailure(this.socket)
      return false
    }
    this.clearHandshakeTimer()
    this.ready = true
    this.attempt = 0
    const previousServerEpoch = this.serverEpochValue
    this.serverEpochValue = message.payload.server_epoch
    this.heartbeatIntervalMS = message.payload.heartbeat_interval_ms
    this.maxMessageBytes = message.payload.max_message_bytes
    this.setState('open')
    this.startHeartbeat(generation)
    const event: TransportReadyEvent = {
      generation,
      serverEpoch: message.payload.server_epoch,
      previousServerEpoch,
      connectionID: message.payload.connection_id,
      heartbeatIntervalMS: message.payload.heartbeat_interval_ms,
      maxMessageBytes: message.payload.max_message_bytes,
    }
    this.notify(this.readyListeners, (listener) => listener(event))
    return true
  }

  private startHeartbeat(generation: number): void {
    this.clearHeartbeatTimer()
    this.heartbeatTimer = this.setTicker(() => {
      if (!this.running || !this.ready || generation !== this.socketGeneration) return
      const ping: PingMessage = { ...makeEnvelope('ping', makeID('ping'), {}) } as PingMessage
      this.sendRaw(ping)
    }, this.heartbeatIntervalMS)
  }

  private sendRaw(message: ProtocolMessage): void {
    const socket = this.socket
    if (!socket) return
    let encoded: string
    try { encoded = encodeProtocolMessage(message) } catch { return }
    const frameBytes = typeof TextEncoder === 'undefined' ? encoded.length : new TextEncoder().encode(encoded).byteLength
    if (frameBytes > this.maxMessageBytes) {
      this.protocolFailure(socket)
      return
    }
    try {
      socket.send(encoded)
    } catch {
      // A failed heartbeat/pong/hello must not leave a transport which still
      // claims to be ready. Closing the current socket funnels all failures
      // through the same bounded reconnect path as an ordinary close.
      this.failSocketSend(socket)
    }
  }

  private failSocketSend(socket: SocketLike): void {
    if (!this.running || this.socket !== socket) return
    this.ready = false
    this.clearHandshakeTimer()
    this.clearHeartbeatTimer()
    this.setState('reconnecting')
    try {
      socket.close(1011, 'socket send failed')
    } catch {
      // A broken socket may throw without dispatching close.  Run the same
      // bounded recovery path explicitly, while generation checks make a
      // later close event harmless.
      this.closed(socket, this.socketGeneration)
    }
  }

  private protocolFailure(socket: SocketLike, reason?: unknown): void {
    if (reason instanceof ProtocolDecodeError) {
      // Keep diagnostics to a stable code only. Never retain the input frame.
      void reason.code
    }
    try { socket.close(1002, 'protocol error') } catch { /* no-op */ }
  }

  private closed(socket: SocketLike, generation: number): void {
    if (this.socket !== socket || generation !== this.socketGeneration) return
    this.detachSocket(socket)
    this.socket = null
    const wasReady = this.ready
    this.ready = false
    this.clearHandshakeTimer()
    this.clearHeartbeatTimer()
    if (!this.running) {
      this.setState('closed')
      this.notify(this.closeListeners, (listener) => listener({ generation, willRetry: false }))
      return
    }
    this.setState('reconnecting')
    this.scheduleReconnect(generation, wasReady)
  }

  private connectFailed(_generation: number, _reason: unknown): void {
    if (!this.running) return
    this.ready = false
    this.clearHandshakeTimer()
    this.clearHeartbeatTimer()
    this.setState('reconnecting')
    this.scheduleReconnect(this.socketGeneration, false)
  }

  private scheduleReconnect(generation: number, _wasReady: boolean): void {
    if (!this.running || generation !== this.socketGeneration) return
    this.attempt += 1
    if (this.attempt > this.maxReconnectAttempts) {
      this.running = false
      this.ready = false
      this.clearReconnectTimer()
      this.clearHandshakeTimer()
      this.clearHeartbeatTimer()
      this.setState('error')
      this.notify(this.closeListeners, (listener) => listener({ generation, willRetry: false }))
      return
    }
    const delay = Math.min(this.reconnectBaseMS * (2 ** Math.max(0, this.attempt - 1)), this.reconnectMaxMS)
    this.clearReconnectTimer()
    this.reconnectTimer = this.setTimer(() => {
      this.reconnectTimer = null
      void this.connect()
    }, delay)
    this.notify(this.closeListeners, (listener) => listener({ generation, willRetry: true }))
  }

  private setState(next: TransportState): void {
    if (this.stateValue === next) return
    this.stateValue = next
    this.notify(this.stateListeners, (listener) => listener(next))
  }

  private notify<T>(listeners: Set<T>, invoke: (listener: T) => void): void {
    for (const listener of [...listeners]) {
      try { invoke(listener) } catch { /* observers cannot break transport recovery */ }
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) this.clearTimer(this.reconnectTimer)
    this.reconnectTimer = null
  }

  private clearHandshakeTimer(): void {
    if (this.handshakeTimer !== null) this.clearTimer(this.handshakeTimer)
    this.handshakeTimer = null
  }

  private clearHeartbeatTimer(): void {
    if (this.heartbeatTimer !== null) this.clearTicker(this.heartbeatTimer)
    this.heartbeatTimer = null
  }

  private detachSocket(socket: SocketLike | null): void {
    if (!socket) return
    socket.onopen = null
    socket.onmessage = null
    socket.onerror = null
    socket.onclose = null
  }
}
