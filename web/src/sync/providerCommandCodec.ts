import type { BlobDescriptor } from '../protocol/types'
import { isRFC3339Timestamp } from '../protocol/datetime'
import type { JsonObject } from '../domain/json'
import type {
  ModelCatalogModel,
  ModelCatalogSearchResult,
  ProviderDiscoverModelsResult,
  ProviderUpdateTarget,
} from '../commands/providerCommands'
import { isWellFormedString, utf8ByteLength } from '../domain/providerIdentity'
import type { BlobClient } from './blobClient'

export const maxProviderCommandArgumentBytes = 1 << 20
export const maxProviderCommandJSONDepth = 32
export const maxProviderCommandJSONFields = 16384
export const maxProviderCommandJSONCollectionLength = 4096
export const maxProviderWireInteger = 1_000_000_000

const maxProviderModels = 4096
const maxProviderModelBytes = 4096
const maxProviderResultBytes = 8 * 1024 * 1024
const allowedTargetKeys = ['base_url', 'api_key', 'keep_api_key', 'auth_file', 'request_timeout', 'http_proxy', 'https_proxy', 'max_concurrent_requests', 'models'] as const
const allowedTargetWriteModeKeys = ['base_url_mode', 'auth_file_mode', 'http_proxy_mode', 'https_proxy_mode'] as const
const allowedModelKeys = ['profile', 'id', 'type', 'compatibility', 'input', 'developer_role', 'context_window', 'input_limit', 'output_limit', 'parameters_mode', 'parameters_source_profile', 'parameters', 'reasoning_config', 'pricing'] as const
const allowedReasoningKeys = ['type', 'parameter', 'default', 'levels'] as const
const allowedPricingKeys = ['input_cache_hit', 'input_cache_miss', 'cache_write', 'output', 'currency', 'long_context_threshold', 'long_context'] as const
const allowedPricingTierKeys = ['input_cache_hit', 'input_cache_miss', 'cache_write', 'output'] as const

type RecordValue = Record<string, unknown>

function isRecord(value: unknown): value is RecordValue {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false
  const prototype = Object.getPrototypeOf(value)
  return prototype === Object.prototype || prototype === null
}

function hasOwn(value: RecordValue, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key)
}

function exactKeys(value: RecordValue, allowed: readonly string[]): boolean {
  return Object.keys(value).every((key) => allowed.includes(key))
}

function requiredString(value: unknown, allowEmpty = true): string {
  if (typeof value !== 'string' || !isWellFormedString(value) || (!allowEmpty && value.length === 0)) throw new Error('provider command string is invalid')
  return value
}

function optionalString(source: RecordValue, key: string, defaultValue = ''): string {
  if (!hasOwn(source, key)) return defaultValue
  return requiredString(source[key])
}

function canonicalConfigString(value: unknown, allowEmpty = true): string {
  const result = requiredString(value, allowEmpty)
  if (result !== result.trim()) throw new Error('provider command string has ambiguous whitespace')
  return result
}

function writeMode(value: unknown): 'preserve' | 'replace' {
  if (value !== 'preserve' && value !== 'replace') throw new Error('provider write mode is invalid')
  return value
}

function boundedInteger(value: unknown, allowZero = true): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || !Number.isSafeInteger(value) || (!allowZero && value <= 0) || value < 0 || value > maxProviderWireInteger) throw new Error('provider command integer is invalid')
  return value
}

function boundedNumber(value: unknown): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) throw new Error('provider command number is invalid')
  return value
}

function optionalInteger(source: RecordValue, key: string): number {
  return hasOwn(source, key) ? boundedInteger(source[key]) : 0
}

function optionalNumber(source: RecordValue, key: string): number {
  return hasOwn(source, key) ? boundedNumber(source[key]) : 0
}

interface JSONBounds { fields: number }

