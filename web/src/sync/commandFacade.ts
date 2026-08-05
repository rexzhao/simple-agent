import type { CommandMessage, CommandResultMessage, ErrorMessage, JsonObject, ProtocolMessage } from '../protocol/types'
import { SyncReadError } from './errors'
import type { RuntimeTransport } from './runtime'

export interface SessionMarkReadResult {
  session_id: string
  run_id: string
  marked_read: boolean
}

export type CommandErrorCode = 'invalid' | 'capacity' | 'id_generation' | 'timeout' | 'cancelled' | 'stopped' | 'transport' | string

export class CommandFacadeError extends Error {
  readonly code: CommandErrorCode
  readonly details?: unknown

  constructor(code: CommandErrorCode, message: string, details?: unknown) {
    super(message)
    this.name = 'CommandFacadeError'
    this.code = code
    this.details = details
  }
}

interface PendingCommand<T> {
  requestID: string
  sessionID: string
  runID: string
  message: CommandMessage
  crossEpochRetrySafe: boolean
  resolve: (value: T) => void
  reject: (reason: CommandFacadeError) => void
  timer: ReturnType<typeof globalThis.setTimeout>
  signal?: AbortSignal
  abortListener?: () => void
  sentGeneration?: number
  sentEpoch?: string
  accepted: boolean
}

export interface CommandFacadeOptions {
  transport: RuntimeTransport
  timeoutMS?: number
  maxPendingCommands?: number
  maxRecentRequestIDs?: number
  requestIDGenerator?: () => string
  setTimeout?: (handler: () => void, timeout: number) => ReturnType<typeof globalThis.setTimeout>
  clearTimeout?: (handle: ReturnType<typeof globalThis.setTimeout>) => void
}

function defaultRequestID(): string {
  const randomUUID = globalThis.crypto?.randomUUID?.bind(globalThis.crypto)
  if (!randomUUID) throw new Error('cryptographic request ID generation is unavailable')
  return `request_${randomUUID()}`
}

function errorFromCommand(code: string, message: string, details?: unknown): CommandFacadeError {
  return new CommandFacadeError(code, message || 'command failed', details)
}

/**
 * Typed application command boundary. A command result is only a promise
 * result; it never mutates a replica. Durable authority still arrives through
 * the resource snapshot/change stream.
 */
export class CommandFacade {
  private readonly transport: RuntimeTransport
  private readonly timeoutMS: number
  private readonly maxPendingCommands: number
  private readonly maxRecentRequestIDs: number
  private readonly requestIDGenerator: () => string
  private readonly setTimer: NonNullable<CommandFacadeOptions['setTimeout']>
  private readonly clearTimer: NonNullable<CommandFacadeOptions['clearTimeout']>
  private pending = new Map<string, PendingCommand<unknown>>()
  private recentRequestIDs = new Set<string>()
  private recentRequestIDOrder: string[] = []
  private detach: (() => void)[] = []
  private started = false

  constructor(options: CommandFacadeOptions) {
    this.transport = options.transport
    this.timeoutMS = options.timeoutMS ?? 10_000
    this.maxPendingCommands = options.maxPendingCommands ?? 128
    this.maxRecentRequestIDs = options.maxRecentRequestIDs ?? 256
    this.requestIDGenerator = options.requestIDGenerator ?? defaultRequestID
    this.setTimer = options.setTimeout ?? ((handler, timeout) => globalThis.setTimeout(handler, timeout))
    this.clearTimer = options.clearTimeout ?? ((handle) => globalThis.clearTimeout(handle))
    if (this.timeoutMS <= 0 || this.maxPendingCommands <= 0 || this.maxRecentRequestIDs <= 0) throw new Error('command bounds must be positive')
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detach = [
      this.transport.onMessage((message, generation) => this.handleMessage(message, generation)),
      this.transport.onReady((event) => this.handleReady(event.generation, event.serverEpoch, event.previousServerEpoch)),
    ]
    if (this.transport.isReady) this.handleReady(this.transport.connectionGeneration, this.transport.serverEpoch ?? '', this.transport.serverEpoch)
  }

  stop(): void {
    if (!this.started && this.pending.size === 0) return
    this.started = false
    for (const detach of this.detach.splice(0)) detach()
    for (const pending of [...this.pending.values()]) this.rejectPending(pending, new CommandFacadeError('stopped', 'command facade stopped'))
  }

