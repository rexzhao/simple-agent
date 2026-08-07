import { LocalReplica, resourceKeyString } from './localReplica'
import type { ProjectIndexData, ProjectSummary } from './projectIndexAdapter'
import { SyncReadError } from './errors'

export type ProjectIndexReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface ProjectIndexReadModel {
  readonly status: ProjectIndexReadState
  readonly summaries: readonly ProjectSummary[]
  readonly active: readonly ProjectSummary[]
  readonly archived: readonly ProjectSummary[]
  readonly error?: SyncReadError
}

const resource = { type: 'project_index' as const, id: 'server' }
const emptySummaries: readonly ProjectSummary[] = Object.freeze([])

interface CachedModel {
  recordIdentity: unknown
  initialized: boolean
  readState: string
  error?: SyncReadError
  model: ProjectIndexReadModel
}

/**
 * Sync-facing local projection for the singleton project_index resource. It
 * owns no socket and exposes no protocol cursor; SyncRuntime remains the only
 * component that applies snapshot/change/blob barriers to LocalReplica.
 */
export class ProjectIndexStore {
  readonly replica: LocalReplica
  private readonly models = new Map<string, CachedModel>()
  private readonly listeners = new Set<() => void>()
  private readonly unsubscribeReplica: () => void
  private readonly retryResource: (() => void) | undefined

  constructor(replica = new LocalReplica(), retry?: () => void) {
    this.replica = replica
    this.retryResource = retry
    this.unsubscribeReplica = replica.subscribe((changed) => {
      if (changed.type !== 'project_index' || changed.id !== 'server') return
      this.models.delete(resourceKeyString(resource))
      for (const listener of [...this.listeners]) listener()
    })
  }

  dispose(): void {
    this.unsubscribeReplica()
    this.listeners.clear()
    this.models.clear()
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  getSnapshot(): ProjectIndexReadModel {
    const record = this.replica.get<ProjectIndexData>(resource)
    const key = resourceKeyString(resource)
    const cached = this.models.get(key)
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
    const status: ProjectIndexReadState = !record.initialized
      ? record.metadata.readState === 'error' ? 'error' : 'loading'
      : record.metadata.readState === 'error' ? 'stale' : record.metadata.readState
    const model: ProjectIndexReadModel = { status, summaries, active, archived, error: record.metadata.error }
    this.models.set(key, {
      recordIdentity: record.value,
      initialized: record.initialized,
      readState: record.metadata.readState,
      error: record.metadata.error,
      model,
    })
    return model
  }

  getByID(id: string): ProjectSummary | undefined {
    const data = this.replica.get<ProjectIndexData>(resource).value
    return data && Object.prototype.hasOwnProperty.call(data.summariesByID, id) ? data.summariesByID[id] : undefined
  }

  /**
   * Navigation consumers use the project index as the set of projects whose
   * session indexes must stay live.  Keep this selector here rather than
   * making the interest policy depend on a React/current-project signal.
   */
  getActiveProjectIDs(): readonly string[] {
    return this.getSnapshot().active.map((project) => project.id)
  }

  hasSnapshot(): boolean {
    return this.replica.get(resource).initialized
  }

  resourceKey(): string {
    return resourceKeyString(resource)
  }

  retry(): void {
    this.retryResource?.()
  }
}