function validateJSONValue(value: unknown, depth: number, bounds: JSONBounds): void {
  if (depth > maxProviderCommandJSONDepth) throw new Error('provider command JSON is too deep')
  if (value === null || typeof value === 'boolean') return
  if (typeof value === 'string') {
    if (!isWellFormedString(value)) throw new Error('provider command JSON string is invalid')
    return
  }
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value))) throw new Error('provider command JSON number is invalid')
    return
  }
  if (Array.isArray(value)) {
    if (value.length > maxProviderCommandJSONCollectionLength) throw new Error('provider command JSON array is too large')
    for (const item of value) validateJSONValue(item, depth + 1, bounds)
    return
  }
  if (!isRecord(value)) throw new Error('provider command JSON value is invalid')
  const keys = Object.keys(value)
  if (keys.length > maxProviderCommandJSONCollectionLength) throw new Error('provider command JSON object is too large')
  bounds.fields += keys.length
  if (bounds.fields > maxProviderCommandJSONFields) throw new Error('provider command JSON has too many fields')
  for (const key of keys) {
    if (!isWellFormedString(key)) throw new Error('provider command JSON key is invalid')
    validateJSONValue(value[key], depth + 1, bounds)
  }
}

function validateWireJSON(value: unknown): void {
  const bounds: JSONBounds = { fields: 0 }
  validateJSONValue(value, 0, bounds)
  let encoded: string | undefined
  try { encoded = JSON.stringify(value) } catch { throw new Error('provider command JSON cannot be serialized') }
  if (encoded === undefined || utf8ByteLength(encoded) > maxProviderCommandArgumentBytes) throw new Error('provider command JSON is too large')
}

function knownObject(source: unknown, keys: readonly string[]): RecordValue {
  if (!isRecord(source) || !exactKeys(source, keys)) throw new Error('provider command object shape is invalid')
  return source
}

function encodeReasoning(value: unknown): JsonObject {
  if (value === undefined) return { type: '', parameter: '', default: '', levels: {} }
  const source = knownObject(value, allowedReasoningKeys)
  const levels = source.levels === undefined ? {} : source.levels
  if (!isRecord(levels)) throw new Error('provider reasoning levels are invalid')
  const result: JsonObject = {
    type: canonicalConfigString(optionalString(source, 'type')),
    parameter: canonicalConfigString(optionalString(source, 'parameter')),
    default: canonicalConfigString(optionalString(source, 'default')),
    levels: levels as JsonObject,
  }
  validateJSONValue(result.levels, 1, { fields: 0 })
  return result
}

function encodePricingTier(value: unknown): JsonObject | null {
  if (value === undefined || value === null) return null
  const source = knownObject(value, allowedPricingTierKeys)
  return {
    input_cache_hit: optionalNumber(source, 'input_cache_hit'),
    input_cache_miss: optionalNumber(source, 'input_cache_miss'),
    cache_write: optionalNumber(source, 'cache_write'),
    output: optionalNumber(source, 'output'),
  }
}

function encodePricing(value: unknown): JsonObject | null {
  if (value === undefined || value === null) return null
  const source = knownObject(value, allowedPricingKeys)
  return {
    input_cache_hit: optionalNumber(source, 'input_cache_hit'),
    input_cache_miss: optionalNumber(source, 'input_cache_miss'),
    cache_write: optionalNumber(source, 'cache_write'),
    output: optionalNumber(source, 'output'),
    currency: canonicalConfigString(optionalString(source, 'currency')),
    long_context_threshold: optionalInteger(source, 'long_context_threshold'),
    long_context: encodePricingTier(source.long_context),
  }
}

