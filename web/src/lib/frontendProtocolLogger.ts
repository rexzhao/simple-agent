import { useCallback, useSyncExternalStore } from 'react'

export const FRONTEND_PROTOCOL_LOG_PREFIX = '[sai:frontend-protocol]'
export const FRONTEND_PROTOCOL_LOG_CAPACITY = 1000

export interface FrontendProtocolLogRecord {
  timestamp: string
  log_seq: number
  session_id: string
  source: string
  kind: string
  event_type?: string
  run_id?: string
  turn_id?: string
  agent_iteration?: unknown
  item_id?: string
  revision?: unknown
  stream_sequence?: unknown
  [key: string]: unknown
}

export interface FrontendProtocolLogInput {
  sessionID: string
  source: string
  kind: string
  eventType?: string
  runID?: string
  turnID?: string
  agentIteration?: unknown
  itemID?: string
  revision?: unknown
  streamSequence?: unknown
  [key: string]: unknown
}

export interface FrontendProtocolLoggingSnapshot {
  enabled: boolean
  records: FrontendProtocolLogRecord[]
  droppedCount: number
}

type SessionLogState = {
  enabled: boolean
  records: FrontendProtocolLogRecord[]
  droppedCount: number
  nextLogSeq: number
  snapshot: FrontendProtocolLoggingSnapshot
}

const emptySnapshot: FrontendProtocolLoggingSnapshot = Object.freeze({
  enabled: false,
  records: Object.freeze([]) as unknown as FrontendProtocolLogRecord[],
  droppedCount: 0,
})

const uiNotifyBatchMS = 100

function cloneValue<T>(value: T): T {
  if (typeof structuredClone === 'function') {
    try {
      return structuredClone(value)
    } catch {
      // Fall through for values such as functions or host objects that the
      // browser structured-clone implementation cannot copy.
    }
  }

  const seen = new WeakMap<object, unknown>()
  const clone = (current: unknown): unknown => {
    if (current === null || typeof current !== 'object') return current
    const prior = seen.get(current)
    if (prior !== undefined) return prior
    if (current instanceof Date) return new Date(current.getTime())
    if (current instanceof Error) return { name: current.name, message: current.message, stack: current.stack }
    if (Array.isArray(current)) {
      const result: unknown[] = []
      seen.set(current, result)
      for (const item of current) result.push(clone(item))
      return result
    }
    const result: Record<string, unknown> = {}
    seen.set(current, result)
    for (const [key, item] of Object.entries(current)) result[key] = clone(item)
    return result
  }
  return clone(value) as T
}

function jsonReplacer() {
  const seen = new WeakSet<object>()
  return (_key: string, value: unknown): unknown => {
    if (typeof value === 'bigint') return value.toString()
    if (value instanceof Error) return { name: value.name, message: value.message, stack: value.stack }
    if (value && typeof value === 'object') {
      if (seen.has(value)) return '[Circular]'
      seen.add(value)
    }
    return value
  }
}

function jsonLines(sessionID: string, snapshot: FrontendProtocolLoggingSnapshot): string {
  const metadata = {
    timestamp: new Date().toISOString(),
    session_id: sessionID,
    source: 'frontend_protocol_logger',
    kind: 'metadata',
    retained_count: snapshot.records.length,
    dropped_count: snapshot.droppedCount,
    capacity: FRONTEND_PROTOCOL_LOG_CAPACITY,
  }
  const lines = [JSON.stringify(metadata, jsonReplacer())]
  lines.push(...snapshot.records.map((record) => JSON.stringify(record, jsonReplacer())))
  return `${lines.join('\n')}\n`
}

function stateFor(sessions: Map<string, SessionLogState>, sessionID: string): SessionLogState {
  let state = sessions.get(sessionID)
  if (!state) {
    state = {
      enabled: false,
      records: [],
      droppedCount: 0,
      nextLogSeq: 1,
      snapshot: emptySnapshot,
    }
    sessions.set(sessionID, state)
  }
  return state
}

