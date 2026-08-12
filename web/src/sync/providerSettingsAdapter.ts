import { isWellFormedString } from '../protocol/identifiers'
import { isDecimalString } from '../protocol/sequence'
import type { ChangeOperation, JsonObject, JsonValue } from '../protocol/types'
import type { ResourceAdapter } from './localReplica'

export interface ReasoningLevel {
  readonly name: string
  readonly value: ReasoningScalar
}

export type ReasoningScalar = string | number | boolean | null

export interface ReasoningMetadata {
  readonly type: string
  readonly parameter: string
  readonly default: string
  readonly levels: readonly ReasoningLevel[]
}

export interface PricingTierMetadata {
  readonly input_cache_hit: number
  readonly input_cache_miss: number
  readonly cache_write: number
  readonly output: number
}

export interface PricingMetadata {
  readonly input_cache_hit: number
  readonly input_cache_miss: number
  readonly cache_write: number
  readonly output: number
  readonly currency: string
  readonly long_context_threshold: number
  readonly long_context: PricingTierMetadata | null
}

export interface ProviderModelSettings {
  readonly profile: string
  readonly id: string
  readonly type: string
  readonly compatibility: string
  readonly input: readonly string[]
  readonly developer_role: string
  readonly context_window: number
  readonly input_limit: number
  readonly output_limit: number
  readonly reasoning_config: ReasoningMetadata
  readonly pricing: PricingMetadata | null
}

export interface ProviderSettingsEntity {
  readonly name: string
  readonly base_url: string
  readonly api_key_configured: boolean
  readonly auth_file: string
  readonly request_timeout: string
  readonly http_proxy: string
  readonly https_proxy: string
  readonly max_concurrent_requests: number
  readonly models: readonly ProviderModelSettings[]
}

/** Sync-only identity. The repository exposes it only as an opaque token. */
export interface ProviderAuthorityIdentity {
  readonly epoch: string
  readonly generation: number
  readonly revision: string
}

export interface ProviderSettingsData {
  readonly server_root: string
  readonly config_path: string
  readonly default_provider: string
  readonly default_model: string
  readonly providersByName: Readonly<Record<string, ProviderSettingsEntity>>
  readonly orderedProviderNames: readonly string[]
  /** Local authority publication identity. It is not part of the wire
   * snapshot and exists so a write that changes only hidden durable fields is
   * still observable by an application authority barrier. */
  readonly authorityRevision: ProviderAuthorityIdentity
  /** Per-provider publication identities are also local-only. The resource is
   * one document, but this prevents an unrelated provider publication from
   * satisfying a hidden-field barrier for the provider being saved. */
  readonly providerAuthorityRevisions: Readonly<Record<string, ProviderAuthorityIdentity>>
}

const MAX_PROVIDER_NAME_BYTES = 256
const MAX_WIRE_INTEGER = 1_000_000_000
const MAX_REASONING_STRING_BYTES = 256
const MAX_REASONING_NUMBER = Number.MAX_SAFE_INTEGER
const snapshotFields = ['server_root', 'config_path', 'default_provider', 'default_model', 'providers'] as const
const providerFields = ['name', 'base_url', 'api_key_configured', 'auth_file', 'request_timeout', 'http_proxy', 'https_proxy', 'max_concurrent_requests', 'models'] as const
const modelFields = ['profile', 'id', 'type', 'compatibility', 'input', 'developer_role', 'context_window', 'input_limit', 'output_limit', 'reasoning_config', 'pricing'] as const
const reasoningFields = ['type', 'parameter', 'default', 'levels'] as const
const levelFields = ['name', 'value'] as const
const pricingFields = ['input_cache_hit', 'input_cache_miss', 'cache_write', 'output', 'currency', 'long_context_threshold', 'long_context'] as const
const tierFields = ['input_cache_hit', 'input_cache_miss', 'cache_write', 'output'] as const