function encodeModel(value: unknown): JsonObject {
  const source = knownObject(value, allowedModelKeys)
  const profile = canonicalConfigString(source.profile, false)
  if (profile.trim() === '') throw new Error('provider model profile is invalid')
  const inputValue = source.input === undefined ? [] : source.input
  if (!Array.isArray(inputValue) || inputValue.length > maxProviderCommandJSONCollectionLength) throw new Error('provider model input is invalid')
  const input = inputValue.map((item) => {
    const modality = canonicalConfigString(item, true)
    if (modality !== '' && modality !== modality.toLowerCase()) throw new Error('provider model input is not canonical')
    return modality
  })
  const parametersMode = writeMode(source.parameters_mode)
  const hasParameters = hasOwn(source, 'parameters')
  if (parametersMode === 'preserve' && hasParameters) throw new Error('preserved provider model parameters must not be sent')
  if (parametersMode === 'replace' && !hasParameters) throw new Error('replacement provider model parameters are required')
  const parameters = hasParameters ? source.parameters : undefined
  if (parametersMode === 'replace' && !isRecord(parameters)) throw new Error('provider model parameters are invalid')
  const sourceProfile = hasOwn(source, 'parameters_source_profile') ? canonicalConfigString(source.parameters_source_profile, false) : ''
  if (parametersMode === 'preserve' && sourceProfile === '') throw new Error('preserved provider model identity is required')
  if (parametersMode === 'replace' && sourceProfile !== '') throw new Error('replacement provider model identity is invalid')
  const type = canonicalConfigString(optionalString(source, 'type'))
  const compatibility = canonicalConfigString(optionalString(source, 'compatibility'))
  const developerRole = canonicalConfigString(optionalString(source, 'developer_role'))
  if (compatibility !== '' && compatibility !== compatibility.toLowerCase()) throw new Error('provider model compatibility is not canonical')
  if (developerRole !== '' && developerRole !== developerRole.toLowerCase()) throw new Error('provider model developer role is not canonical')
  return {
    profile,
    id: canonicalConfigString(optionalString(source, 'id')),
    type,
    compatibility,
    input,
    developer_role: developerRole,
    context_window: optionalInteger(source, 'context_window'),
    input_limit: optionalInteger(source, 'input_limit'),
    output_limit: optionalInteger(source, 'output_limit'),
    parameters_mode: parametersMode,
    ...(parametersMode === 'preserve' ? { parameters_source_profile: sourceProfile } : { parameters: parameters as JsonObject }),
    reasoning_config: encodeReasoning(source.reasoning_config),
    pricing: encodePricing(source.pricing),
  }
}

export function validateProviderCommandJSON(value: unknown): void {
  validateWireJSON(value)
}

/** Encodes and validates the complete target before the facade submits it. */
export function encodeProviderTarget(target: ProviderUpdateTarget): JsonObject {
  const source = knownObject(target, [...allowedTargetKeys, ...allowedTargetWriteModeKeys])
  const modelsValue = source.models
  if (!Array.isArray(modelsValue) || modelsValue.length === 0 || modelsValue.length > maxProviderModels) throw new Error('provider model target list is invalid')
  const baseURLMode = writeMode(source.base_url_mode)
  // A preserve target intentionally carries no visible endpoint value. Only
  // an explicit replacement requires a non-empty safe endpoint.
  const baseURL = canonicalConfigString(source.base_url, baseURLMode === 'preserve')
  if (baseURL.trim() === '' && baseURLMode === 'replace') throw new Error('provider base URL is invalid')
  const authFileMode = writeMode(source.auth_file_mode)
  const httpProxyMode = writeMode(source.http_proxy_mode)
  const httpsProxyMode = writeMode(source.https_proxy_mode)
  const result: JsonObject = {
    base_url: baseURL,
    base_url_mode: baseURLMode,
    api_key: canonicalConfigString(optionalString(source, 'api_key')),
    keep_api_key: hasOwn(source, 'keep_api_key') ? source.keep_api_key as boolean : false,
    auth_file_mode: authFileMode,
    auth_file: canonicalConfigString(optionalString(source, 'auth_file')),
    request_timeout: canonicalConfigString(optionalString(source, 'request_timeout')),
    http_proxy_mode: httpProxyMode,
    http_proxy: canonicalConfigString(optionalString(source, 'http_proxy')),
    https_proxy_mode: httpsProxyMode,
    https_proxy: canonicalConfigString(optionalString(source, 'https_proxy')),
    max_concurrent_requests: optionalInteger(source, 'max_concurrent_requests'),
    models: modelsValue.map(encodeModel),
  }
  if (typeof result.keep_api_key !== 'boolean') throw new Error('provider keep_api_key is invalid')
  validateWireJSON(result)
  return result
}

export async function decodeProviderDiscoverResult(
  value: unknown,
  provider: string,
  blobClient?: BlobClient,
  signal?: AbortSignal,
): Promise<ProviderDiscoverModelsResult> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('provider result is not an object')
  const object = value as Record<string, unknown>
  if (typeof object.provider !== 'string' || object.provider !== provider) throw new Error('provider result identity is invalid')
  const hasModels = Object.prototype.hasOwnProperty.call(object, 'models')
  const hasBlob = Object.prototype.hasOwnProperty.call(object, 'blob')
  if (hasModels === hasBlob || Object.keys(object).length !== 2) throw new Error('provider result shape is invalid')
  if (hasModels) {
    if (!Array.isArray(object.models)) throw new Error('provider model result is invalid')
    return { provider, models: decodeModelIDs(object.models) }
  }
  const descriptor = decodeBlobDescriptor(object.blob)
  if (!blobClient) throw new Error('provider result blob client is unavailable')
  const blob = await blobClient.getJSON(descriptor, { signal })
  if (!Array.isArray(blob)) throw new Error('provider model blob is invalid')
  return { provider, models: decodeModelIDs(blob) }
}

