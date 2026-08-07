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
import { SyncReadError } from './errors'
import type { SessionContentHistoryReadOptions, SessionContentHistoryState } from '../domain/sessionContent'

export type { DataAvailability, SessionView } from '../domain/sessionContent'

export interface SessionContentRepositoryOptions {
  readonly maxCachedSessions?: number
  /** Installed by application composition; the page never sees this reader. */
  readonly historyReader?: (sessionID: string, options: SessionContentHistoryReadOptions, signal?: AbortSignal) => Promise<SessionContentHistoryWindow>
  readonly retry?: (sessionID: string) => void
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

interface HistoryCache {
  readonly generation: number
  readonly items: Map<string, SessionContentItem>
  descriptor: SessionContentHistoryWindow['descriptor']
  loading: boolean
  error?: SyncReadError
  requestGeneration: number
  /** Changes for loading, success, failure and retry are all observable. */
  version: number
}

const emptyRunState = undefined

function mergeText(base: SessionContentText | undefined, transient: { text: string; baseLength: number; checkpointLength?: number } | undefined): SessionContentText | undefined {
  if (!transient) return base
  const text = transient.text
  if (!base) return { inline: text }
  if (base.inline !== undefined) {
    // The adapter stores a stable tail and the durable prefix length at the
    // moment that identity first became transient.  A durable checkpoint may
    // consume none, part, or all of that tail before the next transient frame
    // arrives.  Consume by the protocol's length watermark only; never search
    // for text or attach an unkeyed tail to the last array item.
    const durableProgress = base.inline.length - transient.baseLength
    if (durableProgress < 0) return { ...base }
    const consumed = Math.min(text.length, durableProgress)
    return { ...base, inline: `${base.inline}${text.slice(consumed)}` }
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

function mergedHistory(state: SessionContentState, run: SessionRunState | null, page?: HistoryCache): SessionContentHistoryWindow {
  const durableItems = new Map<string, SessionContentItem>()
  if (page) for (const [key, item] of page.items) durableItems.set(key, item)
  for (const item of state.snapshot.history.items) durableItems.set(itemKey(item), item)
  const items = [...durableItems.values()]
    .sort((left, right) => left.seq - right.seq || itemKey(left).localeCompare(itemKey(right)))
    .map((item) => mergeMessage(item, run))
  // Preserve the adapter's descriptor and stable item references when no
  // overlay touches this window. This is important for virtualized history.
  if (!page && items.every((item, index) => item === state.snapshot.history.items[index])) return state.snapshot.history
  return {
    items,
    // A retained page describes only the older range. The live snapshot is
    // always authoritative for the latest range, especially after a new
    // item.upsert advances newest_item_seq or has_more_after.
    descriptor: page
      ? composePagedHistoryDescriptor(state.snapshot.history.descriptor, page.descriptor, items)
      : state.snapshot.history.descriptor,
  }
}

/**
 * Compose one application history view from two deliberately different
 * authorities:
 *
 * - `older` is a retained, cursor-bounded command page. It owns the loaded
 *   older boundary and whether more history exists before that boundary.
 * - `latest` is the current Session Content snapshot. It owns the live latest
 *   boundary and whether newer items exist.
 *
 * The bounds are recomputed from the exact deduplicated items so ItemsPage
 * watermarks cannot describe a different set than the rows being rendered.
 */
function composePagedHistoryDescriptor(
  latest: SessionContentHistoryWindow['descriptor'],
  older: SessionContentHistoryWindow['descriptor'],
  items: readonly SessionContentItem[],
): SessionContentHistoryWindow['descriptor'] {
  const first = items[0]
  const last = items[items.length - 1]
  return {
    limit: Math.max(latest.limit, older.limit),
    align_turn: latest.align_turn,
    visible_only: latest.visible_only,
    has_more_before: older.has_more_before,
    has_more_after: latest.has_more_after,
    ...(older.before_item_seq !== undefined ? { before_item_seq: older.before_item_seq } : {}),
    ...(latest.after_item_seq !== undefined ? { after_item_seq: latest.after_item_seq } : {}),
    ...(first ? { oldest_item_seq: String(first.seq) } : {}),
    ...(last ? { newest_item_seq: String(last.seq) } : {}),
  }
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

const emptyHistoryState: SessionContentHistoryState = Object.freeze({ loading: false, version: 0 })

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
  private readonly historyReader?: SessionContentRepositoryOptions['historyReader']
  private readonly retryResource?: (sessionID: string) => void
  private readonly historyPages = new Map<string, HistoryCache>()

  constructor(replica = new LocalReplica(), options: SessionContentRepositoryOptions = {}) {
    this.replica = replica
    this.maxCachedSessions = options.maxCachedSessions ?? 64
    if (this.maxCachedSessions <= 0) throw new Error('maxCachedSessions must be positive')
    this.historyReader = options.historyReader
    this.retryResource = options.retry
    this.detachReplica = replica.subscribe((resource, notification) => {
      if (resource.type !== 'session_content') return
      this.views.delete(resource.id)
      const page = this.historyPages.get(resource.id)
      const generation = replica.get<SessionContentState>(resource).metadata.generation
      if (notification?.retainedWindowInvalidated || (page && page.generation !== generation)) this.historyPages.delete(resource.id)
      for (const listener of [...(this.listeners.get(resource.id) ?? [])]) listener()
    })
  }

  dispose(): void {
    this.detachReplica()
    this.listeners.clear()
    this.views.clear()
    this.historyPages.clear()
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
    const historyPage = this.historyPages.get(sessionID)
    if (historyPage && historyPage.generation !== record.metadata.generation) {
      this.historyPages.delete(sessionID)
    }
    const cached = this.views.get(sessionID)
    const historyState = this.historyState(sessionID)
    if (cached && cached.state === state && cached.initialized === record.initialized && cached.availabilityKey === readAvailabilityKey && cached.error === record.metadata.error && cached.view.historyState.version === historyState.version && cached.view.historyState.loading === historyState.loading && cached.view.historyState.error?.code === historyState.error?.code) return cached.view

    const view: SessionView = state ? {
      availability: readAvailability,
      dataAvailability: readAvailability,
      session: state.snapshot.session,
      history: mergedHistory(state, state.transientRun, this.historyPages.get(sessionID)),
      historyState,
      activeRun: state.snapshot.active_run,
      compaction: state.snapshot.compaction,
      ...(state.transientRun ? { runState: state.transientRun } : {}),
      ...(record.metadata.error ? { error: { code: record.metadata.error.code, message: record.metadata.error.message } } : {}),
    } : {
      availability: readAvailability,
      dataAvailability: readAvailability,
      history: emptyHistory,
      historyState,
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
    this.historyPages.delete(sessionID)
  }

  resourceKey(sessionID: string): string {
    return resourceKeyString(resourceFor(sessionID))
  }

  historyState(sessionID: string): SessionContentHistoryState {
    const state = this.historyPages.get(sessionID)
    return state ? {
      loading: state.loading,
      version: state.version,
      ...(state.error ? { error: { code: state.error.code, message: state.error.message } } : {}),
    } : emptyHistoryState
  }

  retry(sessionID: string): void {
    this.retryResource?.(sessionID)
  }

  async readHistory(sessionID: string, options: SessionContentHistoryReadOptions = {}, signal?: AbortSignal): Promise<SessionContentHistoryWindow> {
    if (!this.historyReader) throw new Error('session history is unavailable')
    const resource = resourceFor(sessionID)
    const record = this.replica.get<SessionContentState>(resource)
    const generation = record.metadata.generation
    const current = this.historyPages.get(sessionID)
    const cache: HistoryCache = current && current.generation === generation
      ? current
      : {
        generation,
        items: new Map(),
        descriptor: record.value?.snapshot.history.descriptor ?? emptyHistory.descriptor,
        loading: false,
        requestGeneration: 0,
        version: 0,
      }
    if (cache.loading) return this.get(sessionID).history
    cache.loading = true
    cache.error = undefined
    cache.requestGeneration += 1
    cache.version += 1
    const requestGeneration = cache.requestGeneration
    this.historyPages.set(sessionID, cache)
    this.views.delete(sessionID)
    this.notifyLocal(sessionID)
    try {
      const page = await this.historyReader(sessionID, options, signal)
      const latestRecord = this.replica.get<SessionContentState>(resource)
      const latestCache = this.historyPages.get(sessionID)
      // A released/reopened session receives a new resource generation. A
      // page/blob that belongs to the old interest is never merged into it.
      if (latestRecord.metadata.generation !== generation || latestCache !== cache || latestCache.requestGeneration !== requestGeneration) return this.get(sessionID).history
      // Abort is an interest release, not merely a hint to fetch. A reader
      // may legally resolve after observing the signal, so check it again at
      // the merge barrier as well as passing it to the Blob client.
      if (signal?.aborted) {
        cache.loading = false
        cache.version += 1
        this.views.delete(sessionID)
        this.notifyLocal(sessionID)
        return this.get(sessionID).history
      }
      for (const item of page.items) cache.items.set(itemKey(item), item)
      cache.descriptor = mergeHistoryDescriptor(cache.descriptor, page.descriptor, options.direction)
      cache.loading = false
      cache.version += 1
      this.views.delete(sessionID)
      this.notifyLocal(sessionID)
      return this.get(sessionID).history
    } catch (reason) {
      const latestCache = this.historyPages.get(sessionID)
      if (latestCache === cache && latestCache.requestGeneration === requestGeneration) {
        cache.loading = false
        // Selection/unmount cancellation is not a failed history read. Do not
        // leave an error behind that a later interest could accidentally
        // resurrect; the generation/request guard still prevents late data.
        if (signal?.aborted || (reason instanceof SyncReadError && reason.code === 'aborted')) {
          cache.error = undefined
        } else {
          cache.error = reason instanceof SyncReadError ? reason : new SyncReadError('transport', 'session history is unavailable')
        }
        cache.version += 1
        this.views.delete(sessionID)
        this.notifyLocal(sessionID)
      }
      throw reason
    }
  }

  async loadOlder(sessionID: string, signal?: AbortSignal): Promise<boolean> {
    const view = this.get(sessionID)
    const descriptor = view.history.descriptor
    if (!descriptor.has_more_before || !descriptor.oldest_item_seq) return false
    const cursor = Number(descriptor.oldest_item_seq)
    if (!Number.isSafeInteger(cursor) || cursor <= 0) throw new SyncReadError('invalid_change', 'history cursor is invalid')
    await this.readHistory(sessionID, { cursor, direction: 'before', limit: descriptor.limit || 50, alignTurn: descriptor.align_turn }, signal)
    return true
  }

  private notifyLocal(sessionID: string): void {
    for (const listener of [...(this.listeners.get(sessionID) ?? [])]) listener()
  }

  /** The adapter is exposed only to Runtime wiring, never to page callers. */
  adapter(sessionID: string): SessionContentAdapter {
    return new SessionContentAdapter(sessionID)
  }
}

function itemKey(item: SessionContentItem): string {
  return JSON.stringify([item.key.turn_id, item.key.agent_iteration, item.key.item_id])
}

function mergeHistoryDescriptor(
  current: SessionContentHistoryWindow['descriptor'],
  next: SessionContentHistoryWindow['descriptor'],
  direction?: 'before' | 'after',
): SessionContentHistoryWindow['descriptor'] {
  if (!direction) return next
  if (direction === 'before') return {
    ...current,
    limit: Math.max(current.limit, next.limit),
    align_turn: next.align_turn,
    visible_only: next.visible_only,
    oldest_item_seq: next.oldest_item_seq ?? current.oldest_item_seq,
    newest_item_seq: current.newest_item_seq ?? next.newest_item_seq,
    before_item_seq: next.before_item_seq,
    has_more_before: next.has_more_before,
    has_more_after: current.has_more_after || next.has_more_after,
  }
  return {
    ...current,
    limit: Math.max(current.limit, next.limit),
    align_turn: next.align_turn,
    visible_only: next.visible_only,
    oldest_item_seq: current.oldest_item_seq ?? next.oldest_item_seq,
    newest_item_seq: next.newest_item_seq ?? current.newest_item_seq,
    after_item_seq: next.after_item_seq,
    has_more_before: current.has_more_before || next.has_more_before,
    has_more_after: next.has_more_after,
  }
}

export function selectSessionView(repository: SessionContentRepository, sessionID: string): SessionView {
  return repository.get(sessionID)
}

export function selectSessionRunState(repository: SessionContentRepository, sessionID: string): SessionRunState | undefined {
  return repository.getRunState(sessionID)
}