function own(value: object, key: string): boolean { return Object.prototype.hasOwnProperty.call(value, key) }
function record(value: unknown, message: string): Record<string, unknown> { if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error(message); return value as Record<string, unknown> }
function exactKeys(value: Record<string, unknown>, fields: readonly string[], message: string): void { const expected = new Set(fields); for (const key of Object.keys(value)) if (!expected.has(key)) throw new Error(`${message}: unknown field`); for (const key of fields) if (!own(value, key)) throw new Error(`${message}: missing field`) }
function stringValue(value: unknown, field: string, allowEmpty = false): string { if (typeof value !== 'string' || (!allowEmpty && value.trim() === '') || !isWellFormedString(value)) throw new Error(`${field} must be a valid string`); return value }
function integer(value: unknown, field: string): number { if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0 || value > MAX_WIRE_INTEGER) throw new Error(`${field} must be an integer between 0 and ${MAX_WIRE_INTEGER}`); return value }
function numberValue(value: unknown, field: string): number { if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) throw new Error(`${field} must be a finite non-negative number`); return value }
function strings(value: unknown, field: string): readonly string[] { if (!Array.isArray(value)) throw new Error(`${field} must be an array`); return value.map((item, index) => stringValue(item, `${field}[${index}]`, true)) }

function isGoSpace(code: number): boolean {
  return code === 0x0009 || code === 0x000a || code === 0x000b || code === 0x000c || code === 0x000d || code === 0x0020 || code === 0x0085 || code === 0x00a0 || code === 0x1680 || (code >= 0x2000 && code <= 0x200a) || code === 0x2028 || code === 0x2029 || code === 0x202f || code === 0x205f || code === 0x3000
}

function providerName(value: unknown, field: string, allowEmpty = false): string {
  if (typeof value !== 'string' || !isWellFormedString(value) || (!allowEmpty && value === '')) throw new Error(`${field} must be a valid provider name`)
  if (allowEmpty && value === '') return value
  const characters = [...value]
  if (value === '.' || value === '..' || new TextEncoder().encode(value).byteLength > MAX_PROVIDER_NAME_BYTES || isGoSpace(characters[0].codePointAt(0) as number) || isGoSpace(characters.at(-1)?.codePointAt(0) as number)) throw new Error(`${field} must be a valid provider name`)
  for (const character of characters) {
    const code = character.codePointAt(0) as number
    if (character === '/' || character === '\\' || code <= 0x1f || (code >= 0x7f && code <= 0x9f)) throw new Error(`${field} must be a valid provider name`)
  }
  return value
}

function reasoningScalar(value: unknown, field: string): ReasoningScalar {
  if (value === null) return null
  if (typeof value === 'string') {
    if (!isWellFormedString(value) || new TextEncoder().encode(value).byteLength > MAX_REASONING_STRING_BYTES) throw new Error(`${field} string exceeds maximum size`)
    return value
  }
  if (typeof value === 'number') {
    if (!Number.isFinite(value) || Math.abs(value) > MAX_REASONING_NUMBER) throw new Error(`${field} number exceeds safe precision boundary`)
    return value
  }
  if (typeof value === 'boolean') return value
  throw new Error(`${field} must be a JSON scalar`)
}

function reasoningText(value: unknown, field: string, allowEmpty = false): string {
  const result = stringValue(value, field, allowEmpty)
  if (new TextEncoder().encode(result).byteLength > MAX_REASONING_STRING_BYTES) throw new Error(`${field} exceeds maximum size`)
  return result
}

function reasoning(value: unknown): ReasoningMetadata {
  const source = record(value, 'reasoning_config must be an object'); exactKeys(source, reasoningFields, 'reasoning_config')
  if (!Array.isArray(source.levels)) throw new Error('reasoning levels must be an array')
  const levels = source.levels.map((raw, index) => { const level = record(raw, `reasoning level ${index} must be an object`); exactKeys(level, levelFields, 'reasoning level'); return { name: reasoningText(level.name, 'reasoning level name'), value: reasoningScalar(level.value, 'reasoning level value') } })
  const names = new Set<string>(); for (const level of levels) { if (names.has(level.name)) throw new Error('duplicate reasoning level'); names.add(level.name) }
  return { type: reasoningText(source.type, 'reasoning type', true), parameter: reasoningText(source.parameter, 'reasoning parameter', true), default: reasoningText(source.default, 'reasoning default', true), levels }
}

