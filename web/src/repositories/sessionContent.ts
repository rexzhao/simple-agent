import type {
  DataAvailability,
  SessionContentHistoryReadOptions,
  SessionContentHistoryState,
  SessionContentHistoryWindow,
  SessionRunState,
  SessionView,
} from '../domain/sessionContent'

export type { DataAvailability, DomainReadError, SessionContentMetadata, SessionContentActiveRun, SessionContentCompaction, SessionContentHistoryReadOptions, SessionContentHistoryState, SessionContentHistoryWindow, SessionRunState, SessionView } from '../domain/sessionContent'

export interface SessionContentSource {
  get(sessionID: string): SessionView
  observe(sessionID: string, listener: () => void): () => void
  readHistory?(sessionID: string, options?: SessionContentHistoryReadOptions, signal?: AbortSignal): Promise<SessionContentHistoryWindow>
  loadOlder?(sessionID: string, signal?: AbortSignal): Promise<boolean>
  historyState?(sessionID: string): SessionContentHistoryState
  retry?(sessionID: string): void
}

export interface SessionContentObservationOptions {
  readonly signal?: AbortSignal
  readonly timeoutMS?: number
}

/** Safe, typed outcome for an observation barrier. */
export class SessionContentObservationError extends Error {
  readonly code: 'timeout' | 'cancelled'

  constructor(code: 'timeout' | 'cancelled') {
    super(code === 'timeout' ? 'session content did not update in time' : 'session content observation was cancelled')
    this.name = 'SessionContentObservationError'
    this.code = code
  }
}

export interface SessionContentRepositoryOptions {
  readonly maxCachedSessions?: number
}

interface CachedView {
  readonly source: SessionView
  readonly view: SessionView
}

function copyError(view: SessionView): SessionView {
  const safeError = (error: { readonly code: string; readonly message: string }) => ({
    code: error.code,
    message: 'Session content synchronization is unavailable.',
  })
  const error = view.error ? safeError(view.error) : undefined
  const availability = view.availability.status === 'error'
    ? { ...view.availability, error: safeError(view.availability.error) }
    : view.availability
  const dataAvailability = view.dataAvailability.status === 'error'
    ? { ...view.dataAvailability, error: safeError(view.dataAvailability.error) }
    : view.dataAvailability
  const historyState = view.historyState.error
    ? { loading: view.historyState.loading, version: view.historyState.version, error: safeError(view.historyState.error) }
    : view.historyState
  return { ...view, ...(error ? { error } : {}), availability, dataAvailability, historyState }
}

/**
 * Page-facing domain facade. The structural source contract also allows the
 * sync repository to be supplied by application composition without exposing
 * its adapter, LocalReplica or protocol types to this module.
 */
export class SessionContentRepository {
  private readonly source: SessionContentSource
  private readonly maxCachedSessions: number
  private readonly caches = new Map<string, CachedView>()

  constructor(source: SessionContentSource, options: SessionContentRepositoryOptions = {}) {
    this.source = source
    this.maxCachedSessions = options.maxCachedSessions ?? 64
    if (this.maxCachedSessions <= 0) throw new Error('maxCachedSessions must be positive')
  }

  get(sessionID: string): SessionView {
    const source = this.source.get(sessionID)
    const cached = this.caches.get(sessionID)
    if (cached?.source === source) return cached.view
    const view = copyError(source)
    this.caches.set(sessionID, { source, view })
    while (this.caches.size > this.maxCachedSessions) {
      const oldest = this.caches.keys().next().value as string | undefined
      if (!oldest) break
      this.caches.delete(oldest)
    }
    return view
  }

  getSessionView(sessionID: string): SessionView {
    return this.get(sessionID)
  }

  getRunState(sessionID: string): SessionRunState | undefined {
    return this.get(sessionID).runState
  }

  select<T>(sessionID: string, selector: (view: SessionView) => T): T {
    return selector(this.get(sessionID))
  }

  observe(sessionID: string, listener: () => void): () => void {
    return this.source.observe(sessionID, listener)
  }

  subscribe(sessionID: string, listener: () => void): () => void {
    return this.observe(sessionID, listener)
  }

  readHistory(sessionID: string, options: SessionContentHistoryReadOptions = {}, signal?: AbortSignal): Promise<SessionContentHistoryWindow> {
    if (!this.source.readHistory) return Promise.reject(new Error('session history is unavailable'))
    return this.source.readHistory(sessionID, options, signal)
  }

  loadOlder(sessionID: string, signal?: AbortSignal): Promise<boolean> {
    if (!this.source.loadOlder) return Promise.resolve(false)
    return this.source.loadOlder(sessionID, signal)
  }

  historyState(sessionID: string): SessionContentHistoryState {
    const state = this.get(sessionID).historyState ?? this.source.historyState?.(sessionID) ?? { loading: false, version: 0 }
    return state.error
      ? { loading: state.loading, version: state.version, error: { code: state.error.code, message: 'Session history synchronization is unavailable.' } }
      : state
  }

  retry(sessionID: string): void {
    this.source.retry?.(sessionID)
  }

  /** Waits for the content repository to publish an authority change. */
  waitFor(
    sessionID: string,
    predicate: (view: SessionView) => boolean,
    options: SessionContentObservationOptions = {},
  ): Promise<SessionView> {
    const timeoutMS = options.timeoutMS ?? 5000
    if (!Number.isFinite(timeoutMS) || timeoutMS <= 0) throw new Error('session content observation timeout must be positive')
    const signal = options.signal
    const initial = this.get(sessionID)
    if (predicate(initial)) return Promise.resolve(initial)
    if (signal?.aborted) return Promise.reject(new SessionContentObservationError('cancelled'))
    return new Promise<SessionView>((resolve, reject) => {
      let timer: ReturnType<typeof setTimeout> | undefined
      let unsubscribe: (() => void) | undefined
      let settled = false
      const finish = (reason?: unknown) => {
        if (settled) return
        settled = true
        if (timer !== undefined) clearTimeout(timer)
        unsubscribe?.()
        signal?.removeEventListener('abort', onAbort)
        if (reason === undefined) resolve(this.get(sessionID))
        else reject(reason)
      }
      const check = () => {
        const view = this.get(sessionID)
        if (predicate(view)) finish()
      }
      const onAbort = () => finish(new SessionContentObservationError('cancelled'))
      unsubscribe = this.observe(sessionID, check)
      signal?.addEventListener('abort', onAbort, { once: true })
      if (signal?.aborted) return onAbort()
      check()
      if (!settled) timer = setTimeout(() => finish(new SessionContentObservationError('timeout')), timeoutMS)
    })
  }
}

export function selectSessionView(repository: SessionContentRepository, sessionID: string): SessionView {
  return repository.get(sessionID)
}

export function selectSessionRunState(repository: SessionContentRepository, sessionID: string): SessionRunState | undefined {
  return repository.getRunState(sessionID)
}

export function selectSessionAvailability(repository: SessionContentRepository, sessionID: string): DataAvailability {
  return repository.get(sessionID).availability
}
