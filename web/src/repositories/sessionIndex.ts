export type SessionIndexStatus = 'idle' | 'queued' | 'running' | 'completed' | 'failed' | 'interrupted'
export type SessionIndexReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface SessionSummary {
  readonly session_id: string
  readonly project_id: string
  readonly parent_session_id: string | null
  readonly display_name: string
  readonly archived: boolean
  readonly status: SessionIndexStatus
  readonly run_id: string | null
  readonly resource_revision: string
  readonly updated_at: string
  readonly has_unread_result: boolean
}

export interface DomainReadError {
  readonly code: string
  readonly message: string
}

export interface SessionIndexReadModel {
  readonly status: SessionIndexReadState
  readonly summaries: readonly SessionSummary[]
  readonly active: readonly SessionSummary[]
  readonly archived: readonly SessionSummary[]
  readonly error?: DomainReadError
}

export type SessionIndexNavigationReadModels = Readonly<Record<string, SessionIndexReadModel>>

export interface SessionIndexCompletionObservation {
  readonly status: SessionIndexStatus
  readonly hasUnreadResult: boolean
  readonly runID: string | null
}

const terminalSessionStatuses = new Set<SessionIndexStatus>(['completed', 'failed', 'interrupted'])

/**
 * Session Index is the only navigation-level source for background completion.
 * A missing previous observation is intentionally not a transition: the first
 * snapshot may contain an old unread result and must not replay a notification.
 */
export function isBackgroundSessionCompletionTransition(
  previous: SessionIndexCompletionObservation | undefined,
  current: Pick<SessionSummary, 'session_id' | 'status' | 'has_unread_result'>,
  currentSessionID: string,
): boolean {
  if (!previous || current.session_id === currentSessionID) return false
  return (
    previous.status === 'running' && terminalSessionStatuses.has(current.status)
  ) || (!previous.hasUnreadResult && current.has_unread_result)
}

/** Keep status and unread updates for one run as one visible notice. */
export function sessionIndexCompletionNoticeKey(summary: Pick<SessionSummary, 'session_id' | 'run_id'>): string {
  return `${summary.session_id}\u0000${summary.run_id ?? 'no-run'}`
}

export interface SessionReadModel {
  readonly status: SessionIndexReadState
  readonly summary?: SessionSummary
  readonly error?: DomainReadError
}

/**
 * Narrow, transport-free source contract. The sync implementation satisfies
 * this structurally, while page-facing code only sees this domain contract.
 */
export interface SessionIndexSource {
  getProjectReadModel(projectID: string): SessionIndexReadModel
  getSummary(projectID: string, sessionID: string): SessionSummary | undefined
  subscribeProject(projectID: string, listener: () => void): () => void
  subscribeProjects?(projectIDs: readonly string[], listener: () => void): () => void
  retry?(projectID: string): void
}

export interface SessionIndexObservationOptions {
  readonly signal?: AbortSignal
  readonly timeoutMS?: number
}

export class SessionIndexObservationError extends Error {
  readonly code: 'timeout' | 'cancelled'

  constructor(code: 'timeout' | 'cancelled') {
    super(code === 'timeout' ? 'session index did not update in time' : 'session index observation was cancelled')
    this.name = 'SessionIndexObservationError'
    this.code = code
  }
}

interface ProjectCache {
  sourceModel: SessionIndexReadModel
  model: SessionIndexReadModel
}

interface SessionCache {
  status: SessionIndexReadState
  summary?: SessionSummary
  error?: DomainReadError
  model: SessionReadModel
}

export interface SessionIndexRepositoryOptions {
  maxCachedProjects?: number
}

function copyError(error: DomainReadError | undefined): DomainReadError | undefined {
  // Protocol/transport diagnostics stay in infrastructure logs.  The page
  // only receives a stable, safe recovery message.
  return error ? { code: 'unavailable', message: 'Session list is temporarily unavailable.' } : undefined
}

/**
 * Domain repository facade. It accepts only the narrow source contract, so
 * protocol keys, sequence values, Blob descriptors, and transport objects do
 * not cross the page-facing module boundary.
 */
export class SessionIndexRepository {
  private readonly source: SessionIndexSource
  private readonly maxCachedProjects: number
  private readonly projectCaches = new Map<string, ProjectCache>()
  private readonly sessionCaches = new Map<string, SessionCache>()
  private navigationCache: {
    key: string
    sourceModels: readonly SessionIndexReadModel[]
    model: SessionIndexNavigationReadModels
  } | null = null

  constructor(source: SessionIndexSource, options: SessionIndexRepositoryOptions = {}) {
    this.source = source
    this.maxCachedProjects = options.maxCachedProjects ?? 64
    if (this.maxCachedProjects <= 0) throw new Error('maxCachedProjects must be positive')
  }

  getProjectReadModel(projectID: string): SessionIndexReadModel {
    const sourceModel = this.source.getProjectReadModel(projectID)
    const cached = this.projectCaches.get(projectID)
    if (cached?.sourceModel === sourceModel) return cached.model
    const model: SessionIndexReadModel = {
      status: sourceModel.status,
      summaries: sourceModel.summaries,
      active: sourceModel.active,
      archived: sourceModel.archived,
      error: copyError(sourceModel.error),
    }
    this.projectCaches.set(projectID, { sourceModel, model })
    this.trimCaches()
    return model
  }