  markRead(sessionID: string, runID: string, projectID?: string, options: { signal?: AbortSignal } = {}): Promise<SessionMarkReadResult> {
    const cleanSessionID = sessionID.trim()
    const cleanRunID = runID.trim()
    const cleanProjectID = projectID?.trim()
    if (!cleanSessionID || !cleanRunID || (projectID !== undefined && !cleanProjectID)) {
      return Promise.reject(new CommandFacadeError('invalid', 'session_id and run_id are required'))
    }
    if (this.pending.size >= this.maxPendingCommands) {
      return Promise.reject(new CommandFacadeError('capacity', 'too many pending commands'))
    }
    let id: string
    try {
      id = this.requestIDGenerator()
      if (typeof id !== 'string' || id.trim() === '') throw new Error('request ID is empty')
    } catch {
      return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic request ID generation failed'))
    }
    if (this.recentRequestIDs.has(id) || this.pending.has(id)) {
      return Promise.reject(new CommandFacadeError('id_generation', 'request ID collided with an active or recently used command', { collision: true }))
    }
    this.recentRequestIDs.add(id)
    this.recentRequestIDOrder.push(id)
    while (this.recentRequestIDOrder.length > this.maxRecentRequestIDs) {
      const retired = this.recentRequestIDOrder.shift()
      if (retired !== undefined) this.recentRequestIDs.delete(retired)
    }
    this.start()
    const args: JsonObject = { session_id: cleanSessionID, run_id: cleanRunID }
    if (cleanProjectID) args.project_id = cleanProjectID
    const message: CommandMessage = {
      version: 1,
      type: 'command',
      id: `command_${id}`,
      payload: {
        name: 'session.mark_read',
        schema_version: 1,
        request_id: id,
        arguments: args,
      },
    }
    return new Promise<SessionMarkReadResult>((resolve, reject) => {
      const timer = this.setTimer(() => {
        const pending = this.pending.get(id)
        if (pending) this.rejectPending(pending, new CommandFacadeError('timeout', 'command timed out'))
      }, this.timeoutMS)
      const pending: PendingCommand<SessionMarkReadResult> = {
        requestID: id,
        sessionID: cleanSessionID,
        runID: cleanRunID,
        message,
        crossEpochRetrySafe: true,
        resolve,
        reject,
        timer,
        signal: options.signal,
        accepted: false,
      }
      this.pending.set(id, pending as PendingCommand<unknown>)
      if (options.signal) {
        const abort = () => {
          const current = this.pending.get(id)
          if (current) this.rejectPending(current, new CommandFacadeError('cancelled', 'command was cancelled'))
        }
        pending.abortListener = abort
        options.signal.addEventListener('abort', abort, { once: true })
        if (options.signal.aborted) abort()
      }
      this.sendPending(pending)
    })
  }

  private handleReady(generation: number, serverEpoch: string, _previousServerEpoch?: string): void {
    if (!this.started) return
    for (const pending of [...this.pending.values()]) {
      if (pending.sentGeneration === undefined) {
        this.sendPending(pending, generation, serverEpoch)
        continue
      }
      const sameEpoch = pending.sentEpoch !== undefined && pending.sentEpoch === serverEpoch
      // The transport normally supplies previousServerEpoch, but the command
      // contract must not depend on that advisory field: a reconnect fake or
      // another transport implementation can still prove an epoch change by
      // comparing it with the epoch on which this request was sent.
      const epochChanged = pending.sentEpoch !== undefined && pending.sentEpoch !== serverEpoch
      if (sameEpoch || (epochChanged && pending.crossEpochRetrySafe)) this.sendPending(pending, generation, serverEpoch)
    }
  }

  private sendPending<T>(pending: PendingCommand<T>, generation = this.transport.connectionGeneration, epoch = this.transport.serverEpoch ?? ''): void {
    if (!this.transport.isReady || !this.pending.has(pending.requestID)) return
    pending.sentGeneration = generation
    pending.sentEpoch = epoch
    try {
      this.transport.send(pending.message)
    } catch (reason) {
      pending.sentGeneration = undefined
      pending.sentEpoch = undefined
      if (reason instanceof SyncReadError && reason.code === 'protocol') this.rejectPending(pending, new CommandFacadeError('transport', 'command could not be sent'))
    }
  }

  private handleMessage(message: ProtocolMessage, generation: number): void {
    if (message.type === 'command_accepted') {
      const pending = this.pending.get(message.payload.request_id)
      if (pending && pending.sentGeneration === generation) pending.accepted = true
      return
    }
    if (message.type === 'command_result') {
      this.handleResult(message as CommandResultMessage, generation)
      return
    }
    if (message.type === 'error' && message.payload.request_id) {
      const pending = this.pending.get(message.payload.request_id)
      if (pending && pending.sentGeneration === generation) {
        const error = message as ErrorMessage
        this.rejectPending(pending, errorFromCommand(error.payload.code, error.payload.message, error.payload.details))
      }
    }
  }

  private handleResult(message: CommandResultMessage, generation: number): void {
    const pending = this.pending.get(message.payload.request_id)
    if (!pending || pending.sentGeneration !== generation) return
    if (message.payload.status === 'failed') {
      const error = message.payload.error
      this.rejectPending(pending, errorFromCommand(error?.code ?? 'command_failed', error?.message ?? 'command failed', error?.details))
      return
    }
    const result = message.payload.result
    if (!result || typeof result !== 'object' || Array.isArray(result)) {
      this.rejectPending(pending, new CommandFacadeError('invalid', 'command result was invalid'))
      return
    }
    const value = result as Record<string, unknown>
    if (typeof value.session_id !== 'string' || typeof value.run_id !== 'string' || typeof value.marked_read !== 'boolean') {
      this.rejectPending(pending, new CommandFacadeError('invalid', 'session.mark_read result was invalid'))
      return
    }
    if (value.session_id !== pending.sessionID || value.run_id !== pending.runID) {
      this.rejectPending(pending, new CommandFacadeError('invalid', 'session.mark_read result did not match its request'))
      return
    }
    this.resolvePending(pending, {
      session_id: value.session_id,
      run_id: value.run_id,
      marked_read: value.marked_read,
    })
  }

  private resolvePending(pending: PendingCommand<unknown>, value: unknown): void {
    if (!this.pending.delete(pending.requestID)) return
    this.clearTimer(pending.timer)
    if (pending.signal && pending.abortListener) pending.signal.removeEventListener('abort', pending.abortListener)
    pending.resolve(value)
  }

  private rejectPending<T>(pending: PendingCommand<T>, error: CommandFacadeError): void {
    if (!this.pending.delete(pending.requestID)) return
    this.clearTimer(pending.timer)
    if (pending.signal && pending.abortListener) pending.signal.removeEventListener('abort', pending.abortListener)
    pending.reject(error)
  }
}
