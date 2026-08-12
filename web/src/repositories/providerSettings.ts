export type ProviderSettingsReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface DomainReadError {
  readonly code: string
  readonly message: string
}

export interface ProviderSettingsAvailability {
  readonly status: ProviderSettingsReadState
  readonly error?: DomainReadError
}

export interface ProviderPricingTierDomain {
  readonly inputCacheHit: number
  readonly inputCacheMiss: number
  readonly cacheWrite: number
  readonly output: number
}

export interface ProviderPricingDomain extends ProviderPricingTierDomain {
  readonly currency: string
  readonly longContextThreshold: number
  readonly longContext: ProviderPricingTierDomain | null
}

export interface ProviderReasoningLevelDomain {
  readonly name: string
  readonly value: string | number | boolean | null
}

export interface ProviderModelDomain {
  readonly profile: string
  readonly id: string
  readonly type: string
  readonly compatibility: string
  readonly input: readonly string[]
  readonly developerRole: string
  readonly contextWindow: number
  readonly inputLimit: number
  readonly outputLimit: number
  readonly reasoningConfig: {
    readonly type: string
    readonly parameter: string
    readonly default: string
    readonly levels: readonly ProviderReasoningLevelDomain[]
  }
  readonly pricing: ProviderPricingDomain | null
}

export interface ProviderSettingsDomain {
  readonly name: string
  readonly baseURL: string
  readonly apiKeyConfigured: boolean
  readonly authFile: string
  readonly requestTimeout: string
  readonly httpProxy: string
  readonly httpsProxy: string
  readonly maxConcurrentRequests: number
  readonly models: readonly ProviderModelDomain[]
}

export interface ProviderSettingsReadModel {
  readonly status: ProviderSettingsReadState
  readonly serverRoot: string
  readonly configPath: string
  readonly defaultProvider: string
  readonly defaultModel: string
  readonly providers: readonly ProviderSettingsDomain[]
  readonly availability: ProviderSettingsAvailability
  readonly error?: DomainReadError
}

export interface ProviderSettingsSource {
  subscribe(listener: () => void): () => void
  getSnapshot(): ProviderSettingsReadModel
  getProvider(name: string): ProviderSettingsDomain | undefined
  getModel(providerName: string, profile: string): ProviderModelDomain | undefined
  retry?(): void
  /** Opaque application observation identity; its shape belongs to sync. */
  getAuthorityToken?(): unknown
}

export interface ProviderSettingsObservationOptions {
  readonly signal?: AbortSignal
  readonly timeoutMS?: number
}

export class ProviderSettingsObservationError extends Error {
  readonly code: 'timeout' | 'cancelled'

  constructor(code: 'timeout' | 'cancelled') {
    super(code === 'timeout' ? 'provider settings did not update in time' : 'provider settings observation was cancelled')
    this.name = 'ProviderSettingsObservationError'
    this.code = code
  }
}

interface CachedModel {
  sourceModel: ProviderSettingsReadModel
  model: ProviderSettingsReadModel
}

function copyError(error: DomainReadError | undefined): DomainReadError | undefined {
  return error ? { code: 'unavailable', message: 'Provider settings are temporarily unavailable' } : undefined
}

function copyAvailability(availability: ProviderSettingsAvailability): ProviderSettingsAvailability {
  return { status: availability.status, error: copyError(availability.error) }
}

/** Page-facing domain facade. It has no dependency on sync, protocol,
 * transport, sequence, subscription, Blob, or wire DTO types. */
export class ProviderSettingsRepository {
  private readonly source: ProviderSettingsSource
  private cached?: CachedModel

  constructor(source: ProviderSettingsSource) {
    this.source = source
  }

  subscribe(listener: () => void): () => void { return this.source.subscribe(listener) }