function decodeModelIDs(value: unknown[]): string[] {
  if (value.length > maxProviderModels) throw new Error('provider model result is too large')
  const seen = new Set<string>()
  const result: string[] = []
  let total = 0
  let previous: string | undefined
  for (const item of value) {
    if (typeof item !== 'string' || item.length === 0 || !isWellFormedString(item)) throw new Error('provider model result is invalid')
    const size = utf8ByteLength(item)
    if (size > maxProviderModelBytes) throw new Error('provider model result is invalid')
    if (seen.has(item) || (previous !== undefined && compareUTF8(previous, item) >= 0)) throw new Error('provider model result is not strictly sorted')
    seen.add(item)
    result.push(item)
    previous = item
    total += size
    if (total > maxProviderResultBytes) throw new Error('provider model result is too large')
  }
  let encoded: string | undefined
  try { encoded = JSON.stringify(result) } catch { throw new Error('provider model result is invalid') }
  if (encoded === undefined || utf8ByteLength(encoded) > maxProviderResultBytes) throw new Error('provider model result is too large')
  return result
}

function compareUTF8(left: string, right: string): number {
  const leftBytes = new TextEncoder().encode(left)
  const rightBytes = new TextEncoder().encode(right)
  const length = Math.min(leftBytes.length, rightBytes.length)
  for (let index = 0; index < length; index += 1) {
    if (leftBytes[index] !== rightBytes[index]) return leftBytes[index] - rightBytes[index]
  }
  return leftBytes.length - rightBytes.length
}

function decodeBlobDescriptor(value: unknown): BlobDescriptor {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('provider blob descriptor is invalid')
  const object = value as Record<string, unknown>
  const keys = ['id', 'url', 'content_type', 'size', 'sha256', 'etag', 'expires_at']
  if (Object.keys(object).length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(object, key))) throw new Error('provider blob descriptor is invalid')
  const stringFields = ['id', 'url', 'content_type', 'sha256', 'etag', 'expires_at']
  if (stringFields.some((key) => typeof object[key] !== 'string' || !isWellFormedString(object[key] as string) || utf8ByteLength(object[key] as string) > maxProviderModelBytes)) throw new Error('provider blob descriptor is invalid')
  if (object.content_type !== 'application/json' || (object.id as string).trim() === '' || (object.url as string).trim() === '' || (object.sha256 as string).trim() === '' || (object.etag as string).trim() === '' || (object.expires_at as string).trim() === '') throw new Error('provider blob descriptor is invalid')
  if (!/^[0-9a-fA-F]{64}$/.test(object.sha256 as string) || /[\u0000-\u001f\u007f]/.test(object.url as string)) throw new Error('provider blob descriptor is invalid')
  let blobURL: URL
  try { blobURL = new URL(object.url as string, 'http://provider-command.invalid') } catch { throw new Error('provider blob descriptor is invalid') }
  if (blobURL.protocol !== 'http:' && blobURL.protocol !== 'https:') throw new Error('provider blob descriptor is invalid')
  if (blobURL.username || blobURL.password) throw new Error('provider blob descriptor is invalid')
  if (!Number.isSafeInteger(object.size) || (object.size as number) < 0 || (object.size as number) > maxProviderResultBytes) throw new Error('provider blob descriptor is invalid')
  if (!isRFC3339Timestamp(object.expires_at) || Date.now() >= Date.parse(object.expires_at as string)) throw new Error('provider blob descriptor is expired')
  return object as unknown as BlobDescriptor
}

