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
  retry?(provider: string): void
}

export interface CodexLoginObservationOptions {
  readonly signal?: AbortSignal
  readonly timeoutMS?: number
}

export class CodexLoginObservationError extends Error {
  readonly code: 'timeout' | 'cancelled'

  constructor(code: 'timeout' | 'cancelled') {
    super(code === 'timeout' ? 'Codex login status did not update in time' : 'Codex login observation was cancelled')
    this.name = 'CodexLoginObservationError'
    this.code = code
  }
}

interface CachedModel {
  sourceModel: CodexLoginReadModel
  model: CodexLoginReadModel
}

function copyError(error: CodexLoginReadError | undefined): CodexLoginReadError | undefined {
  return error ? { code: 'unavailable', message: 'Codex login status is temporarily unavailable' } : undefined
}

function copyAvailability(availability: CodexLoginAvailability): CodexLoginAvailability {
  return { status: availability.status, error: copyError(availability.error) }
}

function copyLogin(login: CodexLoginDomain | null): CodexLoginDomain | null {
  if (!login) return null
  const error = login.status === 'error'
    ? { code: 'sign_in_failed', message: 'Codex sign-in failed.' }
    : login.status === 'expired'
      ? { code: 'session_expired', message: 'Codex sign-in has expired.' }
      : { code: '', message: '' }
  return {
    provider: login.provider,
    status: login.status,
    loginID: login.loginID,
    userCode: login.userCode,
    verificationURL: login.verificationURL,
    refreshable: login.refreshable,
    errorCode: error.code,
    errorMessage: error.message,
  }
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
      login: copyLogin(sourceModel.login),
      availability: copyAvailability(sourceModel.availability),
      error: copyError(sourceModel.error),
    }
    this.cached.set(provider, { sourceModel, model })
    return model
  }

  retry(provider: string): void { this.source.retry?.(provider) }

  waitFor(
    provider: string,
    predicate: (model: CodexLoginReadModel) => boolean,
    options: CodexLoginObservationOptions = {},
  ): Promise<CodexLoginReadModel> {
    const timeoutMS = options.timeoutMS ?? 5000
    if (!Number.isFinite(timeoutMS) || timeoutMS <= 0) throw new Error('Codex login observation timeout must be positive')
    const initial = this.getSnapshot(provider)
    if (predicate(initial)) return Promise.resolve(initial)
    if (options.signal?.aborted) return Promise.reject(new CodexLoginObservationError('cancelled'))

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
      const onAbort = () => finish(() => reject(new CodexLoginObservationError('cancelled')))
      const onChange = () => {
        const model = this.getSnapshot(provider)
        if (predicate(model)) finish(() => resolve(model))
      }
      release = this.subscribe(onChange)
      onChange()
      if (settled) return
      options.signal?.addEventListener('abort', onAbort, { once: true })
      timer = globalThis.setTimeout(() => finish(() => reject(new CodexLoginObservationError('timeout'))), timeoutMS)
      if (options.signal?.aborted) onAbort()
    })
  }
}

export function selectCodexLogin(repository: CodexLoginRepository, provider: string): CodexLoginReadModel {
  return repository.getSnapshot(provider)
}
