import { LocalReplica, resourceKeyString } from './localReplica'
import type { SessionIndexData, SessionSummary } from './sessionIndexAdapter'
import { SyncReadError } from './errors'

export type SessionIndexReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface SessionIndexReadModel {
  readonly status: SessionIndexReadState
  readonly summaries: readonly SessionSummary[]
  readonly active: readonly SessionSummary[]
  readonly archived: readonly SessionSummary[]
  readonly error?: SyncReadError
}

export interface SessionIndexRepositoryOptions {
  maxCachedProjects?: number
  retry?: (projectID: string) => void
}

type ProjectListener = () => void
interface CachedModel {
  recordIdentity: unknown
  initialized: boolean
  readState: string
  error?: SyncReadError
  model: SessionIndexReadModel
}

const emptySummaries: readonly SessionSummary[] = Object.freeze([])

type SessionIndexResource = { type: 'session_index'; id: string }

function resourceFor(projectID: string): SessionIndexResource {
  return { type: 'session_index', id: projectID }
}

/**
 * Domain-only repository over the local replica. Its public snapshots contain
 * summaries and a typed read state, never sequence, replay, blob or socket
 * metadata. Listeners are project-scoped so a background project update does
 * not invalidate a currently rendered project's read model.
 */
export class SessionIndexRepository {
  readonly replica: LocalReplica
  private readonly projectListeners = new Map<string, Set<ProjectListener>>()
  private readonly models = new Map<string, CachedModel>()
  private readonly maxCachedProjects: number
  private readonly retryResource: ((projectID: string) => void) | undefined
  private readonly unsubscribeReplica: () => void

  constructor(replica = new LocalReplica(), options: SessionIndexRepositoryOptions = {}) {
    this.replica = replica
    this.maxCachedProjects = options.maxCachedProjects ?? 64
    this.retryResource = options.retry
    if (this.maxCachedProjects <= 0) throw new Error('maxCachedProjects must be positive')
    this.unsubscribeReplica = replica.subscribe((resource) => {
      if (resource.type !== 'session_index') return
      this.models.delete(resource.id)
      for (const listener of [...(this.projectListeners.get(resource.id) ?? [])]) listener()
    })
  }

  dispose(): void {
    this.unsubscribeReplica()
    this.projectListeners.clear()
    this.models.clear()
  }

  subscribeProject(projectID: string, listener: ProjectListener): () => void {
    const listeners = this.projectListeners.get(projectID) ?? new Set<ProjectListener>()
    listeners.add(listener)
    this.projectListeners.set(projectID, listeners)
    return () => {
      listeners.delete(listener)
      if (listeners.size === 0) this.projectListeners.delete(projectID)
    }
  }

  subscribeProjects(projectIDs: readonly string[], listener: ProjectListener): () => void {
    const releases = [...new Set(projectIDs)].map((projectID) => this.subscribeProject(projectID, listener))
    return () => releases.forEach((release) => release())
  }

  getProjectReadModel(projectID: string): SessionIndexReadModel {
    const resource = resourceFor(projectID)
    const record = this.replica.get<SessionIndexData>(resource)
    const cached = this.models.get(projectID)
    if (
      cached && cached.recordIdentity === record.value &&
      cached.initialized === record.initialized &&
      cached.readState === record.metadata.readState &&
      cached.error === record.metadata.error
    ) return cached.model

    const data = record.value
    const summaries = data ? data.orderedIDs.map((id) => data.summariesByID[id]) : emptySummaries
    const active = summaries.filter((summary) => !summary.archived)
    const archived = summaries.filter((summary) => summary.archived)
    const status: SessionIndexReadState = !record.initialized
      ? record.metadata.readState === 'error' ? 'error' : 'loading'
      : record.metadata.readState === 'error' ? 'stale' : record.metadata.readState
    const model: SessionIndexReadModel = { status, summaries, active, archived, error: record.metadata.error }
    this.models.set(projectID, {
      recordIdentity: record.value,
      initialized: record.initialized,
      readState: record.metadata.readState,
      error: record.metadata.error,
      model,
    })
    while (this.models.size > this.maxCachedProjects) {
      const oldest = this.models.keys().next().value as string | undefined
      if (!oldest) break
      this.models.delete(oldest)
    }
    return model
  }

  getSummary(projectID: string, sessionID: string): SessionSummary | undefined {
    const data = this.replica.get<SessionIndexData>(resourceFor(projectID)).value
    return data && Object.prototype.hasOwnProperty.call(data.summariesByID, sessionID)
      ? data.summariesByID[sessionID]
      : undefined
  }

  selectByID(projectID: string, sessionID: string): SessionSummary | undefined {
    return this.getSummary(projectID, sessionID)
  }

  retry(projectID: string): void {
    this.retryResource?.(projectID)
  }

  /** Useful for runtime tests without leaking metadata through read models. */
  hasProjectSnapshot(projectID: string): boolean {
    return this.replica.get(resourceFor(projectID)).initialized
  }

  resourceKey(projectID: string): string {
    return resourceKeyString(resourceFor(projectID))
  }
}

export function selectProjectSummaries(repository: SessionIndexRepository, projectID: string): readonly SessionSummary[] {
  return repository.getProjectReadModel(projectID).summaries
}

export function selectSessionSummary(repository: SessionIndexRepository, projectID: string, sessionID: string): SessionSummary | undefined {
  return repository.getSummary(projectID, sessionID)
}
