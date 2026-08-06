export type ProjectIndexReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface ProjectSummary {
  readonly id: string
  readonly root: string
  readonly display_name: string
  readonly archived: boolean
  readonly created_at: string
  readonly updated_at: string
}

export interface DomainReadError {
  readonly code: string
  readonly message: string
}

export interface ProjectIndexReadModel {
  readonly status: ProjectIndexReadState
  readonly summaries: readonly ProjectSummary[]
  readonly active: readonly ProjectSummary[]
  readonly archived: readonly ProjectSummary[]
  readonly error?: DomainReadError
}

export interface ProjectIndexSource {
  getSnapshot(): ProjectIndexReadModel
  getByID(id: string): ProjectSummary | undefined
  subscribe(listener: () => void): () => void
}

interface CachedModel {
  sourceModel: ProjectIndexReadModel
  model: ProjectIndexReadModel
}

function copyError(error: DomainReadError | undefined): DomainReadError | undefined {
  return error ? { code: error.code, message: error.message } : undefined
}

/**
 * Transport-free domain facade for the singleton project list. A page can
 * read snapshots and keyed summaries without knowing about snapshots versus
 * changes, replay, reconnects, or Blob descriptors.
 */
export class ProjectIndexRepository {
  private readonly source: ProjectIndexSource
  private cached?: CachedModel

  constructor(source: ProjectIndexSource) {
    this.source = source
  }

  getSnapshot(): ProjectIndexReadModel {
    const sourceModel = this.source.getSnapshot()
    if (this.cached?.sourceModel === sourceModel) return this.cached.model
    const model: ProjectIndexReadModel = {
      status: sourceModel.status,
      summaries: sourceModel.summaries,
      active: sourceModel.active,
      archived: sourceModel.archived,
      error: copyError(sourceModel.error),
    }
    this.cached = { sourceModel, model }
    return model
  }

  getByID(id: string): ProjectSummary | undefined {
    return this.source.getByID(id)
  }

  subscribe(listener: () => void): () => void {
    return this.source.subscribe(listener)
  }
}

export function selectProjectSummaries(repository: ProjectIndexRepository): readonly ProjectSummary[] {
  return repository.getSnapshot().summaries
}

export function selectProjectByID(repository: ProjectIndexRepository, id: string): ProjectSummary | undefined {
  return repository.getByID(id)
}
