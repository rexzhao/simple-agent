import type { CommandMessage, CommandResultMessage, ErrorMessage, JsonObject, ProtocolMessage } from '../protocol/types'
import type {
  CommandOptions,
  SessionArchiveResult,
  SessionCommands,
  SessionDebugResult,
  SessionFullAccessResult,
  SessionMarkReadResult,
  SessionRenameResult,
} from '../commands/sessionCommands'
import type { RunCancelResult, RunCommands, RunStatus } from '../commands/runCommands'
import { SyncReadError } from './errors'
import type { RuntimeTransport } from './runtime'

// Keep the original result type import path available while the typed command
// contracts live under the page-independent commands boundary.
export type { SessionMarkReadResult } from '../commands/sessionCommands'

export type CommandErrorCode = 'invalid' | 'capacity' | 'id_generation' | 'timeout' | 'cancelled' | 'stopped' | 'transport' | 'outcome_unknown' | string

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
  message: CommandMessage
  crossEpochRetrySafe: boolean
  decodeResult: (value: unknown) => T
  resolve: (value: T) => void
  reject: (reason: CommandFacadeError) => void
  timer: ReturnType<typeof globalThis.setTimeout>
  signal?: AbortSignal
  abortListener?: () => void
  sentGeneration?: number
  sentEpoch?: string
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

function nonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim() !== ''
}

function exactObject(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('result is not an object')
  const object = value as Record<string, unknown>
  const actual = Object.keys(object)
  if (actual.length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(object, key))) throw new Error('result has an unexpected shape')
  return object
}

function resultString(object: Record<string, unknown>, key: string): string {
  const value = object[key]
  if (!nonEmptyString(value)) throw new Error('result string is invalid')
  return value
}

function resultBoolean(object: Record<string, unknown>, key: string): boolean {
  if (typeof object[key] !== 'boolean') throw new Error('result boolean is invalid')
  return object[key] as boolean
}

