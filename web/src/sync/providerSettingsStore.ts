import { LocalReplica, resourceKeyString } from './localReplica'
import type { ProviderSettingsData, ProviderSettingsEntity, ProviderModelSettings } from './providerSettingsAdapter'
import { SyncReadError } from './errors'
import type {
  DomainReadError,
  ProviderModelDomain,
  ProviderPricingDomain,
  ProviderSettingsAvailability,
  ProviderSettingsDomain,
  ProviderSettingsReadModel,
  ProviderSettingsReadState,
  ProviderSettingsSource,
} from '../repositories/providerSettings'

const resource = { type: 'provider_settings' as const, id: 'server' }
const emptyProviders: readonly ProviderSettingsDomain[] = Object.freeze([])

interface CachedModel {
  value: unknown
  initialized: boolean
  readState: string
  error?: SyncReadError
  model: ProviderSettingsReadModel
}

function domainError(error: SyncReadError | undefined): DomainReadError | undefined {
  return error ? { code: error.code, message: error.message } : undefined
}

function domainModel(value: ProviderModelSettings): ProviderModelDomain {
  const pricing: ProviderPricingDomain | null = value.pricing ? {
    inputCacheHit: value.pricing.input_cache_hit,
    inputCacheMiss: value.pricing.input_cache_miss,
    cacheWrite: value.pricing.cache_write,
    output: value.pricing.output,
    currency: value.pricing.currency,
    longContextThreshold: value.pricing.long_context_threshold,
    longContext: value.pricing.long_context ? {
      inputCacheHit: value.pricing.long_context.input_cache_hit,
      inputCacheMiss: value.pricing.long_context.input_cache_miss,
      cacheWrite: value.pricing.long_context.cache_write,
      output: value.pricing.long_context.output,
    } : null,
  } : null
  return {
    profile: value.profile,
    id: value.id,
    type: value.type,
    compatibility: value.compatibility,
    input: value.input,
    developerRole: value.developer_role,
    contextWindow: value.context_window,
    inputLimit: value.input_limit,
    outputLimit: value.output_limit,
    reasoningConfig: {
      parameter: value.reasoning_config.parameter,
      default: value.reasoning_config.default,
      levels: value.reasoning_config.levels.map((level) => ({ name: level.name, value: level.value })),
    },
    pricing,
  }
}

function domainProvider(value: ProviderSettingsEntity): ProviderSettingsDomain {
  return {
    name: value.name,
    baseURL: value.base_url,
    apiKeyConfigured: value.api_key_configured,
    authFile: value.auth_file,
    requestTimeout: value.request_timeout,
    httpProxy: value.http_proxy,
    httpsProxy: value.https_proxy,
    maxConcurrentRequests: value.max_concurrent_requests,
    models: value.models.map(domainModel),
  }
}

/** Structured implementation of the page-facing source contract. The public
 * read model contains domain errors only; SyncReadError stays inside sync. */
export class ProviderSettingsStore implements ProviderSettingsSource {
  readonly replica: LocalReplica
  private readonly models = new Map<string, CachedModel>()
  private readonly listeners = new Set<() => void>()
  private readonly unsubscribeReplica: () => void

  constructor(replica = new LocalReplica()) {
    this.replica = replica
    this.unsubscribeReplica = replica.subscribe((changed) => {
      if (changed.type !== resource.type || changed.id !== resource.id) return
      this.models.delete(resourceKeyString(resource))
      for (const listener of [...this.listeners]) listener()
    })
  }

  dispose(): void { this.unsubscribeReplica(); this.listeners.clear(); this.models.clear() }
  subscribe(listener: () => void): () => void { this.listeners.add(listener); return () => this.listeners.delete(listener) }

  getSnapshot(): ProviderSettingsReadModel {
    const record = this.replica.get<ProviderSettingsData>(resource)
    const key = resourceKeyString(resource)
    const cached = this.models.get(key)
    if (cached && cached.value === record.value && cached.initialized === record.initialized && cached.readState === record.metadata.readState && cached.error === record.metadata.error) return cached.model
    const data = record.value
    const providers = data ? data.orderedProviderNames.map((name) => domainProvider(data.providersByName[name])) : emptyProviders
    const status: ProviderSettingsReadState = !record.initialized
      ? (record.metadata.readState === 'error' ? 'error' : 'loading')
      : (record.metadata.readState === 'error' ? 'stale' : record.metadata.readState)
    const error = domainError(record.metadata.error)
    const availability: ProviderSettingsAvailability = !record.initialized
      ? (record.metadata.readState === 'error' ? { status: 'error', error } : { status: 'loading' })
      : (record.metadata.readState === 'error' || record.metadata.readState === 'stale' || error ? { status: 'stale', error } : { status: 'ready' })
    const model: ProviderSettingsReadModel = {
      status,
      serverRoot: data?.server_root ?? '',
      configPath: data?.config_path ?? '',
      defaultProvider: data?.default_provider ?? '',
      defaultModel: data?.default_model ?? '',
      providers,
      availability,
      error,
    }
    this.models.set(key, { value: record.value, initialized: record.initialized, readState: record.metadata.readState, error: record.metadata.error, model })
    return model
  }

  getProvider(name: string): ProviderSettingsDomain | undefined {
    const data = this.replica.get<ProviderSettingsData>(resource).value
    return data && Object.prototype.hasOwnProperty.call(data.providersByName, name) ? domainProvider(data.providersByName[name]) : undefined
  }

  getModel(providerName: string, profile: string): ProviderModelDomain | undefined { return this.getProvider(providerName)?.models.find((model) => model.profile === profile) }
  hasSnapshot(): boolean { return this.replica.get(resource).initialized }
  resourceKey(): string { return resourceKeyString(resource) }
}
