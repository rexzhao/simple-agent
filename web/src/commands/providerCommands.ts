import type { JsonObject } from '../domain/json'

export interface ProviderCommandOptions {
  readonly signal?: AbortSignal
}

export interface ProviderCreateOptions extends ProviderCommandOptions {
  /** Caller-owned identity for a create whose outcome may be uncertain. */
  readonly operationID?: string
}

export interface ProviderReasoningConfig {
  readonly parameter?: string
  readonly default?: string
  readonly levels?: JsonObject
}

export interface ProviderPricingTier {
  readonly input_cache_hit?: number
  readonly input_cache_miss?: number
  readonly cache_write?: number
  readonly output?: number
}

export interface ProviderPricing {
  readonly input_cache_hit?: number
  readonly input_cache_miss?: number
  readonly cache_write?: number
  readonly output?: number
  readonly currency?: string
  readonly long_context_threshold?: number
  readonly long_context?: ProviderPricingTier | null
}

export type ProviderWriteMode = 'preserve' | 'replace'

/** Complete model target. Optional members are encoded as their explicit zero/empty target. */
export interface ProviderModelTarget {
  readonly profile: string
  readonly id?: string
  readonly type?: string
  readonly compatibility?: string
  readonly input?: readonly string[]
  readonly developer_role?: string
  readonly context_window?: number
  readonly input_limit?: number
  readonly output_limit?: number
  /** Preserve is the default for a model already present in the durable
   * provider file. Replace is explicit because the safe resource never
   * contains arbitrary request parameters. */
  readonly parameters_mode: ProviderWriteMode
  /** Stable durable identity used when a preserved model is renamed. It is
   * deliberately only a profile name, never the hidden parameter value. */
  readonly parameters_source_profile?: string
  readonly parameters?: JsonObject
  readonly reasoning_config?: ProviderReasoningConfig
  readonly pricing?: ProviderPricing | null
}

/** The API key is write-only. It is never present in a command result. */
export interface ProviderUpdateTarget {
  /** The safe resource projection is not a complete URL. Existing endpoint
   * components are preserved unless the user explicitly replaces them. */
  readonly base_url_mode: ProviderWriteMode
  readonly base_url: string
  readonly api_key?: string
  readonly keep_api_key?: boolean
  readonly auth_file_mode: ProviderWriteMode
  readonly auth_file?: string
  readonly request_timeout?: string
  readonly http_proxy_mode: ProviderWriteMode
  readonly http_proxy?: string
  readonly https_proxy_mode: ProviderWriteMode
  readonly https_proxy?: string
  readonly max_concurrent_requests?: number
  readonly models: readonly ProviderModelTarget[]
}

export interface ProviderMutationResult {
  readonly provider: string
  readonly status: 'applied'
  readonly changed: boolean
}

export interface ProviderCreateResult {
  readonly operation_id: string
  readonly provider: string
  readonly status: 'applied'
  readonly changed: boolean
}

export interface ProviderDefaultResult {
  readonly provider: string
  readonly model: string
  readonly status: 'applied'
}

export interface ProviderDiscoverModelsResult {
  readonly provider: string
  readonly models: readonly string[]
}

export interface ProviderCommands {
  createProvider(provider: string, target: ProviderUpdateTarget, options?: ProviderCreateOptions): Promise<ProviderCreateResult>
  update(provider: string, target: ProviderUpdateTarget, options?: ProviderCommandOptions): Promise<ProviderMutationResult>
  setDefault(provider: string, model: string, options?: ProviderCommandOptions): Promise<ProviderDefaultResult>
  discoverModels(provider: string, options?: ProviderCommandOptions): Promise<ProviderDiscoverModelsResult>
}