function decodeMarkReadResult(value: unknown, sessionID: string, runID: string): SessionMarkReadResult {
  const object = exactObject(value, ['session_id', 'run_id', 'marked_read'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  if (resultSessionID !== sessionID || resultRunID !== runID) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, marked_read: resultBoolean(object, 'marked_read') }
}

function decodeRenameResult(value: unknown, sessionID: string, displayName: string): SessionRenameResult {
  const object = exactObject(value, ['session_id', 'display_name'])
  const resultSessionID = resultString(object, 'session_id')
  const resultDisplayName = resultString(object, 'display_name')
  if (resultSessionID !== sessionID || resultDisplayName !== displayName) throw new Error('result does not match request')
  return { session_id: resultSessionID, display_name: resultDisplayName }
}

function decodeArchiveResult(value: unknown, sessionID: string, archived: boolean): SessionArchiveResult {
  const object = exactObject(value, ['session_id', 'archived'])
  const resultSessionID = resultString(object, 'session_id')
  if (resultSessionID !== sessionID || resultBoolean(object, 'archived') !== archived) throw new Error('result does not match request')
  return { session_id: resultSessionID, archived }
}

function decodeFullAccessResult(value: unknown, sessionID: string, fullAccess: boolean): SessionFullAccessResult {
  const object = exactObject(value, ['session_id', 'full_access'])
  const resultSessionID = resultString(object, 'session_id')
  if (resultSessionID !== sessionID || resultBoolean(object, 'full_access') !== fullAccess) throw new Error('result does not match request')
  return { session_id: resultSessionID, full_access: fullAccess }
}

function decodeDebugResult(value: unknown, sessionID: string, requestBodies: boolean): SessionDebugResult {
  const object = exactObject(value, ['session_id', 'request_bodies'])
  const resultSessionID = resultString(object, 'session_id')
  if (resultSessionID !== sessionID || resultBoolean(object, 'request_bodies') !== requestBodies) throw new Error('result does not match request')
  return { session_id: resultSessionID, request_bodies: requestBodies }
}

function decodeRunCancelResult(value: unknown, runID: string): RunCancelResult {
  const object = exactObject(value, ['run_id', 'status'])
  const resultRunID = resultString(object, 'run_id')
  const status = object.status
  const statuses: RunStatus[] = ['running', 'committed', 'failed', 'cancelled']
  if (resultRunID !== runID || typeof status !== 'string' || !statuses.includes(status as RunStatus)) throw new Error('result does not match request')
  return { run_id: resultRunID, status: status as RunStatus }
}

/**
 * Typed application command boundary. A command result is only a promise
 * result; it never mutates a replica. Durable authority still arrives through
 * the resource snapshot/change stream.
 */
export class CommandFacade implements SessionCommands, RunCommands {
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

  markRead(sessionID: string, runID: string, projectID?: string, options: CommandOptions = {}): Promise<SessionMarkReadResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    const cleanProjectID = projectID === undefined ? undefined : this.cleanID(projectID)
    if (!cleanSessionID || !cleanRunID || (projectID !== undefined && !cleanProjectID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id and run_id are required'))
    const args: JsonObject = { session_id: cleanSessionID, run_id: cleanRunID }
    if (cleanProjectID) args.project_id = cleanProjectID
    return this.submit('session.mark_read', args, true, (value) => decodeMarkReadResult(value, cleanSessionID, cleanRunID), options)
  }

  rename(sessionID: string, displayName: string, options: CommandOptions = {}): Promise<SessionRenameResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanDisplayName = this.cleanID(displayName)
    if (!cleanSessionID || !cleanDisplayName) return Promise.reject(new CommandFacadeError('invalid', 'session_id and display_name are required'))
    return this.submit('session.rename', { session_id: cleanSessionID, display_name: cleanDisplayName }, true, (value) => decodeRenameResult(value, cleanSessionID, cleanDisplayName), options)
  }

  archive(sessionID: string, options: CommandOptions = {}): Promise<SessionArchiveResult> {
    return this.submitSessionToggle('session.archive', sessionID, true, options)
  }

  restore(sessionID: string, options: CommandOptions = {}): Promise<SessionArchiveResult> {
    return this.submitSessionToggle('session.restore', sessionID, false, options)
  }

  setFullAccess(sessionID: string, fullAccess: boolean, options: CommandOptions = {}): Promise<SessionFullAccessResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID || typeof fullAccess !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'session_id and full_access are required'))
    return this.submit('session.set_full_access', { session_id: cleanSessionID, full_access: fullAccess }, true, (value) => decodeFullAccessResult(value, cleanSessionID, fullAccess), options)
  }

  setDebug(sessionID: string, requestBodies: boolean, options: CommandOptions = {}): Promise<SessionDebugResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID || typeof requestBodies !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'session_id and request_bodies are required'))
    return this.submit('session.set_debug', { session_id: cleanSessionID, request_bodies: requestBodies }, true, (value) => decodeDebugResult(value, cleanSessionID, requestBodies), options)
  }

  cancelRun(runID: string, options: CommandOptions = {}): Promise<RunCancelResult> {
    const cleanRunID = this.cleanID(runID)
    if (!cleanRunID) return Promise.reject(new CommandFacadeError('invalid', 'run_id is required'))
    return this.submit('run.cancel', { run_id: cleanRunID }, false, (value) => decodeRunCancelResult(value, cleanRunID), options)
  }

  private submitSessionToggle(name: 'session.archive' | 'session.restore', sessionID: string, archived: boolean, options: CommandOptions): Promise<SessionArchiveResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID) return Promise.reject(new CommandFacadeError('invalid', 'session_id is required'))
    return this.submit(name, { session_id: cleanSessionID }, true, (value) => decodeArchiveResult(value, cleanSessionID, archived), options)
  }

  private cleanID(value: string): string {
    return typeof value === 'string' ? value.trim() : ''
  }

  private submit<T>(name: string, args: JsonObject, crossEpochRetrySafe: boolean, decodeResult: (value: unknown) => T, options: CommandOptions): Promise<T> {
    if (this.pending.size >= this.maxPendingCommands) return Promise.reject(new CommandFacadeError('capacity', 'too many pending commands'))
    let id: string
    try {
      id = this.requestIDGenerator()
      if (typeof id !== 'string' || id.trim() === '') throw new Error('request ID is empty')
    } catch {
      return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic request ID generation failed'))
    }
    if (this.recentRequestIDs.has(id) || this.pending.has(id)) return Promise.reject(new CommandFacadeError('id_generation', 'request ID collided with an active or recently used command', { collision: true }))
    this.rememberRequestID(id)
    this.start()
    const message: CommandMessage = {
      version: 1,
      type: 'command',
      id: `command_${id}`,
      payload: { name, schema_version: 1, request_id: id, arguments: args },
    }
    return new Promise<T>((resolve, reject) => {
      const timer = this.setTimer(() => {
        const pending = this.pending.get(id)
        if (pending) this.rejectPending(pending, new CommandFacadeError('timeout', 'command timed out'))
      }, this.timeoutMS)
      const pending: PendingCommand<T> = { requestID: id, message, crossEpochRetrySafe, decodeResult, resolve, reject, timer, signal: options.signal }
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

  private rememberRequestID(id: string): void {
    this.recentRequestIDs.add(id)
    this.recentRequestIDOrder.push(id)
    while (this.recentRequestIDOrder.length > this.maxRecentRequestIDs) {
      const retired = this.recentRequestIDOrder.shift()
      if (retired !== undefined) this.recentRequestIDs.delete(retired)
    }
  }

  private handleReady(generation: number, serverEpoch: string, _previousServerEpoch?: string): void {
    if (!this.started) return
    for (const pending of [...this.pending.values()]) {
      if (pending.sentGeneration === undefined) {
        this.sendPending(pending, generation, serverEpoch)
        continue
      }
      const sameEpoch = pending.sentEpoch !== undefined && pending.sentEpoch === serverEpoch
      const epochChanged = pending.sentEpoch !== undefined && pending.sentEpoch !== serverEpoch
      if (sameEpoch || (epochChanged && pending.crossEpochRetrySafe)) {
        this.sendPending(pending, generation, serverEpoch)
      } else if (epochChanged) {
        this.rejectPending(pending, new CommandFacadeError('outcome_unknown', 'command outcome is unknown after the server epoch changed'))
      }
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
    if (message.type === 'command_accepted') return
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
    try {
      this.resolvePending(pending, pending.decodeResult(message.payload.result))
    } catch {
      this.rejectPending(pending, new CommandFacadeError('invalid', 'command result was invalid'))
    }
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
