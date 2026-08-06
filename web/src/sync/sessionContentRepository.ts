import { LocalReplica, resourceKeyString } from './localReplica'
import { SessionContentAdapter } from './sessionContentAdapter'
import type { SessionContentState, SessionView } from '../domain/sessionContent'
import { emptyCompaction, emptyHistory } from '../domain/sessionContent'
import type {
  SessionContentHistoryWindow,
  SessionContentItem,
  SessionContentMessage,
  SessionContentText,
  SessionRunState,
} from '../domain/sessionContent'
import type { DataAvailability } from '../domain/sessionContent'
import type { SyncReadError } from './errors'

export type { DataAvailability, SessionView } from '../domain/sessionContent'

export interface SessionContentRepositoryOptions {
  readonly maxCachedSessions?: number
}

type SessionListener = () => void

type SessionContentResource = { type: 'session_content'; id: string }

function resourceFor(sessionID: string): SessionContentResource {
  return { type: 'session_content', id: sessionID }
}

interface CachedView {
  readonly state: SessionContentState | undefined
  readonly initialized: boolean
  readonly availabilityKey: string
  readonly error?: SyncReadError
  readonly view: SessionView
}

const emptyRunState = undefined

function mergeText(base: SessionContentText | undefined, transient: { text: string; baseLength: number; checkpointLength?: number } | undefined): SessionContentText | undefined {
  if (!transient) return base
  const text = transient.text
  if (!base) return { inline: text }
  if (base.inline !== undefined) {
    // The adapter stores a stable tail and the durable prefix length at the
    // moment that identity first became transient. This is the only safe
    // concatenation rule; no substring search is used to infer an identity.
    if (base.inline.length === transient.baseLength) return { ...base, inline: `${base.inline}${text}` }
    // A checkpoint-less event cannot be matched by content. Keep the stable
    // item identity and prefer the authoritative prefix rather than guessing
    // by arbitrary text search; the transient run state still exposes its
    // tail to a caller that wants a recovery indicator.
    return { ...base }
  }
  // A Blob/preview-backed durable value cannot be concatenated locally. Do
  // not turn a partial overlay into a duplicate durable bubble.
  return base
}

function mergeMessage(item: SessionContentItem, run: SessionRunState | null): SessionContentItem {
  if (!item.message || !run) return item
  const textKey = JSON.stringify([item.key.turn_id, item.key.agent_iteration, item.key.item_id])
  const textOverlay = run.text[textKey]
  const reasoningOverlay = run.reasoning[textKey]
  if (!textOverlay && !reasoningOverlay) return item
  const message: SessionContentMessage = {
    ...item.message,
    ...(textOverlay ? { content: mergeText(item.message.content, textOverlay) } : {}),
    ...(reasoningOverlay ? { reasoning: mergeText(item.message.reasoning, reasoningOverlay) } : {}),
  }
  return { ...item, message }
}

function mergedHistory(state: SessionContentState, run: SessionRunState | null): SessionContentHistoryWindow {
  const items = state.snapshot.history.items.map((item) => mergeMessage(item, run))
  // Preserve the adapter's descriptor and stable item references when no
  // overlay touches this window. This is important for virtualized history.
  if (items.every((item, index) => item === state.snapshot.history.items[index])) return state.snapshot.history
  return { items, descriptor: state.snapshot.history.descriptor }
}

function availability(initialized: boolean, readState: string, state: SessionContentState | undefined, error?: SyncReadError): DataAvailability {
  if (!initialized) return readState === 'error'
    ? { status: 'error', error: { code: error?.code ?? 'transport', message: error?.message ?? 'session content is unavailable' } }
    : { status: 'loading' }
  if (readState === 'error' || readState === 'stale' || error) return {
    status: 'stale',
    dataUpdatedAt: state?.snapshot.session.updated_at ?? '',
  }
  return { status: 'ready' }
}

function availabilityKey(value: DataAvailability): string {
  return JSON.stringify(value)
}

/**
 * Domain repository for one session-content resource. It intentionally
 * accepts a LocalReplica only in the sync layer; its returned SessionView has
 * no protocol DTO, socket, Blob loading or subscription metadata.
 */
