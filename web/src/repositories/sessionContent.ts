import type {
  DataAvailability,
  SessionRunState,
  SessionView,
} from '../domain/sessionContent'

export type { DataAvailability, DomainReadError, SessionContentMetadata, SessionContentActiveRun, SessionContentCompaction, SessionContentHistoryWindow, SessionRunState, SessionView } from '../domain/sessionContent'

export interface SessionContentSource {
  get(sessionID: string): SessionView
  observe(sessionID: string, listener: () => void): () => void
}

export interface SessionContentRepositoryOptions {
  readonly maxCachedSessions?: number
}

interface CachedView {
  readonly source: SessionView
  readonly view: SessionView
}

function copyError(view: SessionView): SessionView {
  return view.error ? { ...view, error: { code: view.error.code, message: view.error.message } } : view
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
