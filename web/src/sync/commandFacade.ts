import type { CommandMessage, CommandResultMessage, ErrorMessage, JsonObject, ProtocolMessage } from '../protocol/types'
import type {
  CommandOptions,
  SessionArchiveResult,
  SessionCommands,
  SessionDebugResult,
  SessionFullAccessResult,
  SessionMarkReadResult,
  SessionRenameResult,
  SessionCreateOptions,
  SessionCreateResult,
} from '../commands/sessionCommands'
import type { RunCancelResult, RunCommands, RunStartOptions, RunStartResult, RunStatus } from '../commands/runCommands'
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
  maxRecentEntityIDs?: number
  requestIDGenerator?: () => string
  sessionIDGenerator?: () => string
  runIDGenerator?: () => string
  setTimeout?: (handler: () => void, timeout: number) => ReturnType<typeof globalThis.setTimeout>
  clearTimeout?: (handle: ReturnType<typeof globalThis.setTimeout>) => void
}

function defaultRequestID(): string {
  const randomUUID = globalThis.crypto?.randomUUID?.bind(globalThis.crypto)
  if (!randomUUID) throw new Error('cryptographic request ID generation is unavailable')
  return `request_${randomUUID()}`
}

function defaultSessionID(): string {
  const randomUUID = globalThis.crypto?.randomUUID?.bind(globalThis.crypto)
  if (!randomUUID) throw new Error('cryptographic session ID generation is unavailable')
  return `session_${randomUUID()}`
}

function defaultRunID(): string {
  const randomUUID = globalThis.crypto?.randomUUID?.bind(globalThis.crypto)
  if (!randomUUID) throw new Error('cryptographic run ID generation is unavailable')
  return `run_${randomUUID()}`
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

function decodeCreateResult(value: unknown, sessionID: string, projectID: string): SessionCreateResult {
  const object = exactObject(value, ['session_id', 'project_id'])
  const resultSessionID = resultString(object, 'session_id')
  const resultProjectID = resultString(object, 'project_id')
  if (resultSessionID !== sessionID || resultProjectID !== projectID) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, project_id: resultProjectID }
}

function decodeRunCancelResult(value: unknown, runID: string): RunCancelResult {
  const object = exactObject(value, ['run_id', 'status'])
  const resultRunID = resultString(object, 'run_id')
  const status = object.status
  const statuses: RunStatus[] = ['running', 'committed', 'failed', 'interrupted', 'cancelled']
  if (resultRunID !== runID || typeof status !== 'string' || !statuses.includes(status as RunStatus)) throw new Error('result does not match request')
  return { run_id: resultRunID, status: status as RunStatus }
}