export class SessionContentRepository {
  readonly replica: LocalReplica
  private readonly listeners = new Map<string, Set<SessionListener>>()
  private readonly views = new Map<string, CachedView>()
  private readonly maxCachedSessions: number
  private readonly detachReplica: () => void

  constructor(replica = new LocalReplica(), options: SessionContentRepositoryOptions = {}) {
    this.replica = replica
    this.maxCachedSessions = options.maxCachedSessions ?? 64
    if (this.maxCachedSessions <= 0) throw new Error('maxCachedSessions must be positive')
    this.detachReplica = replica.subscribe((resource) => {
      if (resource.type !== 'session_content') return
      this.views.delete(resource.id)
      for (const listener of [...(this.listeners.get(resource.id) ?? [])]) listener()
    })
  }

  dispose(): void {
    this.detachReplica()
    this.listeners.clear()
    this.views.clear()
  }

  observe(sessionID: string, listener: SessionListener): () => void {
    const current = this.listeners.get(sessionID) ?? new Set<SessionListener>()
    current.add(listener)
    this.listeners.set(sessionID, current)
    return () => {
      current.delete(listener)
      if (current.size === 0) this.listeners.delete(sessionID)
    }
  }

  subscribe(sessionID: string, listener: SessionListener): () => void {
    return this.observe(sessionID, listener)
  }

  get(sessionID: string): SessionView {
    const resource = resourceFor(sessionID)
    const record = this.replica.get<SessionContentState>(resource)
    const state = record.value
    const readAvailability = availability(record.initialized, record.metadata.readState, state, record.metadata.error)
    const readAvailabilityKey = availabilityKey(readAvailability)
    const cached = this.views.get(sessionID)
    if (cached && cached.state === state && cached.initialized === record.initialized && cached.availabilityKey === readAvailabilityKey && cached.error === record.metadata.error) return cached.view

    const view: SessionView = state ? {
      availability: readAvailability,
      dataAvailability: readAvailability,
      session: state.snapshot.session,
      history: mergedHistory(state, state.transientRun),
      activeRun: state.snapshot.active_run,
      compaction: state.snapshot.compaction,
      ...(state.transientRun ? { runState: state.transientRun } : {}),
      ...(record.metadata.error ? { error: { code: record.metadata.error.code, message: record.metadata.error.message } } : {}),
    } : {
      availability: readAvailability,
      dataAvailability: readAvailability,
      history: emptyHistory,
      activeRun: null,
      compaction: emptyCompaction,
      ...(emptyRunState ? { runState: emptyRunState } : {}),
      ...(record.metadata.error ? { error: { code: record.metadata.error.code, message: record.metadata.error.message } } : {}),
    }
    this.views.set(sessionID, { state, initialized: record.initialized, availabilityKey: readAvailabilityKey, error: record.metadata.error, view })
    while (this.views.size > this.maxCachedSessions) {
      const oldest = this.views.keys().next().value as string | undefined
      if (!oldest) break
      this.views.delete(oldest)
    }
    return view
  }

  getSessionView(sessionID: string): SessionView {
    return this.get(sessionID)
  }

  select<T>(sessionID: string, selector: (view: SessionView) => T): T {
    return selector(this.get(sessionID))
  }

  getRunState(sessionID: string): SessionRunState | undefined {
    return this.get(sessionID).runState
  }

  selectRunState(sessionID: string): SessionRunState | undefined {
    return this.getRunState(sessionID)
  }

  evict(sessionID: string): void {
    this.views.delete(sessionID)
  }

  resourceKey(sessionID: string): string {
    return resourceKeyString(resourceFor(sessionID))
  }

  /** The adapter is exposed only to Runtime wiring, never to page callers. */
  adapter(sessionID: string): SessionContentAdapter {
    return new SessionContentAdapter(sessionID)
  }
}

export function selectSessionView(repository: SessionContentRepository, sessionID: string): SessionView {
  return repository.get(sessionID)
}

export function selectSessionRunState(repository: SessionContentRepository, sessionID: string): SessionRunState | undefined {
  return repository.getRunState(sessionID)
}
