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
}

interface CachedModel {
  sourceModel: ProviderSettingsReadModel
  model: ProviderSettingsReadModel
}

function copyError(error: DomainReadError | undefined): DomainReadError | undefined {
  return error ? { code: error.code, message: error.message } : undefined
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
}

export function selectProviderSettings(repository: ProviderSettingsRepository): ProviderSettingsReadModel { return repository.getSnapshot() }
export function selectProvider(repository: ProviderSettingsRepository, name: string): ProviderSettingsDomain | undefined { return repository.getProvider(name) }
export function selectProviderModel(repository: ProviderSettingsRepository, providerName: string, profile: string): ProviderModelDomain | undefined { return repository.getModel(providerName, profile) }
export function selectProviderSettingsAvailability(repository: ProviderSettingsRepository): ProviderSettingsAvailability { return repository.getSnapshot().availability }