function pricingTier(value: unknown): PricingTierMetadata {
  const source = record(value, 'long_context must be an object'); exactKeys(source, tierFields, 'pricing tier')
  return { input_cache_hit: numberValue(source.input_cache_hit, 'input_cache_hit'), input_cache_miss: numberValue(source.input_cache_miss, 'input_cache_miss'), cache_write: numberValue(source.cache_write, 'cache_write'), output: numberValue(source.output, 'output') }
}

function pricing(value: unknown): PricingMetadata | null {
  if (value === null) return null
  const source = record(value, 'pricing must be an object'); exactKeys(source, pricingFields, 'pricing')
  return { input_cache_hit: numberValue(source.input_cache_hit, 'input_cache_hit'), input_cache_miss: numberValue(source.input_cache_miss, 'input_cache_miss'), cache_write: numberValue(source.cache_write, 'cache_write'), output: numberValue(source.output, 'output'), currency: stringValue(source.currency, 'currency', true), long_context_threshold: integer(source.long_context_threshold, 'long_context_threshold'), long_context: source.long_context === null ? null : pricingTier(source.long_context) }
}

function model(value: unknown): ProviderModelSettings {
  const source = record(value, 'provider model must be an object'); exactKeys(source, modelFields, 'provider model')
  return { profile: stringValue(source.profile, 'profile'), id: stringValue(source.id, 'id', true), type: stringValue(source.type, 'type', true), compatibility: stringValue(source.compatibility, 'compatibility', true), input: strings(source.input, 'input'), developer_role: stringValue(source.developer_role, 'developer_role', true), context_window: integer(source.context_window, 'context_window'), input_limit: integer(source.input_limit, 'input_limit'), output_limit: integer(source.output_limit, 'output_limit'), reasoning_config: reasoning(source.reasoning_config), pricing: pricing(source.pricing) }
}

function provider(value: unknown): ProviderSettingsEntity {
  const source = record(value, 'provider settings must be an object'); exactKeys(source, providerFields, 'provider settings')
  if (!Array.isArray(source.models)) throw new Error('models must be an array')
  const models = source.models.map(model); const profiles = new Set<string>(); for (const item of models) { if (profiles.has(item.profile)) throw new Error('duplicate model profile'); profiles.add(item.profile) }
  return { name: providerName(source.name, 'provider name'), base_url: stringValue(source.base_url, 'base_url', true), api_key_configured: typeof source.api_key_configured === 'boolean' ? source.api_key_configured : (() => { throw new Error('api_key_configured must be a boolean') })(), auth_file: stringValue(source.auth_file, 'auth_file', true), request_timeout: stringValue(source.request_timeout, 'request_timeout', true), http_proxy: stringValue(source.http_proxy, 'http_proxy', true), https_proxy: stringValue(source.https_proxy, 'https_proxy', true), max_concurrent_requests: integer(source.max_concurrent_requests, 'max_concurrent_requests'), models }
}