function decodeRunStartResult(value: unknown, sessionID: string, runID: string): RunStartResult {
  const object = exactObject(value, ['session_id', 'run_id', 'status'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const status = object.status
  const statuses: RunStatus[] = ['running', 'committed', 'failed', 'interrupted', 'cancelled']
  if (resultSessionID !== sessionID || resultRunID !== runID || typeof status !== 'string' || !statuses.includes(status as RunStatus)) throw new Error('result does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, status: status as RunStatus }
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
  private readonly maxRecentEntityIDs: number
  private readonly requestIDGenerator: () => string
  private readonly sessionIDGenerator: () => string
  private readonly runIDGenerator: () => string
  private readonly setTimer: NonNullable<CommandFacadeOptions['setTimeout']>
  private readonly clearTimer: NonNullable<CommandFacadeOptions['clearTimeout']>
  private pending = new Map<string, PendingCommand<unknown>>()
  private recentRequestIDs = new Set<string>()
  private recentRequestIDOrder: string[] = []
  private recentEntityIDs = new Set<string>()
  private recentEntityIDOrder: string[] = []
  private recentRunIDs = new Set<string>()
  private recentRunIDOrder: string[] = []
  private detach: (() => void)[] = []
  private started = false

  constructor(options: CommandFacadeOptions) {
    this.transport = options.transport
    this.timeoutMS = options.timeoutMS ?? 10_000
    this.maxPendingCommands = options.maxPendingCommands ?? 128
    this.maxRecentRequestIDs = options.maxRecentRequestIDs ?? 256
    this.maxRecentEntityIDs = options.maxRecentEntityIDs ?? 256
    this.requestIDGenerator = options.requestIDGenerator ?? defaultRequestID
    this.sessionIDGenerator = options.sessionIDGenerator ?? defaultSessionID
    this.runIDGenerator = options.runIDGenerator ?? defaultRunID
    this.setTimer = options.setTimeout ?? ((handler, timeout) => globalThis.setTimeout(handler, timeout))
    this.clearTimer = options.clearTimeout ?? ((handle) => globalThis.clearTimeout(handle))
    if (this.timeoutMS <= 0 || this.maxPendingCommands <= 0 || this.maxRecentRequestIDs <= 0 || this.maxRecentEntityIDs <= 0) throw new Error('command bounds must be positive')
  }

  private ensureStarted(): void {
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

  create(projectID: string, options: SessionCreateOptions = {}, commandOptions: CommandOptions = {}): Promise<SessionCreateResult> {
    const cleanProjectID = this.cleanID(projectID)
    if (!cleanProjectID || cleanProjectID.length > 128 || !/^[A-Za-z0-9_.-]+$/.test(cleanProjectID) || cleanProjectID === '.' || cleanProjectID === '..') return Promise.reject(new CommandFacadeError('invalid', 'project_id is invalid'))

    const explicitSessionID = options.sessionID !== undefined
    let sessionID: string
    if (explicitSessionID) {
      // A caller-owned entity ID is the durable idempotency key. It is
      // intentionally not checked against the local recent-ID cache: after a
      // timeout, reload, or epoch change the same ID must reach the server so
      // the durable claim can return the original result or a conflict.
      if (typeof options.sessionID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
      sessionID = this.cleanID(options.sessionID)
      if (!this.validSessionID(sessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
    } else {
      try {
        sessionID = this.cleanID(this.sessionIDGenerator())
        if (!this.validSessionID(sessionID)) throw new Error('session ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic session ID generation failed'))
      }
      if (this.recentEntityIDs.has(sessionID)) return Promise.reject(new CommandFacadeError('id_generation', 'session ID collided with an active or recently used create command', { collision: true }))
    }

    const args: JsonObject = { session_id: sessionID, project_id: cleanProjectID }
    const strings: Array<[keyof SessionCreateOptions, string]> = [
      ['displayName', 'display_name'],
      ['parentSessionID', 'parent_session_id'],
      ['cwd', 'cwd'],
      ['configPath', 'config_path'],
      ['provider', 'provider'],
      ['modelProfile', 'model_profile'],
      ['reasoningLevel', 'reasoning_level'],
    ]
    for (const [source, wire] of strings) {
      const value = options[source]
      if (value !== undefined) {
        if (typeof value !== 'string' || !value.trim() || value.length > 4096) return Promise.reject(new CommandFacadeError('invalid', `${wire} is invalid`))
        args[wire] = value.trim()
      }
    }
    if (options.fullAccess !== undefined) {
      if (typeof options.fullAccess !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'full_access is invalid'))
      args.full_access = options.fullAccess
    }
    this.rememberEntityID(sessionID)
    return this.submit('session.create', args, true, (value) => decodeCreateResult(value, sessionID, cleanProjectID), commandOptions)
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

  start(): void
  start(sessionID: string, content: string, options?: RunStartOptions): Promise<RunStartResult>
  start(sessionID?: string, content?: string, options: RunStartOptions = {}): void | Promise<RunStartResult> {
    // Keep the existing lifecycle attach call page-independent. RunCommands'
    // callers use the argument-bearing overload below.
    if (sessionID === undefined && content === undefined) {
      this.ensureStarted()
      return
    }
    if (typeof sessionID !== 'string' || typeof content !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'session_id and content are required'))
    const cleanSessionID = this.cleanID(sessionID)
    if (!this.validSessionID(cleanSessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
    if (typeof content !== 'string' || content.trim() === '' || this.utf8Bytes(content) > 256 * 1024) return Promise.reject(new CommandFacadeError('invalid', 'content is invalid'))

    const explicitRunID = options.runID !== undefined
    let runID: string
    if (explicitRunID) {
      if (typeof options.runID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'run_id is invalid'))
      runID = this.cleanID(options.runID)
      if (!this.validRunID(runID)) return Promise.reject(new CommandFacadeError('invalid', 'run_id is invalid'))
    } else {
      try {
        runID = this.cleanID(this.runIDGenerator())
        if (!this.validRunID(runID)) throw new Error('run ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic run ID generation failed'))
      }
      if (this.recentRunIDs.has(runID)) return Promise.reject(new CommandFacadeError('id_generation', 'run ID collided with an active or recently used command', { collision: true }))
      this.rememberRunID(runID)
    }
    // Do not trim content in the wire payload: text is the normalized input
    // and its exact bytes are part of the durable run fingerprint.
    return this.submit('run.start', { session_id: cleanSessionID, run_id: runID, content }, true, (value) => decodeRunStartResult(value, cleanSessionID, runID), options)
  }

  startRun(sessionID: string, content: string, options: RunStartOptions = {}): Promise<RunStartResult> {
    return this.start(sessionID, content, options)
  }

  private submitSessionToggle(name: 'session.archive' | 'session.restore', sessionID: string, archived: boolean, options: CommandOptions): Promise<SessionArchiveResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID) return Promise.reject(new CommandFacadeError('invalid', 'session_id is required'))
    return this.submit(name, { session_id: cleanSessionID }, true, (value) => decodeArchiveResult(value, cleanSessionID, archived), options)
  }

  private cleanID(value: string): string {
    return typeof value === 'string' ? value.trim() : ''
  }

  private validSessionID(value: string): boolean {
    if (value.length === 0 || value.length > 128 || !/^[A-Za-z0-9_.-]+$/.test(value) || value === '.' || value === '..') return false
    // Keep this client-side boundary byte-for-byte aligned with Go's
    // ValidateSessionCreateID: path-safe IDs whose trailing dots/spaces are
    // removed are compared case-insensitively against reserved directories.
    const reservedKey = value.replace(/[. ]+$/g, '').toLowerCase()
    return reservedKey !== 'blobs' && reservedKey !== '.session-claims'
  }

  private validRunID(value: string): boolean {
    if (value.length === 0 || value.length > 128 || !/^[A-Za-z0-9_.-]+$/.test(value) || value === '.' || value === '..') return false
    const reservedKey = value.replace(/[. ]+$/g, '').toLowerCase()
    return reservedKey !== 'blobs' && reservedKey !== '.session-claims'
  }

  private utf8Bytes(value: string): number {
    return typeof TextEncoder === 'function' ? new TextEncoder().encode(value).byteLength : value.length
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
    this.ensureStarted()
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

  private rememberEntityID(id: string): void {
    if (this.recentEntityIDs.has(id)) return
    this.recentEntityIDs.add(id)
    this.recentEntityIDOrder.push(id)
    while (this.recentEntityIDOrder.length > this.maxRecentEntityIDs) {
      const retired = this.recentEntityIDOrder.shift()
      if (retired !== undefined) this.recentEntityIDs.delete(retired)
    }
  }

  private rememberRunID(id: string): void {
    this.recentRunIDs.add(id)
    this.recentRunIDOrder.push(id)
    while (this.recentRunIDOrder.length > this.maxRecentEntityIDs) {
      const retired = this.recentRunIDOrder.shift()
      if (retired !== undefined) this.recentRunIDs.delete(retired)
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
