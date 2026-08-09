import type { ApplicationRepositories, ApplicationSignals } from '../applicationServices'
import type { DebugFocusedMessage, DebugRegisteredMessage, DebugExecutorPayload, DebugUnregisteredMessage, ErrorMessage, ProtocolMessage } from '../protocol/types'
import { CommandFacade } from '../sync/commandFacade'
import { LocalReplica } from '../sync/localReplica'
import type { RuntimeTransport } from '../sync/runtime'
import { SyncRuntime } from '../sync/runtime'
import type { TransportCloseEvent, TransportReadyEvent } from '../sync/transport'

export const WEB_DEBUG_TARGET_PROJECT_ID = 'project-f25c5aac78f681b52aabf5c0'

export interface WebDebugAppStateSnapshot {
  readonly currentProject: string | null
  readonly currentSession: string | null
}

export interface WebDebugAppState {
  readonly currentProject: string | null
  readonly currentSession: string | null
  readonly snapshot: () => WebDebugAppStateSnapshot
}

export type WebDebugSelectionErrorCode =
  | 'invalid_project'
  | 'invalid_session'
  | 'project_unavailable'
  | 'project_not_found'
  | 'session_unavailable'
  | 'session_not_found'

export class WebDebugSelectionError extends Error {
  readonly code: WebDebugSelectionErrorCode

  constructor(code: WebDebugSelectionErrorCode) {
    const messages: Record<WebDebugSelectionErrorCode, string> = {
      invalid_project: 'debug project selection is invalid',
      invalid_session: 'debug session selection is invalid',
      project_unavailable: 'project authority is unavailable',
      project_not_found: 'project was not found in project authority',
      session_unavailable: 'session authority is unavailable',
      session_not_found: 'session was not found in session authority',
    }
    super(messages[code])
    this.name = 'WebDebugSelectionError'
    this.code = code
  }
}

export interface WebDebugSelectionResult {
  readonly projectID: string
  readonly sessionID?: string
}

export interface WebDebugIdleOptions {
  readonly timeoutMS?: number
  readonly signal?: AbortSignal
}

export type WebDebugIdleErrorCode = 'timeout' | 'cancelled' | 'stopped' | 'disposed' | 'invalid_timeout'

export class WebDebugIdleError extends Error {
  readonly code: WebDebugIdleErrorCode

  constructor(code: WebDebugIdleErrorCode) {
    const messages: Record<WebDebugIdleErrorCode, string> = {
      timeout: 'debug waitIdle timed out',
      cancelled: 'debug waitIdle was cancelled',
      stopped: 'debug waitIdle stopped with the application',
      disposed: 'debug waitIdle was disposed with the application',
      invalid_timeout: 'debug waitIdle timeout is invalid',
    }
    super(messages[code])
    this.name = 'WebDebugIdleError'
    this.code = code
  }
}

export interface WebDebugSurface {
  readonly replica: LocalReplica
  readonly runtime: SyncRuntime
  readonly repositories: ApplicationRepositories
  readonly commandFacade: CommandFacade
  readonly appState: WebDebugAppState
  readonly selectProject: (projectID: string) => Promise<WebDebugSelectionResult>
  readonly selectSession: (sessionID: string) => Promise<WebDebugSelectionResult>
  readonly waitIdle: (options?: WebDebugIdleOptions) => Promise<void>
}

declare global {
  interface Window {
    __SAI_DEBUG__?: WebDebugSurface
  }
}

interface IdentityOptions {
  readonly pageIDGenerator?: () => string
  readonly pageEpochGenerator?: () => string
}

export interface WebDebugBridgeOptions extends IdentityOptions {
  readonly transport: RuntimeTransport
  readonly runtime: SyncRuntime
  readonly replica: LocalReplica
  readonly repositories: ApplicationRepositories
  readonly commandFacade: CommandFacade
  readonly signals: ApplicationSignals
  readonly window?: Window
  readonly document?: Document
  readonly pollIntervalMS?: number
  readonly setTimeout?: (handler: () => void, timeout: number) => ReturnType<typeof globalThis.setTimeout>
  readonly clearTimeout?: (handle: ReturnType<typeof globalThis.setTimeout>) => void
}

