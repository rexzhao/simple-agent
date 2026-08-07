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
  /** Retry the resource through its owning sync runtime, when supported. */
  retry?(): void
}

export interface ProjectIndexObservationOptions {
  readonly signal?: AbortSignal
  readonly timeoutMS?: number
}

export class ProjectIndexObservationError extends Error {
  readonly code: 'timeout' | 'cancelled'

  constructor(code: 'timeout' | 'cancelled') {
    super(code === 'timeout' ? 'project index did not update in time' : 'project index observation was cancelled')
    this.name = 'ProjectIndexObservationError'
    this.code = code
  }
}

interface CachedModel {
  sourceModel: ProjectIndexReadModel
  model: ProjectIndexReadModel
}

function copyError(error: DomainReadError | undefined): DomainReadError | undefined {
  // Protocol/read-recovery details stay below the page boundary. The page can
  // distinguish a failed read from a successful one, but never renders a
  // stream, cursor, or transport diagnostic.
  return error ? { code: 'unavailable', message: 'Project list is temporarily unavailable' } : undefined
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

  retry(): void {
    this.source.retry?.()
  }

  /**
   * Waits for an authoritative repository publication. This is deliberately
   * subscription based: command acknowledgement never patches this read
   * model and the helper never polls an HTTP endpoint.
   */
  waitFor(
    predicate: (model: ProjectIndexReadModel) => boolean,
    options: ProjectIndexObservationOptions = {},
  ): Promise<ProjectIndexReadModel> {
    const timeoutMS = options.timeoutMS ?? 5000
    if (!Number.isFinite(timeoutMS) || timeoutMS <= 0) throw new Error('project observation timeout must be positive')
    const initial = this.getSnapshot()
    if (predicate(initial)) return Promise.resolve(initial)
    if (options.signal?.aborted) return Promise.reject(new ProjectIndexObservationError('cancelled'))

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
      const onAbort = () => finish(() => reject(new ProjectIndexObservationError('cancelled')))
      const onChange = () => {
        const model = this.getSnapshot()
        if (predicate(model)) finish(() => resolve(model))
      }
      release = this.subscribe(onChange)
      // Close the check/subscribe race without polling: a publication between
      // the initial read and listener registration is observed here.
      onChange()
      if (settled) return
      options.signal?.addEventListener('abort', onAbort, { once: true })
      timer = globalThis.setTimeout(() => finish(() => reject(new ProjectIndexObservationError('timeout'))), timeoutMS)
      if (options.signal?.aborted) onAbort()
    })
  }

  waitForProject(id: string, present: boolean, options: ProjectIndexObservationOptions = {}): Promise<ProjectIndexReadModel> {
    return this.waitFor((model) => {
      if (model.status === 'loading') return false
      return model.summaries.some((summary) => summary.id === id) === present
    }, options)
  }
}

export function selectProjectSummaries(repository: ProjectIndexRepository): readonly ProjectSummary[] {
  return repository.getSnapshot().summaries
}

export function selectProjectByID(repository: ProjectIndexRepository, id: string): ProjectSummary | undefined {
  return repository.getByID(id)
}
