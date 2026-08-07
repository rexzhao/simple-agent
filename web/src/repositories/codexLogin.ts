export type CodexLoginReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface CodexLoginReadError {
  readonly code: string
  readonly message: string
}

export interface CodexLoginAvailability {
  readonly status: CodexLoginReadState
  readonly error?: CodexLoginReadError
}

export type CodexLoginStatus = 'signed_out' | 'pending' | 'signed_in' | 'expired' | 'error'

export interface CodexLoginDomain {
  readonly provider: string
  readonly status: CodexLoginStatus
  readonly loginID: string
  readonly userCode: string
  readonly verificationURL: string
  readonly refreshable: boolean
  readonly errorCode: string
  readonly errorMessage: string
}

export interface CodexLoginReadModel {
  readonly status: CodexLoginReadState
  readonly provider: string
  readonly login: CodexLoginDomain | null
  readonly availability: CodexLoginAvailability
  readonly error?: CodexLoginReadError
}

export interface CodexLoginSource {
  subscribe(listener: () => void): () => void
  getSnapshot(provider: string): CodexLoginReadModel
}

interface CachedModel {
  sourceModel: CodexLoginReadModel
  model: CodexLoginReadModel
}

function copyError(error: CodexLoginReadError | undefined): CodexLoginReadError | undefined {
  return error ? { code: error.code, message: error.message } : undefined
}

function copyAvailability(availability: CodexLoginAvailability): CodexLoginAvailability {
  return { status: availability.status, error: copyError(availability.error) }
}

/** Page-facing facade. It contains no resource, protocol, cursor, or
 * transport concepts; the sync store is its only source implementation. */
export class CodexLoginRepository {
  private readonly source: CodexLoginSource
  private readonly cached = new Map<string, CachedModel>()

  constructor(source: CodexLoginSource) { this.source = source }

  subscribe(listener: () => void): () => void { return this.source.subscribe(listener) }

  getSnapshot(provider: string): CodexLoginReadModel {
    const sourceModel = this.source.getSnapshot(provider)
    const old = this.cached.get(provider)
    if (old?.sourceModel === sourceModel) return old.model
    const model: CodexLoginReadModel = {
      status: sourceModel.status,
      provider: sourceModel.provider,
      login: sourceModel.login,
      availability: copyAvailability(sourceModel.availability),
      error: copyError(sourceModel.error),
    }
    this.cached.set(provider, { sourceModel, model })
    return model
  }
}

export function selectCodexLogin(repository: CodexLoginRepository, provider: string): CodexLoginReadModel {
  return repository.getSnapshot(provider)
}