interface DebugRegistration {
  readonly generation: number
  readonly sessionID: string
  readonly focused: boolean
  readonly focusRevision: number
  readonly registerOperationID: string
}

type DebugOperationType = 'register' | 'focus' | 'unregister'

interface DebugOperation {
  readonly id: string
  readonly type: DebugOperationType
  readonly generation: number
  readonly pageID: string
  readonly pageEpoch: string
  readonly payloadPageID: string
  readonly payloadPageEpoch: string
  readonly sessionID: string
}

type DebugBlockScope = 'generation' | 'eligibility'

type BridgePhase = 'hidden' | 'pending' | 'registered' | 'blocked'

interface Waiter {
  readonly reject: (reason: WebDebugIdleError) => void
  readonly resolve: () => void
  readonly signal?: AbortSignal
  timer?: ReturnType<typeof globalThis.setTimeout>
  abortListener?: () => void
  stable: boolean
  settled: boolean
}

function defaultIdentity(): string | undefined {
  const cryptoObject = globalThis.crypto
  if (!cryptoObject) return undefined
  try {
    if (typeof cryptoObject.randomUUID === 'function') return cryptoObject.randomUUID()
  } catch {
    // Fall through to getRandomValues when randomUUID is unavailable or fails.
  }
  try {
    if (typeof cryptoObject.getRandomValues !== 'function') return undefined
    const bytes = cryptoObject.getRandomValues(new Uint8Array(16))
    bytes[6] = (bytes[6] & 0x0f) | 0x40
    bytes[8] = (bytes[8] & 0x3f) | 0x80
    const hex = [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('')
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
  } catch {
    return undefined
  }
}

let debugMessageSequence = 0
function messageID(): string {
  debugMessageSequence += 1
  return `debug_${debugMessageSequence}`
}

function hasNonEmptyID(value: string | null | undefined): value is string {
  return typeof value === 'string' && value.length > 0
}

function isDebugControlError(message: ProtocolMessage): message is ErrorMessage {
  return message.type === 'error' && message.payload.code.startsWith('web_debug_')
}

function debugPayload(pageID: string, pageEpoch: string, sessionID: string, focused: boolean): DebugExecutorPayload {
  return { page_id: pageID, page_epoch: pageEpoch, session_id: sessionID, focused }
}

function makeDebugMessage<T extends 'debug_register' | 'debug_focus' | 'debug_unregister'>(
  type: T,
  payload: DebugExecutorPayload,
): ProtocolMessage {
  return { version: 1, type, id: messageID(), payload } as ProtocolMessage
}

/**
 * Browser-only infrastructure bridge for the stage-2 debug surface. It owns
 * no second transport and never crosses the ordinary ApplicationPageServices
 * boundary. Registration is intentionally separate from surface construction:
 * the object is created once, but is reachable from window only after the
 * server has acknowledged the current page/session generation.
 */
export class WebDebugBridge {
  private readonly transport: RuntimeTransport
  private readonly runtime: SyncRuntime
  private readonly replica: LocalReplica
  private readonly repositories: ApplicationRepositories
  private readonly commandFacade: CommandFacade
  private readonly signals: ApplicationSignals
  private readonly scope?: Window
  private readonly documentRef?: Document
  private readonly pollIntervalMS: number
  private readonly setTimer: NonNullable<WebDebugBridgeOptions['setTimeout']>
  private readonly clearTimer: NonNullable<WebDebugBridgeOptions['clearTimeout']>
  private readonly surface: WebDebugSurface
  private readonly detach: Array<() => void> = []
  private readonly waiters = new Set<Waiter>()
  private readonly operations = new Map<string, DebugOperation>()

  private pageIDValue?: string
  private pageEpochValue?: string
  private phase: BridgePhase = 'hidden'
  private registration?: DebugRegistration
  private registerOperation?: DebugOperation
  private activeFocusOperation?: DebugOperation
  private blockedGeneration?: number
  private blockedEligibilityVersion?: number
  private blockedSessionID?: string
  private blockedScope?: DebugBlockScope
  private lastAttemptGeneration?: number
  private lastAttemptEligibilityVersion?: number
  private lastAttemptSessionID?: string
  private eligibilityVersion = 0
  private focusRevision = 0
  private started = false
  private disposed = false

  constructor(options: WebDebugBridgeOptions) {
    this.transport = options.transport
    this.runtime = options.runtime
    this.replica = options.replica
    this.repositories = options.repositories
    this.commandFacade = options.commandFacade
    this.signals = options.signals
    this.scope = options.window ?? (typeof window === 'undefined' ? undefined : window)
    this.documentRef = options.document ?? this.scope?.document ?? (typeof document === 'undefined' ? undefined : document)
    this.pollIntervalMS = options.pollIntervalMS ?? 20
    this.setTimer = options.setTimeout ?? ((handler, timeout) => globalThis.setTimeout(handler, timeout))
    this.clearTimer = options.clearTimeout ?? ((handle) => globalThis.clearTimeout(handle))
    if (!Number.isFinite(this.pollIntervalMS) || this.pollIntervalMS <= 0) throw new Error('debug waitIdle polling interval must be positive')

    const injectedPageID = options.pageIDGenerator?.()
    const injectedPageEpoch = options.pageEpochGenerator?.()
    if (options.pageIDGenerator && !hasNonEmptyID(injectedPageID ?? null)) throw new Error('debug page identity must be non-empty')
    if (options.pageEpochGenerator && !hasNonEmptyID(injectedPageEpoch ?? null)) throw new Error('debug page epoch must be non-empty')
    const pageID = injectedPageID ?? defaultIdentity()
    const pageEpoch = injectedPageEpoch ?? defaultIdentity()
    if (hasNonEmptyID(pageID) && hasNonEmptyID(pageEpoch)) {
      this.pageIDValue = pageID
      this.pageEpochValue = pageEpoch
    }

    const appState: WebDebugAppState = Object.freeze({
      get currentProject() { return options.signals.currentProject.get() },
      get currentSession() { return options.signals.currentSession.get() },
      snapshot: () => Object.freeze({
        currentProject: options.signals.currentProject.get(),
        currentSession: options.signals.currentSession.get(),
      }),
    })
    this.surface = Object.freeze({
      replica: this.replica,
      runtime: this.runtime,
      repositories: this.repositories,
      commandFacade: this.commandFacade,
      appState,
      selectProject: (projectID: string) => this.selectProject(projectID),
      selectSession: (sessionID: string) => this.selectSession(sessionID),
      waitIdle: (idleOptions?: WebDebugIdleOptions) => this.waitIdle(idleOptions),
    })
  }

  get pageID(): string | undefined { return this.pageIDValue }
  get pageEpoch(): string | undefined { return this.pageEpochValue }
  get registered(): boolean { return this.phase === 'registered' }
  get exposed(): boolean { return this.scope?.__SAI_DEBUG__ === this.surface }

  start(): void {
    if (this.disposed) throw new Error('web debug bridge has been disposed')
    if (this.started) return
    this.started = true
    this.detach.push(
      this.transport.onMessage((message, generation) => this.handleMessage(message, generation)),
      this.transport.onReady((event) => this.handleReady(event)),
      this.transport.onClose((event) => this.handleClose(event)),
      this.signals.currentProject.subscribe(() => this.handleEligibilityPublication()),
      this.signals.currentSession.subscribe(() => this.handleEligibilityPublication()),
      this.repositories.sessionIndex.subscribeProject(WEB_DEBUG_TARGET_PROJECT_ID, () => this.handleEligibilityPublication()),
    )
    this.scope?.addEventListener('focus', this.handleFocus)
    this.scope?.addEventListener('blur', this.handleFocus)
    this.detach.push(() => this.scope?.removeEventListener('focus', this.handleFocus))
    this.detach.push(() => this.scope?.removeEventListener('blur', this.handleFocus))
    this.handleEligibilityPublication()
    if (this.transport.isReady) {
      this.handleReady({
        generation: this.transport.connectionGeneration,
        serverEpoch: this.transport.serverEpoch ?? '',
        connectionID: '',
        heartbeatIntervalMS: 0,
        maxMessageBytes: 0,
      })
    }
  }

  stop(): void {
    if (!this.started) return
    // Keep the transport alive long enough for the best-effort unregister.
    this.hide(true)
    this.started = false
    for (const release of this.detach.splice(0)) release()
    this.rejectWaiters(new WebDebugIdleError('stopped'))
    this.blockedGeneration = undefined
    this.blockedEligibilityVersion = undefined
    this.blockedSessionID = undefined
    this.blockedScope = undefined
    this.lastAttemptGeneration = undefined
    this.lastAttemptEligibilityVersion = undefined
    this.lastAttemptSessionID = undefined
    this.operations.clear()
    this.registerOperation = undefined
    this.activeFocusOperation = undefined
  }

  dispose(): void {
    if (this.disposed) return
    if (this.started) {
      // Keep the transport alive long enough for the best-effort unregister.
      this.hide(true)
      this.started = false
      for (const release of this.detach.splice(0)) release()
    }
    this.disposed = true
    this.operations.clear()
    this.registerOperation = undefined
    this.activeFocusOperation = undefined
    this.removeSurface()
    this.rejectWaiters(new WebDebugIdleError('disposed'))
  }

  private handleFocus = (): void => {
    this.focusRevision += 1
    if (this.phase !== 'registered' || !this.registration) return
    this.sendFocus(this.registration.sessionID)
  }

  private handleReady(event: TransportReadyEvent): void {
    if (!this.started) return
    if (this.phase !== 'hidden' && this.phase !== 'blocked' && this.registration && this.registration.generation !== event.generation) {
      this.hide(false)
    }
    if (this.blockedGeneration !== event.generation) {
      this.blockedGeneration = undefined
      this.blockedEligibilityVersion = undefined
      this.blockedSessionID = undefined
      this.blockedScope = undefined
    }
    this.clearOperationsExceptGeneration(event.generation)
    this.tryRegister(event.generation)
  }

  private handleClose(event: TransportCloseEvent): void {
    if (!this.started) return
    if (this.registration && this.registration.generation !== event.generation) return
    this.hide(false)
    this.clearOperationsForGeneration(event.generation)
    this.lastAttemptGeneration = undefined
    this.lastAttemptEligibilityVersion = undefined
    this.lastAttemptSessionID = undefined
  }

  private handleEligibilityPublication(): void {
    if (!this.started) return
    this.eligibilityVersion += 1
    const candidate = this.eligibleSession()
    const activeSession = this.registration?.sessionID
    if ((this.phase === 'pending' || this.phase === 'registered') && (!candidate || candidate !== activeSession)) {
      this.hide(true)
    }
    if (candidate && this.phase === 'blocked' && this.blockedScope !== 'generation' && (
      this.blockedEligibilityVersion !== this.eligibilityVersion || this.blockedSessionID !== candidate
    )) {
      this.phase = 'hidden'
    }
    if (!candidate && this.phase !== 'pending' && this.phase !== 'registered') {
      this.phase = 'hidden'
      this.removeSurface()
    }
    this.tryRegister(this.transport.connectionGeneration)
  }

  private eligibleSession(): string | undefined {
    if (this.signals.currentProject.get() !== WEB_DEBUG_TARGET_PROJECT_ID) return undefined
    const sessionID = this.signals.currentSession.get()
    if (!hasNonEmptyID(sessionID)) return undefined
    const model = this.repositories.sessionIndex.getProjectReadModel(WEB_DEBUG_TARGET_PROJECT_ID)
    if (model.status !== 'ready') return undefined
    const summary = this.repositories.sessionIndex.getSummary(WEB_DEBUG_TARGET_PROJECT_ID, sessionID)
    if (!summary || summary.project_id !== WEB_DEBUG_TARGET_PROJECT_ID) return undefined
    return sessionID
  }

  private tryRegister(generation: number): void {
    if (!this.started || !this.transport.isReady || this.phase === 'registered' || this.phase === 'pending') return
    const sessionID = this.eligibleSession()
    if (!sessionID) {
      this.phase = 'hidden'
      this.removeSurface()
      return
    }
    if (
      this.blockedGeneration === generation &&
      (this.blockedScope === 'generation' || (
        this.blockedEligibilityVersion === this.eligibilityVersion &&
        this.blockedSessionID === sessionID
      ))
    ) return
    if (
      this.lastAttemptGeneration === generation &&
      this.lastAttemptEligibilityVersion === this.eligibilityVersion &&
      this.lastAttemptSessionID === sessionID
    ) return

    const focused = this.currentFocus()
    if (!this.ensureIdentity()) {
      this.phase = 'blocked'
      this.registration = undefined
      this.blockedGeneration = generation
      this.blockedEligibilityVersion = this.eligibilityVersion
      this.blockedSessionID = sessionID
      this.blockedScope = 'eligibility'
      this.removeSurface()
      return
    }
    const pageID = this.pageIDValue
    const pageEpoch = this.pageEpochValue
    if (!pageID || !pageEpoch) return
    this.lastAttemptGeneration = generation
    this.lastAttemptEligibilityVersion = this.eligibilityVersion
    this.lastAttemptSessionID = sessionID
    this.phase = 'pending'
    const message = makeDebugMessage('debug_register', debugPayload(pageID, pageEpoch, sessionID, focused))
    const operation = this.rememberOperation('register', message, generation, sessionID)
    this.registerOperation = operation
    this.registration = { generation, sessionID, focused, focusRevision: this.focusRevision, registerOperationID: operation.id }
    try {
      this.transport.send(message)
    } catch {
      this.forgetOperation(operation.id)
      this.registerOperation = undefined
      this.phase = 'blocked'
      this.registration = undefined
      this.blockedGeneration = generation
      this.blockedEligibilityVersion = this.eligibilityVersion
      this.blockedSessionID = sessionID
      this.blockedScope = 'eligibility'
      this.removeSurface()
    }
  }

  private handleMessage(message: ProtocolMessage, generation: number): void {
    if (message.type === 'debug_registered') {
      this.handleRegistered(message, generation)
      return
    }
    if (message.type === 'debug_focused') {
      this.handleFocused(message, generation)
      return
    }
    if (message.type === 'debug_unregistered') {
      this.handleUnregistered(message, generation)
      return
    }
    if (isDebugControlError(message)) {
      this.handleDebugError(message, generation)
    }
  }

  private handleRegistered(message: DebugRegisteredMessage, generation: number): void {
    const payload = message.payload
    const pending = this.registration
    const matches = this.phase === 'pending' && pending !== undefined && pending.generation === generation &&
      generation === this.transport.connectionGeneration && payload.page_id === this.pageID &&
      payload.page_epoch === this.pageEpoch && payload.session_id === pending.sessionID
    if (!matches) {
      // A delayed acknowledgement can describe an old session/epoch on the
      // same socket. Cleanup is owned by the unregister operation itself; its
      // response can never be mistaken for the current register operation.
      this.sendUnregister(payload, generation)
      return
    }
    if (this.eligibleSession() !== pending.sessionID) {
      this.hide(true, payload)
      return
    }
    this.forgetOperation(pending.registerOperationID)
    if (this.registerOperation?.id === pending.registerOperationID) this.registerOperation = undefined
    const focusChangedWhilePending = pending.focusRevision !== this.focusRevision || payload.focused !== this.currentFocus()
    this.phase = 'registered'
    this.registration = { ...pending, focused: payload.focused, focusRevision: this.focusRevision }
    this.exposeSurface()
    if (focusChangedWhilePending) this.sendFocus(pending.sessionID)
  }

  private handleFocused(message: DebugFocusedMessage, generation: number): void {
    const operation = this.findOperation('focus', message.payload, generation)
    if (!operation) return
    this.forgetOperation(operation.id)
    if (this.activeFocusOperation?.id === operation.id) this.activeFocusOperation = undefined
  }

  private handleUnregistered(message: DebugUnregisteredMessage, generation: number): void {
    const operation = this.findOperation('unregister', message.payload, generation)
    if (operation) this.forgetOperation(operation.id)
  }

  private handleDebugError(message: ErrorMessage, generation: number): void {
    if (generation !== this.transport.connectionGeneration) return
    const requestID = message.payload.request_id
    if (!requestID) return
    const operation = this.operations.get(requestID)
    if (!operation || operation.generation !== generation || operation.pageID !== this.pageID || operation.pageEpoch !== this.pageEpoch) return
    this.forgetOperation(operation.id)
    if (operation.type === 'unregister') return
    if (operation.type === 'focus' && this.activeFocusOperation?.id !== operation.id) return
    if (operation.type === 'focus') this.activeFocusOperation = undefined
    if (operation.type === 'register') {
      if (this.phase !== 'pending' || !this.registration || this.registration.registerOperationID !== operation.id) return
      this.registerOperation = undefined
    } else if (this.phase !== 'registered' || !this.registration || this.registration.generation !== operation.generation || this.registration.sessionID !== operation.sessionID) {
      return
    }
    const sessionID = this.registration?.sessionID ?? operation.sessionID
    this.hide(false)
    this.blockedGeneration = generation
    this.blockedEligibilityVersion = this.eligibilityVersion
    this.blockedSessionID = sessionID
    this.blockedScope = message.payload.code === 'web_debug_disabled' || message.payload.code === 'web_debug_closed'
      ? 'generation'
      : 'eligibility'
    this.phase = 'blocked'
    this.removeSurface()
    // The message is deliberately not forwarded to runtime or command
    // handling; it belongs only to this bridge's owned control operation.
  }

  private hide(sendUnregister: boolean, payload?: DebugExecutorPayload): void {
    const registration = this.registration
    const pageID = this.pageIDValue
    const pageEpoch = this.pageEpochValue
    const unregisterPayload = payload ?? (registration
      ? pageID && pageEpoch ? debugPayload(pageID, pageEpoch, registration.sessionID, this.currentFocus()) : undefined
      : undefined)
    const unregisterGeneration = registration?.generation
    this.phase = 'hidden'
    this.registration = undefined
    if (registration && this.registerOperation?.id === registration.registerOperationID) {
      this.forgetOperation(registration.registerOperationID)
      this.registerOperation = undefined
    }
    if (this.activeFocusOperation) {
      this.forgetOperation(this.activeFocusOperation.id)
      this.activeFocusOperation = undefined
    }
    this.removeSurface()
    if (sendUnregister && unregisterPayload) this.sendUnregister(unregisterPayload, unregisterGeneration)
  }

  private sendUnregister(payload: DebugExecutorPayload, generation = this.transport.connectionGeneration): void {
    if (!this.transport.isReady || generation !== this.transport.connectionGeneration) return
    const message = makeDebugMessage('debug_unregister', payload)
    const operation = this.rememberOperation('unregister', message, generation, payload.session_id)
    try {
      this.transport.send(message)
    } catch {
      this.forgetOperation(operation.id)
      /* best effort */
    }
  }

  private sendFocus(sessionID: string): void {
    if (!this.transport.isReady || this.phase !== 'registered') return
    const pageID = this.pageIDValue
    const pageEpoch = this.pageEpochValue
    if (!pageID || !pageEpoch) return
    const message = makeDebugMessage('debug_focus', debugPayload(pageID, pageEpoch, sessionID, this.currentFocus()))
    const operation = this.rememberOperation('focus', message, this.registration?.generation ?? this.transport.connectionGeneration, sessionID)
    this.activeFocusOperation = operation
    try {
      this.transport.send(message)
    } catch {
      this.forgetOperation(operation.id)
      if (this.activeFocusOperation?.id === operation.id) this.activeFocusOperation = undefined
      // The close event hides the surface. A focus update itself is not a
      // registration retry trigger and must not create a reconnect loop.
    }
  }

  private ensureIdentity(): boolean {
    if (this.pageIDValue && this.pageEpochValue) return true
    const pageID = defaultIdentity()
    const pageEpoch = defaultIdentity()
    if (!pageID || !pageEpoch) return false
    this.pageIDValue = pageID
    this.pageEpochValue = pageEpoch
    return true
  }

  private rememberOperation(
    type: DebugOperationType,
    message: ProtocolMessage,
    generation: number,
    sessionID: string,
  ): DebugOperation {
    const pageID = this.pageIDValue
    const pageEpoch = this.pageEpochValue
    if (!pageID || !pageEpoch) throw new Error('debug operation identity is unavailable')
    const payload = message.payload as DebugExecutorPayload
    if (type === 'focus' && this.activeFocusOperation) this.forgetOperation(this.activeFocusOperation.id)
    const operation: DebugOperation = {
      id: message.id,
      type,
      generation,
      pageID,
      pageEpoch,
      payloadPageID: payload.page_id,
      payloadPageEpoch: payload.page_epoch,
      sessionID,
    }
    this.operations.set(operation.id, operation)
    return operation
  }

  private forgetOperation(operationID: string): void {
    this.operations.delete(operationID)
  }

  private findOperation(type: DebugOperationType, payload: DebugExecutorPayload, generation: number): DebugOperation | undefined {
    return [...this.operations.values()].find((operation) => operation.type === type && operation.generation === generation &&
      operation.sessionID === payload.session_id && operation.payloadPageID === payload.page_id && operation.payloadPageEpoch === payload.page_epoch)
  }

  private clearOperationsForGeneration(generation: number): void {
    for (const [operationID, operation] of this.operations) {
      if (operation.generation === generation) this.operations.delete(operationID)
    }
    if (this.registerOperation?.generation === generation) this.registerOperation = undefined
    if (this.activeFocusOperation?.generation === generation) this.activeFocusOperation = undefined
  }

  private clearOperationsExceptGeneration(generation: number): void {
    for (const [operationID, operation] of this.operations) {
      if (operation.generation !== generation) this.operations.delete(operationID)
    }
    if (this.registerOperation && this.registerOperation.generation !== generation) this.registerOperation = undefined
    if (this.activeFocusOperation && this.activeFocusOperation.generation !== generation) this.activeFocusOperation = undefined
  }

  private exposeSurface(): void {
    if (this.scope) this.scope.__SAI_DEBUG__ = this.surface
  }

  private removeSurface(): void {
    if (this.scope?.__SAI_DEBUG__ === this.surface) delete this.scope.__SAI_DEBUG__
  }

  private currentFocus(): boolean {
    try { return this.documentRef?.hasFocus?.() ?? false } catch { return false }
  }

  private selectProject(projectID: string): Promise<WebDebugSelectionResult> {
    if (typeof projectID !== 'string' || projectID.length === 0) return Promise.reject(new WebDebugSelectionError('invalid_project'))
    const model = this.repositories.projectIndex.getSnapshot()
    if (model.status !== 'ready') return Promise.reject(new WebDebugSelectionError('project_unavailable'))
    if (!this.repositories.projectIndex.getByID(projectID)) return Promise.reject(new WebDebugSelectionError('project_not_found'))
    this.signals.currentProject.set(projectID)
    this.signals.currentSession.set(null)
    return Promise.resolve({ projectID })
  }

  private selectSession(sessionID: string): Promise<WebDebugSelectionResult> {
    if (typeof sessionID !== 'string' || sessionID.length === 0) return Promise.reject(new WebDebugSelectionError('invalid_session'))
    const projectModel = this.repositories.projectIndex.getSnapshot()
    if (projectModel.status !== 'ready') return Promise.reject(new WebDebugSelectionError('project_unavailable'))
    const projectIDs = projectModel.summaries.map((summary) => summary.id)
    const currentProject = this.signals.currentProject.get()
    const orderedProjectIDs = [
      ...(currentProject && projectIDs.includes(currentProject) ? [currentProject] : []),
      ...projectIDs.filter((projectID) => projectID !== currentProject),
    ]
    let unavailable = false
    for (const projectID of orderedProjectIDs) {
      const model = this.repositories.sessionIndex.getProjectReadModel(projectID)
      if (model.status !== 'ready') {
        unavailable = true
        continue
      }
      const summary = this.repositories.sessionIndex.getSummary(projectID, sessionID)
      if (summary?.project_id !== projectID) continue
      this.signals.currentProject.set(projectID)
      this.signals.currentSession.set(sessionID)
      return Promise.resolve({ projectID, sessionID })
    }
    return Promise.reject(new WebDebugSelectionError(unavailable ? 'session_unavailable' : 'session_not_found'))
  }

  private waitIdle(options: WebDebugIdleOptions = {}): Promise<void> {
    if (this.disposed) return Promise.reject(new WebDebugIdleError('disposed'))
    if (!this.started) return Promise.reject(new WebDebugIdleError('stopped'))
    const requestedTimeout = options.timeoutMS ?? 5_000
    if (!Number.isFinite(requestedTimeout) || requestedTimeout <= 0) return Promise.reject(new WebDebugIdleError('invalid_timeout'))
    const timeoutMS = Math.min(requestedTimeout, 30_000)
    if (options.signal?.aborted) return Promise.reject(new WebDebugIdleError('cancelled'))

    return new Promise<void>((resolve, reject) => {
      const waiter: Waiter = { resolve, reject, signal: options.signal, stable: false, settled: false }
      const finish = (error?: WebDebugIdleError) => {
        if (waiter.settled) return
        waiter.settled = true
        if (waiter.timer !== undefined) this.clearTimer(waiter.timer)
        if (waiter.signal && waiter.abortListener) waiter.signal.removeEventListener('abort', waiter.abortListener)
        this.waiters.delete(waiter)
        if (error) reject(error)
        else resolve()
      }
      const check = () => {
        if (waiter.settled) return
        if (this.disposed) { finish(new WebDebugIdleError('disposed')); return }
        if (!this.started) { finish(new WebDebugIdleError('stopped')); return }
        if (Date.now() >= deadline) { finish(new WebDebugIdleError('timeout')); return }
        if (this.isIdle()) {
          if (waiter.stable) { finish(); return }
          waiter.stable = true
        } else {
          waiter.stable = false
        }
        waiter.timer = this.setTimer(check, Math.min(this.pollIntervalMS, Math.max(1, deadline - Date.now())))
      }
      const deadline = Date.now() + timeoutMS
      waiter.abortListener = () => finish(new WebDebugIdleError('cancelled'))
      options.signal?.addEventListener('abort', waiter.abortListener, { once: true })
      this.waiters.add(waiter)
      check()
      if (options.signal?.aborted) waiter.abortListener?.()
    })
  }

  private isIdle(): boolean {
    const runtime = this.runtime.getDebugSnapshot()
    const commands = this.commandFacade.getDebugSnapshot()
    if (!runtime.started || !runtime.transportReady || runtime.busy || commands.pendingCount !== 0) return false
    const currentProject = this.signals.currentProject.get()
    const currentSession = this.signals.currentSession.get()
    if (hasNonEmptyID(currentProject) && this.repositories.projectIndex.getSnapshot().status === 'loading') return false
    if (hasNonEmptyID(currentProject) && hasNonEmptyID(currentSession)) {
      if (this.repositories.sessionIndex.getProjectReadModel(currentProject).status === 'loading') return false
      const content = this.repositories.sessionContent.get(currentSession)
      // A stale or error view is a terminal observation for waitIdle: it is
      // useful diagnostic state, whereas loading still represents unfinished
      // current-session work. History pagination is likewise unfinished work
      // even when the durable Session Content resource is already ready.
      if (content.availability.status === 'loading' || content.dataAvailability.status === 'loading' || content.historyState.loading) return false
    }
    return true
  }

  private rejectWaiters(error: WebDebugIdleError): void {
    for (const waiter of [...this.waiters]) {
      if (waiter.settled) continue
      waiter.settled = true
      if (waiter.timer !== undefined) this.clearTimer(waiter.timer)
      if (waiter.signal && waiter.abortListener) waiter.signal.removeEventListener('abort', waiter.abortListener)
      this.waiters.delete(waiter)
      waiter.reject(error)
    }
  }
}