export async function decodeModelCatalogSearchResult(
  value: unknown,
  query: string,
  blobClient?: BlobClient,
  signal?: AbortSignal,
): Promise<ModelCatalogSearchResult> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('model catalog result is not an object')
  const object = value as Record<string, unknown>
  if (Object.keys(object).length !== 2) throw new Error('model catalog result shape is invalid')
  if (typeof object.query !== 'string' || object.query !== query) throw new Error('model catalog result identity is invalid')
  const hasModels = Object.prototype.hasOwnProperty.call(object, 'models')
  const hasBlob = Object.prototype.hasOwnProperty.call(object, 'blob')
  if (hasModels === hasBlob) throw new Error('model catalog result shape is invalid')
  if (hasModels) {
    if (!Array.isArray(object.models)) throw new Error('model catalog result is invalid')
    return { query, models: decodeCatalogModels(object.models) }
  }
  const descriptor = decodeBlobDescriptor(object.blob)
  if (!blobClient) throw new Error('model catalog blob client is unavailable')
  const blob = await blobClient.getJSON(descriptor, { signal })
  if (!Array.isArray(blob)) throw new Error('model catalog blob is invalid')
  return { query, models: decodeCatalogModels(blob) }
}

const maxModelCatalogModels = 50
const maxModelCatalogFieldBytes = 4096

function decodeCatalogModels(value: unknown[]): ModelCatalogModel[] {
  if (value.length > maxModelCatalogModels) throw new Error('model catalog result is too large')
  return value.map((item) => decodeCatalogModel(item))
}

function decodeCatalogModel(item: unknown): ModelCatalogModel {
  if (item === null || typeof item !== 'object' || Array.isArray(item)) throw new Error('model catalog model is invalid')
  const object = item as Record<string, unknown>
  if (typeof object.id !== 'string' || object.id.length === 0 || !isWellFormedString(object.id) || utf8ByteLength(object.id) > maxModelCatalogFieldBytes) throw new Error('model catalog model id is invalid')
  if (typeof object.provider !== 'string' || !isWellFormedString(object.provider) || utf8ByteLength(object.provider) > maxModelCatalogFieldBytes) throw new Error('model catalog model provider is invalid')
  const reasoning = object.reasoning !== null && typeof object.reasoning === 'object'
    ? (() => {
        const source = object.reasoning as Record<string, unknown>
        const normalized: {
          enabled?: boolean
          effort_levels?: string[]
          supports_toggle?: boolean
          budget_min?: number | null
          budget_max?: number | null
        } = {}
        if (typeof source.enabled === 'boolean') normalized.enabled = source.enabled
        if (Array.isArray(source.effort_levels)) normalized.effort_levels = source.effort_levels.filter((item): item is string => typeof item === 'string')
        if (typeof source.supports_toggle === 'boolean') normalized.supports_toggle = source.supports_toggle
        if (source.budget_min === null || typeof source.budget_min === 'number') normalized.budget_min = source.budget_min as number | null
        if (source.budget_max === null || typeof source.budget_max === 'number') normalized.budget_max = source.budget_max as number | null
        return normalized
      })()
    : undefined
  const pricing = object.pricing !== null && typeof object.pricing === 'object'
    ? (() => {
        const source = object.pricing as Record<string, unknown>
        const normalized: {
          input?: number
          output?: number
          cache_read?: number
          cache_write?: number
          long_context_threshold?: number
          input_long?: number
          output_long?: number
          cache_read_long?: number
          cache_write_long?: number
        } = {}
        for (const key of ['input', 'output', 'cache_read', 'cache_write', 'input_long', 'output_long', 'cache_read_long', 'cache_write_long'] as const) {
          if (typeof source[key] === 'number' && Number.isFinite(source[key])) normalized[key] = source[key]
        }
        if (typeof source.long_context_threshold === 'number' && Number.isSafeInteger(source.long_context_threshold)) normalized.long_context_threshold = source.long_context_threshold
        return normalized
      })()
    : undefined
  return {
    id: object.id,
    name: typeof object.name === 'string' ? object.name : object.id,
    provider: object.provider,
    description: typeof object.description === 'string' ? object.description : undefined,
    context_window: typeof object.context_window === 'number' && Number.isSafeInteger(object.context_window) ? object.context_window : undefined,
    input_limit: typeof object.input_limit === 'number' && Number.isSafeInteger(object.input_limit) ? object.input_limit : undefined,
    output_limit: typeof object.output_limit === 'number' && Number.isSafeInteger(object.output_limit) ? object.output_limit : undefined,
    input: Array.isArray(object.input) ? object.input.filter((item): item is string => typeof item === 'string') : [],
    reasoning,
    pricing,
  }
}