  getSnapshot(): ProviderSettingsReadModel {
    const sourceModel = this.source.getSnapshot()
    if (this.cached?.sourceModel === sourceModel) return this.cached.model
    const model: ProviderSettingsReadModel = {
      status: sourceModel.status,
      serverRoot: sourceModel.serverRoot,
      configPath: sourceModel.configPath,
      defaultProvider: sourceModel.defaultProvider,
      defaultModel: sourceModel.defaultModel,
      providers: sourceModel.providers,
      availability: copyAvailability(sourceModel.availability),
      error: copyError(sourceModel.error),
    }
    this.cached = { sourceModel, model }
    return model
  }

  getProvider(name: string): ProviderSettingsDomain | undefined { return this.source.getProvider(name) }
  getModel(providerName: string, profile: string): ProviderModelDomain | undefined { return this.source.getModel(providerName, profile) }

  captureAuthority(): unknown { return this.source.getAuthorityToken?.() }

  retry(): void { this.source.retry?.() }

  /** Waits for the provider resource publication which follows a typed
   * provider write. Execution tells the application whether this barrier is
   * needed; this repository only observes its opaque authority identity. */
  waitForProviderPublication(providerName: string, previous: unknown, options: ProviderSettingsObservationOptions = {}): Promise<ProviderSettingsReadModel> {
    return this.waitFor((model) => model.status === 'ready' && model.providers.some((item) => item.name === providerName) && authorityAdvanced(previous, this.source.getAuthorityToken?.(), providerName), options)
  }

  waitFor(
    predicate: (model: ProviderSettingsReadModel) => boolean,
    options: ProviderSettingsObservationOptions = {},
  ): Promise<ProviderSettingsReadModel> {
    const timeoutMS = options.timeoutMS ?? 5000
    if (!Number.isFinite(timeoutMS) || timeoutMS <= 0) throw new Error('provider settings observation timeout must be positive')
    const initial = this.getSnapshot()
    if (predicate(initial)) return Promise.resolve(initial)
    if (options.signal?.aborted) return Promise.reject(new ProviderSettingsObservationError('cancelled'))

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
      const onAbort = () => finish(() => reject(new ProviderSettingsObservationError('cancelled')))
      const onChange = () => {
        const model = this.getSnapshot()
        if (predicate(model)) finish(() => resolve(model))
      }
      release = this.subscribe(onChange)
      onChange()
      if (settled) return
      options.signal?.addEventListener('abort', onAbort, { once: true })
      timer = globalThis.setTimeout(() => finish(() => reject(new ProviderSettingsObservationError('timeout'))), timeoutMS)
      if (options.signal?.aborted) onAbort()
    })
  }
}

interface AuthorityToken {
  readonly epoch?: string
  readonly generation?: number
  readonly revision?: string
  readonly providers?: Readonly<Record<string, unknown>>
}

function authorityAdvanced(previous: unknown, current: unknown, providerName: string): boolean {
  const before = previous as AuthorityToken | undefined
  const after = current as AuthorityToken | undefined
  if (!after) return false
  const beforeProvider = before?.providers?.[providerName]
  const afterProvider = after.providers?.[providerName]
  if (beforeProvider !== undefined || afterProvider !== undefined) return !sameAuthorityToken(beforeProvider, afterProvider)
  return !sameAuthorityToken(before, after)
}

function sameAuthorityToken(left: unknown, right: unknown): boolean {
  if (left === right) return true
  if (!left || !right || typeof left !== 'object' || typeof right !== 'object') return false
  const a = left as Record<string, unknown>
  const b = right as Record<string, unknown>
  return a.epoch === b.epoch && a.generation === b.generation && a.revision === b.revision
}

export function selectProviderSettings(repository: ProviderSettingsRepository): ProviderSettingsReadModel { return repository.getSnapshot() }
export function selectProvider(repository: ProviderSettingsRepository, name: string): ProviderSettingsDomain | undefined { return repository.getProvider(name) }
export function selectProviderModel(repository: ProviderSettingsRepository, providerName: string, profile: string): ProviderModelDomain | undefined { return repository.getModel(providerName, profile) }
export function selectProviderSettingsAvailability(repository: ProviderSettingsRepository): ProviderSettingsAvailability { return repository.getSnapshot().availability }