function updateSnapshot(state: SessionLogState): void {
  state.snapshot = {
    enabled: state.enabled,
    records: state.records,
    droppedCount: state.droppedCount,
  }
}

export class FrontendProtocolLogger {
  private readonly sessions = new Map<string, SessionLogState>()
  private readonly listeners = new Map<string, Set<() => void>>()
  private readonly pendingNotifications = new Map<string, ReturnType<typeof setTimeout>>()

  subscribe(sessionID: string, listener: () => void): () => void {
    if (!sessionID) return () => {}
    const sessionListeners = this.listeners.get(sessionID) ?? new Set<() => void>()
    sessionListeners.add(listener)
    this.listeners.set(sessionID, sessionListeners)
    return () => {
      sessionListeners.delete(listener)
      if (sessionListeners.size === 0) {
        this.listeners.delete(sessionID)
        this.cancelPendingNotification(sessionID)
      }
    }
  }

  isEnabled(sessionID: string): boolean {
    return Boolean(sessionID && this.sessions.get(sessionID)?.enabled)
  }

  private cancelPendingNotification(sessionID: string): void {
    const timer = this.pendingNotifications.get(sessionID)
    if (timer === undefined) return
    clearTimeout(timer)
    this.pendingNotifications.delete(sessionID)
  }

  private notifyImmediately(sessionID: string): void {
    this.cancelPendingNotification(sessionID)
    for (const listener of this.listeners.get(sessionID) ?? []) listener()
  }

  private notifySoon(sessionID: string): void {
    if (!this.listeners.has(sessionID) || this.pendingNotifications.has(sessionID)) return
    const timer = setTimeout(() => {
      this.pendingNotifications.delete(sessionID)
      for (const listener of this.listeners.get(sessionID) ?? []) listener()
    }, uiNotifyBatchMS)
    this.pendingNotifications.set(sessionID, timer)
  }

  getSnapshot(sessionID: string): FrontendProtocolLoggingSnapshot {
    if (!sessionID) return emptySnapshot
    return stateFor(this.sessions, sessionID).snapshot
  }

  setEnabled(sessionID: string, enabled: boolean): void {
    if (!sessionID) return
    const state = stateFor(this.sessions, sessionID)
    if (state.enabled === enabled) return
    state.enabled = enabled
    updateSnapshot(state)
    this.notifyImmediately(sessionID)
  }

  clear(sessionID: string): void {
    if (!sessionID) return
    const state = stateFor(this.sessions, sessionID)
    state.records = []
    state.droppedCount = 0
    updateSnapshot(state)
    this.notifyImmediately(sessionID)
  }

  log(input: FrontendProtocolLogInput): FrontendProtocolLogRecord | null {
    if (!input.sessionID) return null
    const state = this.sessions.get(input.sessionID)
    if (!state?.enabled) return null

    const record = cloneValue({
      timestamp: new Date().toISOString(),
      log_seq: state.nextLogSeq++,
      session_id: input.sessionID,
      source: input.source,
      kind: input.kind,
      ...(input.eventType !== undefined ? { event_type: input.eventType } : {}),
      ...(input.runID !== undefined ? { run_id: input.runID } : {}),
      ...(input.turnID !== undefined ? { turn_id: input.turnID } : {}),
      ...(input.agentIteration !== undefined ? { agent_iteration: input.agentIteration } : {}),
      ...(input.itemID !== undefined ? { item_id: input.itemID } : {}),
      ...(input.revision !== undefined ? { revision: input.revision } : {}),
      ...(input.streamSequence !== undefined ? { stream_sequence: input.streamSequence } : {}),
      ...Object.fromEntries(Object.entries(input).filter(([key]) => ![
        'sessionID', 'source', 'kind', 'eventType', 'runID', 'turnID',
        'agentIteration', 'itemID', 'revision', 'streamSequence',
      ].includes(key))),
    }) as FrontendProtocolLogRecord

    const records = [...state.records, record]
    if (records.length > FRONTEND_PROTOCOL_LOG_CAPACITY) {
      records.shift()
      state.droppedCount++
    }
    state.records = records
    updateSnapshot(state)

    // Pass a separate clone to the console. DevTools can inspect objects
    // lazily, so sharing the retained record would show later mutations.
    console.log(
      `${FRONTEND_PROTOCOL_LOG_PREFIX} session=${record.session_id} log_seq=${record.log_seq}`,
      cloneValue(record),
    )
    this.notifySoon(input.sessionID)
    return record
  }

