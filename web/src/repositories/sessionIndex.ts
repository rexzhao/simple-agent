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
  return error ? { code: error.code, message: error.message } : undefined
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

  subscribeSession(projectID: string, _sessionID: string, listener: () => void): () => void {
    // The source invalidates the project listener for any B update. The
    // session getSnapshot remains reference-stable for unchanged A, so
    // useSyncExternalStore will not commit an unnecessary A render.
    return this.source.subscribeProject(projectID, listener)
  }

  evictProject(projectID: string): void {
    this.projectCaches.delete(projectID)
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