function equalArray(left: readonly string[], right: readonly string[]): boolean { return left.length === right.length && left.every((value, index) => value === right[index]) }
function equalReasoning(left: ReasoningMetadata, right: ReasoningMetadata): boolean { return left.parameter === right.parameter && left.default === right.default && left.levels.length === right.levels.length && left.levels.every((value, index) => value.name === right.levels[index].name && Object.is(value.value, right.levels[index].value)) }
function equalPricing(left: PricingMetadata | null, right: PricingMetadata | null): boolean { if (left === right) return true; if (!left || !right) return false; return left.input_cache_hit === right.input_cache_hit && left.input_cache_miss === right.input_cache_miss && left.cache_write === right.cache_write && left.output === right.output && left.currency === right.currency && left.long_context_threshold === right.long_context_threshold && ((!left.long_context && !right.long_context) || (!!left.long_context && !!right.long_context && Object.keys(left.long_context).every((key) => left.long_context![key as keyof PricingTierMetadata] === right.long_context![key as keyof PricingTierMetadata]))) }
function equalModel(left: ProviderModelSettings, right: ProviderModelSettings): boolean { return left.profile === right.profile && left.id === right.id && left.type === right.type && left.compatibility === right.compatibility && equalArray(left.input, right.input) && left.developer_role === right.developer_role && left.context_window === right.context_window && left.input_limit === right.input_limit && left.output_limit === right.output_limit && equalReasoning(left.reasoning_config, right.reasoning_config) && equalPricing(left.pricing, right.pricing) }
function equalProvider(left: ProviderSettingsEntity, right: ProviderSettingsEntity): boolean { return left.name === right.name && left.base_url === right.base_url && left.api_key_configured === right.api_key_configured && left.auth_file === right.auth_file && left.request_timeout === right.request_timeout && left.http_proxy === right.http_proxy && left.https_proxy === right.https_proxy && left.max_concurrent_requests === right.max_concurrent_requests && left.models.length === right.models.length && left.models.every((item, index) => equalModel(item, right.models[index])) }
function sameIdentity(left: ProviderAuthorityIdentity | undefined, right: ProviderAuthorityIdentity | undefined): boolean { return left === right || (!!left && !!right && left.epoch === right.epoch && left.generation === right.generation && left.revision === right.revision) }
function sameRevisionMap(left: Readonly<Record<string, ProviderAuthorityIdentity>>, right: Readonly<Record<string, ProviderAuthorityIdentity>>): boolean { const leftKeys = Object.keys(left).sort(); const rightKeys = Object.keys(right).sort(); return leftKeys.length === rightKeys.length && leftKeys.every((key, index) => key === rightKeys[index] && sameIdentity(left[key], right[key])) }
function makeData(serverRoot: string, configPath: string, defaultProvider: string, defaultModel: string, providersByName: Record<string, ProviderSettingsEntity>, authorityRevision: ProviderAuthorityIdentity, providerAuthorityRevisions: Record<string, ProviderAuthorityIdentity>, previous?: ProviderSettingsData): ProviderSettingsData { const orderedProviderNames = Object.keys(providersByName).sort(); if (previous && sameIdentity(previous.authorityRevision, authorityRevision) && sameRevisionMap(previous.providerAuthorityRevisions, providerAuthorityRevisions) && previous.server_root === serverRoot && previous.config_path === configPath && previous.default_provider === defaultProvider && previous.default_model === defaultModel && orderedProviderNames.length === previous.orderedProviderNames.length && orderedProviderNames.every((name, index) => name === previous.orderedProviderNames[index] && previous.providersByName[name] === providersByName[name])) return previous; return { server_root: serverRoot, config_path: configPath, default_provider: defaultProvider, default_model: defaultModel, providersByName, orderedProviderNames, authorityRevision, providerAuthorityRevisions } }