  jsonl(sessionID: string): string {
    return jsonLines(sessionID, this.getSnapshot(sessionID))
  }

  // Test-only reset. It is intentionally not called by application code, so
  // a browser session keeps its diagnostics until the user clears them.
  resetForTesting(): void {
    for (const sessionID of this.pendingNotifications.keys()) this.cancelPendingNotification(sessionID)
    this.sessions.clear()
    for (const [sessionID] of this.listeners) this.notifyImmediately(sessionID)
  }
}

export const frontendProtocolLogger = new FrontendProtocolLogger()

export function useFrontendProtocolLogging(sessionID: string): FrontendProtocolLoggingSnapshot & {
  setEnabled: (enabled: boolean) => void
  clear: () => void
  jsonl: () => string
} {
  const subscribe = useCallback((listener: () => void) => frontendProtocolLogger.subscribe(sessionID, listener), [sessionID])
  const getSnapshot = useCallback(() => frontendProtocolLogger.getSnapshot(sessionID), [sessionID])
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  const setEnabled = useCallback((enabled: boolean) => frontendProtocolLogger.setEnabled(sessionID, enabled), [sessionID])
  const clear = useCallback(() => frontendProtocolLogger.clear(sessionID), [sessionID])
  const jsonl = useCallback(() => frontendProtocolLogger.jsonl(sessionID), [sessionID])
  return { ...snapshot, setEnabled, clear, jsonl }
}

export function frontendProtocolLogFilename(sessionID: string, now = new Date()): string {
  const safeSessionID = sessionID.replace(/[^a-zA-Z0-9._-]+/g, '_') || 'session'
  const timestamp = now.toISOString().replace(/[:.]/g, '-')
  return `frontend-protocol-${safeSessionID}-${timestamp}.jsonl`
}

export function downloadFrontendProtocolJSONL(sessionID: string): void {
  const blob = new Blob([frontendProtocolLogger.jsonl(sessionID)], { type: 'application/x-ndjson;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = frontendProtocolLogFilename(sessionID)
  try {
    anchor.click()
  } finally {
    window.setTimeout(() => URL.revokeObjectURL(url), 0)
  }
}

export async function copyFrontendProtocolJSONL(sessionID: string): Promise<void> {
  if (!navigator.clipboard?.writeText) {
    throw new Error('Clipboard API is unavailable. Allow clipboard access to copy the frontend protocol log.')
  }
  try {
    await navigator.clipboard.writeText(frontendProtocolLogger.jsonl(sessionID))
  } catch {
    throw new Error('Clipboard access failed. Allow clipboard access to copy the frontend protocol log.')
  }
}

export function protocolLogIdentity(payload: Record<string, unknown>, fallbackRunID?: string) {
  return {
    eventType: typeof payload.type === 'string' ? payload.type : undefined,
    runID: typeof payload.run_id === 'string' ? payload.run_id : fallbackRunID,
    turnID: typeof payload.turn_id === 'string' ? payload.turn_id : undefined,
    agentIteration: payload.agent_iteration,
    itemID: typeof payload.item_id === 'string' ? payload.item_id : undefined,
    revision: payload.revision,
    streamSequence: payload.seq,
  }
}

export { cloneValue as deepCloneForProtocolLog }