  getProjectReadModels(projectIDs: readonly string[]): SessionIndexNavigationReadModels {
    const ids = [...new Set(projectIDs)]
    const key = ids.join('\u0000')
    const sourceModels = ids.map((projectID) => this.getProjectReadModel(projectID))
    const cached = this.navigationCache
    if (cached && cached.key === key && cached.sourceModels.length === sourceModels.length && sourceModels.every((model, index) => model === cached.sourceModels[index])) {
      return cached.model
    }
    const models: Record<string, SessionIndexReadModel> = {}
    ids.forEach((projectID, index) => { models[projectID] = sourceModels[index] })
    const model = Object.freeze(models)
    this.navigationCache = { key, sourceModels, model }
    return model
  }

  getSummary(projectID: string, sessionID: string): SessionSummary | undefined {
    return this.source.getSummary(projectID, sessionID)
  }

  getSessionReadModel(projectID: string, sessionID: string): SessionReadModel {
    const project = this.getProjectReadModel(projectID)
    const summary = this.getSummary(projectID, sessionID)
    const key = `${projectID}\u0000${sessionID}`
    const cached = this.sessionCaches.get(key)
    if (cached && cached.status === project.status && cached.summary === summary && cached.error === project.error) return cached.model
    const model: SessionReadModel = {
      status: project.status,
      ...(summary ? { summary } : {}),
      ...(project.error ? { error: project.error } : {}),
    }
    this.sessionCaches.set(key, { status: project.status, summary, error: project.error, model })
    return model
  }

  subscribeProject(projectID: string, listener: () => void): () => void {
    return this.source.subscribeProject(projectID, listener)
  }

  subscribeProjects(projectIDs: readonly string[], listener: () => void): () => void {
    if (this.source.subscribeProjects) return this.source.subscribeProjects(projectIDs, listener)
    const releases = [...new Set(projectIDs)].map((projectID) => this.source.subscribeProject(projectID, listener))
    return () => releases.forEach((release) => release())
  }

  retry(projectID: string): void {
    this.source.retry?.(projectID)
  }

  waitFor(
    projectID: string,
    predicate: (model: SessionIndexReadModel) => boolean,
    options: SessionIndexObservationOptions = {},
  ): Promise<SessionIndexReadModel> {
    const timeoutMS = options.timeoutMS ?? 5000
    if (!Number.isFinite(timeoutMS) || timeoutMS <= 0) throw new Error('session observation timeout must be positive')
    const initial = this.getProjectReadModel(projectID)
    if (predicate(initial)) return Promise.resolve(initial)
    if (options.signal?.aborted) return Promise.reject(new SessionIndexObservationError('cancelled'))
    return new Promise((resolve, reject) => {
      let settled = false
      let timer: ReturnType<typeof globalThis.setTimeout> | undefined
      let release: (() => void) | undefined
      const finish = (action: () => void) => {
        if (settled) return
        settled = true
        if (timer !== undefined) globalThis.clearTimeout(timer)
        release?.()
        options.signal?.removeEventListener('abort', onAbort)
        action()
      }
      const onAbort = () => finish(() => reject(new SessionIndexObservationError('cancelled')))
      const onChange = () => {
        const model = this.getProjectReadModel(projectID)
        if (predicate(model)) finish(() => resolve(model))
      }
      release = this.subscribeProject(projectID, onChange)
      onChange()
      if (settled) return
      options.signal?.addEventListener('abort', onAbort, { once: true })
      timer = globalThis.setTimeout(() => finish(() => reject(new SessionIndexObservationError('timeout'))), timeoutMS)
      if (options.signal?.aborted) onAbort()
    })
  }

  subscribeSession(projectID: string, _sessionID: string, listener: () => void): () => void {
    // The source invalidates the project listener for any B update. The
    // session getSnapshot remains reference-stable for unchanged A, so
    // useSyncExternalStore will not commit an unnecessary A render.
    return this.source.subscribeProject(projectID, listener)
  }

  evictProject(projectID: string): void {
    this.projectCaches.delete(projectID)
    this.navigationCache = null
    for (const key of this.sessionCaches.keys()) {
      if (key.startsWith(`${projectID}\u0000`)) this.sessionCaches.delete(key)
    }
  }

  private trimCaches(): void {
    while (this.projectCaches.size > this.maxCachedProjects) {
      const oldest = this.projectCaches.keys().next().value as string | undefined
      if (!oldest) break
      this.evictProject(oldest)
    }
  }
}

export function selectProjectSummaries(repository: SessionIndexRepository, projectID: string): readonly SessionSummary[] {
  return repository.getProjectReadModel(projectID).summaries
}

export function selectSessionSummary(repository: SessionIndexRepository, projectID: string, sessionID: string): SessionSummary | undefined {
  return repository.getSummary(projectID, sessionID)
}

export function selectSessionReadModel(repository: SessionIndexRepository, projectID: string, sessionID: string): SessionReadModel {
  return repository.getSessionReadModel(projectID, sessionID)
}