export class ProviderSettingsAdapter implements ResourceAdapter<ProviderSettingsData> {
  readonly resourceType = 'provider_settings' as const
  constructor(readonly resourceID = 'server') { if (resourceID !== 'server') throw new Error('provider settings resource id must be server') }
  validateResourceRevision(revision: string): void { if (!isDecimalString(revision)) throw new Error('resource_revision must be a decimal string') }
  decodeSnapshot(value: unknown, previous: ProviderSettingsData | undefined, context?: { streamEpoch?: string; resourceRevision: string; generation?: number }): ProviderSettingsData {
    const source = record(value, 'provider settings snapshot must be an object'); exactKeys(source, snapshotFields, 'provider settings snapshot'); if (!Array.isArray(source.providers)) throw new Error('providers must be an array')
    const providersByName: Record<string, ProviderSettingsEntity> = Object.create(null) as Record<string, ProviderSettingsEntity>
    const providerAuthorityRevisions: Record<string, ProviderAuthorityIdentity> = Object.create(null) as Record<string, ProviderAuthorityIdentity>
    const identity: ProviderAuthorityIdentity = { epoch: context?.streamEpoch ?? previous?.authorityRevision.epoch ?? '', generation: context?.generation ?? previous?.authorityRevision.generation ?? 0, revision: context?.resourceRevision ?? previous?.authorityRevision.revision ?? '' }
    for (const raw of source.providers) { const item = provider(raw); if (own(providersByName, item.name)) throw new Error('provider snapshot contains duplicate name'); const old = previous && own(previous.providersByName, item.name) ? previous.providersByName[item.name] : undefined; providersByName[item.name] = old && equalProvider(old, item) ? old : item; providerAuthorityRevisions[item.name] = identity }
    return makeData(stringValue(source.server_root, 'server_root', true), stringValue(source.config_path, 'config_path', true), providerName(source.default_provider, 'default provider', true), stringValue(source.default_model, 'default_model', true), providersByName, identity, providerAuthorityRevisions, previous)
  }
  applyChange(previous: ProviderSettingsData, operations: readonly ChangeOperation[], context?: { streamEpoch?: string; resourceRevision: string; generation?: number }): ProviderSettingsData {
    if (!Array.isArray(operations) || operations.length === 0) throw new Error('provider settings change must contain operations')
    const providersByName: Record<string, ProviderSettingsEntity> = Object.assign(Object.create(null) as Record<string, ProviderSettingsEntity>, previous.providersByName)
    const providerAuthorityRevisions: Record<string, ProviderAuthorityIdentity> = Object.assign(Object.create(null) as Record<string, ProviderAuthorityIdentity>, previous.providerAuthorityRevisions)
    const identity: ProviderAuthorityIdentity = { epoch: context?.streamEpoch ?? previous.authorityRevision.epoch, generation: context?.generation ?? previous.authorityRevision.generation, revision: context?.resourceRevision ?? previous.authorityRevision.revision }
    let defaultProvider = previous.default_provider; let defaultModel = previous.default_model
    for (const raw of operations) {
      const operation = record(raw, 'provider settings operation must be an object'); const op = stringValue(operation.op, 'operation op')
      if (op === 'upsert') { exactKeys(operation, ['op', 'key', 'value'], 'provider upsert'); const key = providerName(operation.key, 'upsert key'); const item = provider(operation.value); if (item.name !== key) throw new Error('upsert key does not match provider name'); const old = own(providersByName, key) ? providersByName[key] : undefined; providersByName[key] = old && equalProvider(old, item) ? old : item; providerAuthorityRevisions[key] = identity
      } else if (op === 'remove') { exactKeys(operation, ['op', 'key'], 'provider remove'); const key = providerName(operation.key, 'remove key'); delete providersByName[key]; delete providerAuthorityRevisions[key]
      } else if (op === 'default.replace') { exactKeys(operation, ['op', 'key', 'value'], 'provider default'); if (operation.key !== 'server') throw new Error('default key must be server'); const value = record(operation.value, 'default value must be an object'); exactKeys(value, ['provider', 'model'], 'default value'); defaultProvider = providerName(value.provider, 'default provider', true); defaultModel = stringValue(value.model, 'default model', true)
      } else throw new Error('provider settings operation is not supported')
    }
    return makeData(previous.server_root, previous.config_path, defaultProvider, defaultModel, providersByName, identity, providerAuthorityRevisions, previous)
  }
}

export function providerSettingsSnapshotValue(data: ProviderSettingsData): JsonObject {
  return { server_root: data.server_root, config_path: data.config_path, default_provider: data.default_provider, default_model: data.default_model, providers: data.orderedProviderNames.map((name) => data.providersByName[name]) as unknown as JsonValue[] }
}
