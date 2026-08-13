import type { CommandMessage, CommandResultMessage, ErrorMessage, JsonObject, ProtocolMessage } from '../protocol/types'
import type {
  CommandOptions,
  SessionArchiveResult,
  SessionCommands,
  SessionDebugResult,
  SessionFullAccessResult,
  SessionMarkReadResult,
  SessionRenameResult,
  SessionCreateOptions,
  SessionCreateResult,
  SessionDeleteResult,
  SessionCompactResult,
  SessionHistoryReadOptions,
  SessionHistoryReadResult,
} from '../commands/sessionCommands'
import type { BlobDescriptor } from '../protocol/types'
import { isRFC3339Timestamp } from '../protocol/datetime'
import { isCanonicalWireIdentifier, isWellFormedString } from '../protocol/identifiers'
import type { ItemsPage } from '../types'
import type { RunCancelResult, RunCommands, RunContinueOptions, RunContinueResult, RunControlOptions, RunPromptAppendOptions, RunPromptAppendResult, RunPromptMoveResult, RunPromptRemoveResult, RunPromptSteerResult, RunStartOptions, RunStartResult, RunStatus, RunToolCancelResult } from '../commands/runCommands'
import type { ProjectArchiveResult, ProjectCommands, ProjectCreateOptions, ProjectCreateResult, ProjectDeleteResult, ProjectModelsResult, ProjectRenameResult } from '../commands/projectCommands'
import type { ProviderCodexUsageResult, ProviderCommands, ProviderCreateOptions, ProviderCreateResult, ProviderDefaultResult, ProviderDiscoverModelsResult, ModelCatalogSearchResult, ProviderMutationResult, ProviderUpdateTarget } from '../commands/providerCommands'
import type { CodexUsage, CodexUsageWindow, CodexUsageWindowSet, SessionModelOption } from '../types'
import type { CodexLoginClearResult, CodexLoginCommandOptions, CodexLoginCommands, CodexLoginStartResult } from '../commands/codexLoginCommands'
import { encodeProviderTarget, decodeProviderDiscoverResult, decodeModelCatalogSearchResult, validateProviderCommandJSON } from './providerCommandCodec'
import { isProviderCreateName, isProviderName } from '../domain/providerIdentity'
import { randomID } from '../lib/randomId'
import { SyncReadError } from './errors'
import type { RuntimeTransport } from './runtime'
import type { BlobClient } from './blobClient'

// Keep the original result type import path available while the typed command
// contracts live under the page-independent commands boundary.
export type { SessionMarkReadResult } from '../commands/sessionCommands'

export type CommandErrorCode = 'invalid' | 'capacity' | 'id_generation' | 'timeout' | 'cancelled' | 'stopped' | 'transport' | 'outcome_unknown' | string

export class CommandFacadeError extends Error {
  readonly code: CommandErrorCode
  readonly details?: unknown

  constructor(code: CommandErrorCode, message: string, details?: unknown) {
    super(message)
    this.name = 'CommandFacadeError'
    this.code = code
    this.details = details
  }
}

interface PendingCommand<T> {
  requestID: string
  message: CommandMessage
  crossEpochRetrySafe: boolean
  decodeResult: (value: unknown, signal?: AbortSignal) => T | PromiseLike<T>
  resolve: (value: T) => void
  reject: (reason: CommandFacadeError) => void
  timer: ReturnType<typeof globalThis.setTimeout>
  signal?: AbortSignal
  abortListener?: () => void
  sentGeneration?: number
  sentEpoch?: string
  decodeGeneration?: number
  decodeAbort?: AbortController
}

export interface CommandFacadeOptions {
  transport: RuntimeTransport
  blobClient?: BlobClient
  timeoutMS?: number
  maxPendingCommands?: number
  maxRecentRequestIDs?: number
  maxRecentEntityIDs?: number
  requestIDGenerator?: () => string
  sessionIDGenerator?: () => string
  runIDGenerator?: () => string
  operationIDGenerator?: () => string
  setTimeout?: (handler: () => void, timeout: number) => ReturnType<typeof globalThis.setTimeout>
  clearTimeout?: (handle: ReturnType<typeof globalThis.setTimeout>) => void
}

// randomID falls back to non-secure-context sources, so command IDs remain
// available when the app is served over plain HTTP on a LAN address.
function defaultRequestID(): string {
  return `request_${randomID()}`
}

function defaultSessionID(): string {
  return `session_${randomID()}`
}

function defaultRunID(): string {
  return `run_${randomID()}`
}

function defaultOperationID(): string {
  return `operation_${randomID()}`
}

function errorFromCommand(code: string, message: string, details?: unknown): CommandFacadeError {
  return new CommandFacadeError(code, message || 'command failed', details)
}

function nonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim() !== ''
}

function exactObject(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('result is not an object')
  const object = value as Record<string, unknown>
  const actual = Object.keys(object)
  if (actual.length !== keys.length || keys.some((key) => !Object.prototype.hasOwnProperty.call(object, key))) throw new Error('result has an unexpected shape')
  return object
}

function objectWithAllowedKeys(value: unknown, keys: readonly string[]): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('result is not an object')
  const object = value as Record<string, unknown>
  if (Object.keys(object).some((key) => !keys.includes(key))) throw new Error('result has an unexpected shape')
  return object
}

function resultString(object: Record<string, unknown>, key: string): string {
  const value = object[key]
  if (!nonEmptyString(value)) throw new Error('result string is invalid')
  return value
}

function resultBoolean(object: Record<string, unknown>, key: string): boolean {
  if (typeof object[key] !== 'boolean') throw new Error('result boolean is invalid')
  return object[key] as boolean
}

function validWireID(value: unknown): value is string {
  return isCanonicalWireIdentifier(value)
}

function resultIdentifier(object: Record<string, unknown>, key: string): string {
  if (!validWireID(object[key])) throw new Error('result identifier is invalid')
  return object[key] as string
}

function resultSafeInteger(object: Record<string, unknown>, key: string, min: number, max: number): number {
  const value = object[key]
  if (!Number.isSafeInteger(value) || (value as number) < min || (value as number) > max) throw new Error('result integer is invalid')
  return value as number
}

const historyItemKinds = new Set(['message', 'compaction', 'runtime_context'])
const historyVisibilities = new Set(['visible', 'hidden', 'debug'])
const historyAudiences = new Set(['user', 'model', 'internal'])
const historyStatuses = new Set(['pending', 'completed', 'error', 'interrupted'])
const historyRoles = new Set(['system', 'developer', 'user', 'assistant', 'tool', 'provider'])

function decodeDeleteResult(value: unknown, sessionID: string): SessionDeleteResult {
  const object = exactObject(value, ['session_id', 'status', 'removed_sessions'])
  const resultSessionID = resultString(object, 'session_id')
  const status = object.status
  const removedSessions = resultSafeInteger(object, 'removed_sessions', 1, Number.MAX_SAFE_INTEGER)
  if (resultSessionID !== sessionID || status !== 'removed') throw new Error('result identity does not match request')
  return { session_id: resultSessionID, status: 'removed', removed_sessions: removedSessions }
}

function decodeProjectCreateResult(value: unknown, operationID: string): ProjectCreateResult {
  const object = exactObject(value, ['operation_id', 'project_id', 'created'])
  const resultOperationID = resultIdentifier(object, 'operation_id')
  const projectID = resultIdentifier(object, 'project_id')
  if (resultOperationID !== operationID || typeof object.created !== 'boolean') throw new Error('result identity does not match request')
  return { operation_id: resultOperationID, project_id: projectID, created: object.created as boolean }
}

function decodeProjectRenameResult(value: unknown, projectID: string, displayName: string): ProjectRenameResult {
  const object = exactObject(value, ['project_id', 'display_name'])
  const resultProjectID = resultIdentifier(object, 'project_id')
  const resultDisplayName = object.display_name
  if (resultProjectID !== projectID || typeof resultDisplayName !== 'string' || resultDisplayName.trim() === '' || resultDisplayName !== displayName || !isWellFormedString(resultDisplayName) || new TextEncoder().encode(resultDisplayName).byteLength > 4096) throw new Error('result identity does not match request')
  return { project_id: resultProjectID, display_name: resultDisplayName }
}

function decodeProjectArchiveResult(value: unknown, projectID: string, archived: boolean): ProjectArchiveResult {
  const object = exactObject(value, ['project_id', 'archived'])
  const resultProjectID = resultIdentifier(object, 'project_id')
  if (resultProjectID !== projectID || object.archived !== archived) throw new Error('result identity does not match request')
  return { project_id: resultProjectID, archived }
}

function decodeProjectDeleteResult(value: unknown, projectID: string): ProjectDeleteResult {
  const object = exactObject(value, ['project_id', 'status', 'removed_sessions'])
  const resultProjectID = resultIdentifier(object, 'project_id')
  const removedSessions = resultSafeInteger(object, 'removed_sessions', 0, Number.MAX_SAFE_INTEGER)
  if (resultProjectID !== projectID || object.status !== 'removed') throw new Error('result identity does not match request')
  return { project_id: resultProjectID, status: 'removed', removed_sessions: removedSessions }
}

function decodeProviderMutationResult(value: unknown, provider: string): ProviderMutationResult {
  const object = exactObject(value, ['provider', 'status', 'changed'])
  const resultProvider = resultString(object, 'provider')
  if (resultProvider !== provider || object.status !== 'applied' || typeof object.changed !== 'boolean') throw new Error('provider result identity does not match request')
  return { provider: resultProvider, status: 'applied', changed: object.changed }
}

function decodeProviderCreateResult(value: unknown, provider: string, operationID: string): ProviderCreateResult {
  const object = exactObject(value, ['operation_id', 'provider', 'status', 'changed'])
  const resultOperationID = resultIdentifier(object, 'operation_id')
  const resultProvider = resultString(object, 'provider')
  if (resultOperationID !== operationID || resultProvider !== provider || object.status !== 'applied' || typeof object.changed !== 'boolean') throw new Error('provider create result identity does not match request')
  return { operation_id: resultOperationID, provider: resultProvider, status: 'applied', changed: object.changed }
}

function decodeProviderDefaultResult(value: unknown, provider: string, model: string): ProviderDefaultResult {
  const object = exactObject(value, ['provider', 'model', 'status'])
  const resultProvider = resultString(object, 'provider')
  const resultModel = resultString(object, 'model')
  if (resultProvider !== provider || resultModel !== model || object.status !== 'applied') throw new Error('provider result identity does not match request')
  return { provider: resultProvider, model: resultModel, status: 'applied' }
}

function decodeCodexLoginResult(value: unknown, provider: string, status: 'accepted' | 'cleared'): CodexLoginStartResult | CodexLoginClearResult {
  const object = exactObject(value, ['provider', 'status'])
  const resultProvider = resultString(object, 'provider')
  if (resultProvider !== provider || object.status !== status) throw new Error('Codex login result identity does not match request')
  return status === 'accepted' ? { provider: resultProvider, status } : { provider: resultProvider, status }
}

function decodeCompactResult(value: unknown, sessionID: string): SessionCompactResult {
  const object = exactObject(value, ['session_id', 'status', 'compaction_id', 'summary_item_id', 'revision'])
  const resultSessionID = resultString(object, 'session_id')
  const compactionID = resultIdentifier(object, 'compaction_id')
  const summaryItemID = resultIdentifier(object, 'summary_item_id')
  const revision = resultString(object, 'revision')
  if (resultSessionID !== sessionID || object.status !== 'committed' || !/^\d+$/.test(revision)) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, status: 'committed', compaction_id: compactionID, summary_item_id: summaryItemID, revision }
}

function decodeBlobDescriptor(value: unknown): BlobDescriptor {
  const object = exactObject(value, ['id', 'url', 'content_type', 'size', 'sha256', 'etag', 'expires_at'])
  const descriptor: BlobDescriptor = {
    id: resultString(object, 'id'),
    url: resultString(object, 'url'),
    content_type: resultString(object, 'content_type'),
    size: resultSafeInteger(object, 'size', 0, 16 * 1024 * 1024),
    sha256: resultString(object, 'sha256'),
    etag: resultString(object, 'etag'),
    expires_at: resultString(object, 'expires_at'),
  }
  if (!isRFC3339Timestamp(descriptor.expires_at)) throw new Error('result blob expiry is invalid')
  const expiresAt = Date.parse(descriptor.expires_at)
  if (!Number.isFinite(expiresAt) || Date.now() >= expiresAt) throw new Error('result blob is expired')
  return descriptor
}

function decodeHistoryPage(value: unknown): ItemsPage {
  const object = exactObject(value, ['items', 'oldest_seq', 'newest_seq', 'has_more_before', 'has_more_after'])
  if (!Array.isArray(object.items) || object.items.length > 4096) throw new Error('result history is invalid')
  const oldestSeq = resultSafeInteger(object, 'oldest_seq', 0, Number.MAX_SAFE_INTEGER)
  const newestSeq = resultSafeInteger(object, 'newest_seq', 0, Number.MAX_SAFE_INTEGER)
  if (typeof object.has_more_before !== 'boolean' || typeof object.has_more_after !== 'boolean') throw new Error('result history is invalid')
  for (const item of object.items) {
    if (item === null || typeof item !== 'object' || Array.isArray(item)) throw new Error('result history item is invalid')
    const itemObject = objectWithAllowedKeys(item, ['seq', 'id', 'turn_id', 'agent_iteration', 'created_at', 'kind', 'visibility', 'audience', 'status', 'message'])
    if (!validWireID(itemObject.id) || !Number.isSafeInteger(itemObject.seq) || (itemObject.seq as number) < 1 || typeof itemObject.created_at !== 'string' || !isRFC3339Timestamp(itemObject.created_at) || typeof itemObject.kind !== 'string' || !historyItemKinds.has(itemObject.kind) || typeof itemObject.visibility !== 'string' || !historyVisibilities.has(itemObject.visibility) || typeof itemObject.audience !== 'string' || !historyAudiences.has(itemObject.audience)) {
      throw new Error('result history item is invalid')
    }
    if (itemObject.turn_id !== undefined && !validWireID(itemObject.turn_id)) throw new Error('result history item is invalid')
    if (itemObject.agent_iteration !== undefined && (!Number.isSafeInteger(itemObject.agent_iteration) || (itemObject.agent_iteration as number) < 0)) throw new Error('result history item is invalid')
    if (itemObject.status !== undefined && (!nonEmptyString(itemObject.status) || !historyStatuses.has(itemObject.status))) throw new Error('result history item is invalid')
    if (itemObject.message !== undefined) {
      const message = objectWithAllowedKeys(itemObject.message, ['role', 'content', 'reasoning', 'images', 'tool_call_id', 'tool_calls', 'is_error'])
      if (!nonEmptyString(message.role) || !historyRoles.has(message.role)) throw new Error('result history message is invalid')
      if (message.content !== undefined) {
        const content = objectWithAllowedKeys(message.content, ['inline', 'preview'])
        if (content.inline !== undefined && (typeof content.inline !== 'string' || !isWellFormedString(content.inline))) throw new Error('result history content is invalid')
        if (content.preview !== undefined && (typeof content.preview !== 'string' || !isWellFormedString(content.preview))) throw new Error('result history content is invalid')
      }
      if (message.reasoning !== undefined && (typeof message.reasoning !== 'string' || !isWellFormedString(message.reasoning))) throw new Error('result history reasoning is invalid')
      if (message.tool_call_id !== undefined && !validWireID(message.tool_call_id)) throw new Error('result history tool call is invalid')
      if (message.is_error !== undefined && typeof message.is_error !== 'boolean') throw new Error('result history error flag is invalid')
      if (message.images !== undefined) {
        if (!Array.isArray(message.images) || message.images.length > 64) throw new Error('result history images are invalid')
        for (const image of message.images) {
          const imageObject = objectWithAllowedKeys(image, ['hash', 'media_type', 'size_bytes'])
          if (!validWireID(imageObject.hash) || !nonEmptyString(imageObject.media_type) || !Number.isSafeInteger(imageObject.size_bytes) || (imageObject.size_bytes as number) < 0) throw new Error('result history image is invalid')
        }
      }
      if (message.tool_calls !== undefined) {
        if (!Array.isArray(message.tool_calls) || message.tool_calls.length > 64) throw new Error('result history tool calls are invalid')
        for (const toolCall of message.tool_calls) {
          const toolCallObject = objectWithAllowedKeys(toolCall, ['id', 'name', 'arguments'])
          if (!validWireID(toolCallObject.id) || !validWireID(toolCallObject.name) || (toolCallObject.arguments !== undefined && (typeof toolCallObject.arguments !== 'string' || !isWellFormedString(toolCallObject.arguments)))) throw new Error('result history tool call is invalid')
        }
      }
    }
  }
  return {
    items: object.items as ItemsPage['items'],
    oldest_seq: oldestSeq,
    newest_seq: newestSeq,
    has_more_before: object.has_more_before as boolean,
    has_more_after: object.has_more_after as boolean,
  }
}

function decodeHistoryReadResult(
  value: unknown,
  sessionID: string,
  expectedCursor: number,
  expectedDirection: '' | 'before' | 'after',
  expectedLimit: number,
  expectedAlignTurn: boolean,
  blobClient?: BlobClient,
  signal?: AbortSignal,
): SessionHistoryReadResult | Promise<SessionHistoryReadResult> {
  const object = exactObject(value, ['session_id', 'cursor', 'direction', 'limit', 'align_turn', 'history', 'blob'])
  const resultSessionID = resultString(object, 'session_id')
  const cursor = resultSafeInteger(object, 'cursor', 0, Number.MAX_SAFE_INTEGER)
  const direction = object.direction
  const limit = resultSafeInteger(object, 'limit', 1, 200)
  if (resultSessionID !== sessionID || cursor !== expectedCursor || direction !== expectedDirection || limit !== expectedLimit || object.align_turn !== expectedAlignTurn || (direction !== '' && direction !== 'before' && direction !== 'after') || typeof object.align_turn !== 'boolean') throw new Error('result identity does not match request')
  if (cursor === 0 && direction !== '') throw new Error('result cursor is invalid')
  if (cursor > 0 && direction === '') throw new Error('result cursor is invalid')
  const hasHistory = object.history !== null
  const hasBlob = object.blob !== null
  if (hasHistory === hasBlob) throw new Error('result history payload is invalid')
  const history = hasHistory ? decodeHistoryPage(object.history) : null
  const blob = hasBlob ? decodeBlobDescriptor(object.blob) : null
  if (blob !== null && blob.content_type !== 'application/json') throw new Error('result history blob content type is invalid')
  const result = {
    session_id: resultSessionID,
    cursor,
    direction: direction as '' | 'before' | 'after',
    limit,
    align_turn: object.align_turn as boolean,
    history,
    blob,
  }
  if (!blob) return result
  if (!blobClient) throw new Error('result history blob has no data-plane client')
  // Resolve the data-plane payload inside the typed command boundary. A page
  // receives a normal history page and never receives or parses a descriptor.
  return blobClient.getJSON(blob, { signal }).then((payload) => ({ ...result, history: decodeHistoryPage(payload) }))
}

const maxSessionModelResultItems = 4096
const maxSessionModelResultStringBytes = 4096
const maxSessionModelResultBytes = 8 * 1024 * 1024
const maxReadBlobBytes = 8 * 1024 * 1024

function encodedResultBytes(value: unknown, label: string, maxBytes: number): number {
  let encoded: string | undefined
  try { encoded = JSON.stringify(value) } catch { throw new Error(`${label} is invalid`) }
  if (encoded === undefined) throw new Error(`${label} is invalid`)
  const size = new TextEncoder().encode(encoded).byteLength
  if (size > maxBytes) throw new Error(`${label} is too large`)
  return size
}

function boundedResultString(value: unknown, field: string, maxBytes = maxSessionModelResultStringBytes): string {
  if (typeof value !== 'string' || !isWellFormedString(value) || new TextEncoder().encode(value).byteLength > maxBytes) throw new Error(`${field} is invalid`)
  return value
}

function decodeSessionModelOptions(value: unknown): SessionModelOption[] {
  if (!Array.isArray(value) || value.length > maxSessionModelResultItems) throw new Error('session model result is invalid')
  const result = value.map((item) => {
    const object = objectWithAllowedKeys(item, ['provider', 'model_profile', 'model_id', 'reasoning_levels', 'default_reasoning_level'])
    const option: SessionModelOption = {
      provider: boundedResultString(object.provider, 'provider'),
      model_profile: boundedResultString(object.model_profile, 'model_profile'),
      model_id: boundedResultString(object.model_id, 'model_id'),
    }
    if (object.reasoning_levels !== undefined) {
      if (!Array.isArray(object.reasoning_levels) || object.reasoning_levels.length > 256) throw new Error('session model reasoning levels are invalid')
      option.reasoning_levels = object.reasoning_levels.map((level) => boundedResultString(level, 'reasoning_level'))
    }
    if (object.default_reasoning_level !== undefined) option.default_reasoning_level = boundedResultString(object.default_reasoning_level, 'default_reasoning_level')
    return option
  })
  let encoded: string | undefined
  try { encoded = JSON.stringify(result) } catch { throw new Error('session model result is invalid') }
  if (encoded === undefined || new TextEncoder().encode(encoded).byteLength > maxSessionModelResultBytes) throw new Error('session model result is too large')
  return result
}

function decodeProjectModelsResult(value: unknown, projectID: string, blobClient?: BlobClient, signal?: AbortSignal): ProjectModelsResult | Promise<ProjectModelsResult> {
  const object = exactObject(value, ['project_id', 'models', 'default_provider', 'default_model', 'blob'])
  const resultProjectID = resultIdentifier(object, 'project_id')
  if (resultProjectID !== projectID) throw new Error('project model result identity does not match request')
  const defaultProvider = boundedResultString(object.default_provider, 'default_provider')
  const defaultModel = boundedResultString(object.default_model, 'default_model')
  const hasModels = object.models !== null
  const hasBlob = object.blob !== null
  if (hasModels === hasBlob) throw new Error('project model result payload is invalid')
  if (hasModels) {
    return {
      project_id: resultProjectID,
      models: decodeSessionModelOptions(object.models),
      default_provider: defaultProvider,
      default_model: defaultModel,
    }
  }
  const descriptor = decodeBlobDescriptor(object.blob)
  if (descriptor.size > maxReadBlobBytes) throw new Error('project model blob is too large')
  if (descriptor.content_type !== 'application/json') throw new Error('project model blob content type is invalid')
  if (!blobClient) throw new Error('project model result blob client is unavailable')
  return blobClient.getJSON(descriptor, { signal }).then((payload) => {
    encodedResultBytes(payload, 'project model result', maxReadBlobBytes)
    return {
      project_id: resultProjectID,
      models: decodeSessionModelOptions(payload),
      default_provider: defaultProvider,
      default_model: defaultModel,
    }
  })
}

const codexUsageWindowMaxSeconds = 1_000_000_000
const codexUsageResetAtMax = 10_000_000_000_000
const codexUsageMaxAdditionalLimits = 64
const codexUsageMaxArrayItems = 256

function decodeCodexUsageWindow(value: unknown): CodexUsageWindow | null | undefined {
  if (value === null) return null
  if (value === undefined) return undefined
  const object = exactObject(value, ['used_percent', 'limit_window_seconds', 'reset_after_seconds', 'reset_at'])
  return {
    used_percent: resultSafeInteger(object, 'used_percent', 0, 100),
    limit_window_seconds: resultSafeInteger(object, 'limit_window_seconds', 0, codexUsageWindowMaxSeconds),
    reset_after_seconds: resultSafeInteger(object, 'reset_after_seconds', 0, codexUsageWindowMaxSeconds),
    reset_at: resultSafeInteger(object, 'reset_at', 0, codexUsageResetAtMax),
  }
}

function decodeCodexUsageWindowSet(value: unknown): CodexUsageWindowSet | undefined {
  if (value === null) return undefined
  const object = exactObject(value, ['allowed', 'limit_reached', 'primary_window', 'secondary_window'])
  if (typeof object.allowed !== 'boolean' || typeof object.limit_reached !== 'boolean') throw new Error('Codex usage rate limit is invalid')
  return {
    allowed: object.allowed,
    limit_reached: object.limit_reached,
    primary_window: decodeCodexUsageWindow(object.primary_window),
    secondary_window: object.secondary_window === null ? null : decodeCodexUsageWindow(object.secondary_window),
  }
}

function decodeCodexUsage(value: unknown): CodexUsage {
  encodedResultBytes(value, 'Codex usage result', maxReadBlobBytes)
  const object = exactObject(value, ['user_id', 'account_id', 'email', 'plan_type', 'rate_limit', 'additional_rate_limits', 'credits'])
  const usage: CodexUsage = {
    user_id: boundedResultString(object.user_id, 'user_id', 512),
    account_id: boundedResultString(object.account_id, 'account_id', 512),
    email: boundedResultString(object.email, 'email', 512),
    plan_type: boundedResultString(object.plan_type, 'plan_type', 512),
    rate_limit: decodeCodexUsageWindowSet(object.rate_limit),
  }
  if (object.additional_rate_limits !== null) {
    if (!Array.isArray(object.additional_rate_limits) || object.additional_rate_limits.length > codexUsageMaxAdditionalLimits) throw new Error('Codex usage additional limits are invalid')
    usage.additional_rate_limits = object.additional_rate_limits.map((item) => {
      const additional = exactObject(item, ['limit_name', 'metered_feature', 'rate_limit'])
      return {
        limit_name: boundedResultString(additional.limit_name, 'limit_name', 512),
        metered_feature: boundedResultString(additional.metered_feature, 'metered_feature', 512),
        rate_limit: decodeCodexUsageWindowSet(additional.rate_limit),
      }
    })
  }
  if (object.credits !== null) {
    const credits = exactObject(object.credits, ['has_credits', 'unlimited', 'overage_limit_reached', 'balance', 'approx_local_messages', 'approx_cloud_messages'])
    if (typeof credits.has_credits !== 'boolean' || typeof credits.unlimited !== 'boolean' || typeof credits.overage_limit_reached !== 'boolean') throw new Error('Codex usage credits are invalid')
    const decodeApprox = (value: unknown): number[] | undefined => {
      if (value === null) return undefined
      if (!Array.isArray(value) || value.length > codexUsageMaxArrayItems) throw new Error('Codex usage credit estimate is invalid')
      return value.map((item) => {
        if (!Number.isSafeInteger(item) || (item as number) < 0) throw new Error('Codex usage credit estimate is invalid')
        return item as number
      })
    }
    usage.credits = {
      has_credits: credits.has_credits,
      unlimited: credits.unlimited,
      overage_limit_reached: credits.overage_limit_reached,
      balance: boundedResultString(credits.balance, 'balance', 512),
      approx_local_messages: decodeApprox(credits.approx_local_messages),
      approx_cloud_messages: decodeApprox(credits.approx_cloud_messages),
    }
  }
  return usage
}

function decodeProviderCodexUsageResult(value: unknown, provider: string, blobClient?: BlobClient, signal?: AbortSignal): ProviderCodexUsageResult | Promise<ProviderCodexUsageResult> {
  const object = exactObject(value, ['provider', 'usage', 'blob'])
  const resultProvider = resultString(object, 'provider')
  if (resultProvider !== provider) throw new Error('Codex usage result identity does not match request')
  const hasUsage = object.usage !== null
  const hasBlob = object.blob !== null
  if (hasUsage === hasBlob) throw new Error('Codex usage result payload is invalid')
  if (hasUsage) return { provider: resultProvider, usage: decodeCodexUsage(object.usage) }
  const descriptor = decodeBlobDescriptor(object.blob)
  if (descriptor.size > maxReadBlobBytes) throw new Error('Codex usage blob is too large')
  if (descriptor.content_type !== 'application/json') throw new Error('Codex usage blob content type is invalid')
  if (!blobClient) throw new Error('Codex usage result blob client is unavailable')
  return blobClient.getJSON(descriptor, { signal }).then((payload) => ({ provider: resultProvider, usage: decodeCodexUsage(payload) }))
}

function decodeMarkReadResult(value: unknown, sessionID: string, runID: string): SessionMarkReadResult {
  const object = exactObject(value, ['session_id', 'run_id', 'marked_read'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  if (resultSessionID !== sessionID || resultRunID !== runID) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, marked_read: resultBoolean(object, 'marked_read') }
}

function decodeRenameResult(value: unknown, sessionID: string, displayName: string): SessionRenameResult {
  const object = exactObject(value, ['session_id', 'display_name'])
  const resultSessionID = resultString(object, 'session_id')
  const resultDisplayName = resultString(object, 'display_name')
  if (resultSessionID !== sessionID || resultDisplayName !== displayName) throw new Error('result does not match request')
  return { session_id: resultSessionID, display_name: resultDisplayName }
}

function decodeArchiveResult(value: unknown, sessionID: string, archived: boolean): SessionArchiveResult {
  const object = exactObject(value, ['session_id', 'archived'])
  const resultSessionID = resultString(object, 'session_id')
  if (resultSessionID !== sessionID || resultBoolean(object, 'archived') !== archived) throw new Error('result does not match request')
  return { session_id: resultSessionID, archived }
}

function decodeFullAccessResult(value: unknown, sessionID: string, fullAccess: boolean): SessionFullAccessResult {
  const object = exactObject(value, ['session_id', 'full_access'])
  const resultSessionID = resultString(object, 'session_id')
  if (resultSessionID !== sessionID || resultBoolean(object, 'full_access') !== fullAccess) throw new Error('result does not match request')
  return { session_id: resultSessionID, full_access: fullAccess }
}

function decodeDebugResult(value: unknown, sessionID: string, requestBodies: boolean): SessionDebugResult {
  const object = exactObject(value, ['session_id', 'request_bodies'])
  const resultSessionID = resultString(object, 'session_id')
  if (resultSessionID !== sessionID || resultBoolean(object, 'request_bodies') !== requestBodies) throw new Error('result does not match request')
  return { session_id: resultSessionID, request_bodies: requestBodies }
}

function decodeCreateResult(value: unknown, sessionID: string, projectID: string): SessionCreateResult {
  const object = exactObject(value, ['session_id', 'project_id'])
  const resultSessionID = resultString(object, 'session_id')
  const resultProjectID = resultString(object, 'project_id')
  if (resultSessionID !== sessionID || resultProjectID !== projectID) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, project_id: resultProjectID }
}

function decodeRunCancelResult(value: unknown, runID: string): RunCancelResult {
  const object = exactObject(value, ['run_id', 'status'])
  const resultRunID = resultString(object, 'run_id')
  const status = object.status
  const statuses: RunStatus[] = ['running', 'committed', 'failed', 'interrupted', 'cancelled']
  if (resultRunID !== runID || typeof status !== 'string' || !statuses.includes(status as RunStatus)) throw new Error('result does not match request')
  return { run_id: resultRunID, status: status as RunStatus }
}

function decodeRunStartResult(value: unknown, sessionID: string, runID: string): RunStartResult {
  const object = exactObject(value, ['session_id', 'run_id', 'status'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const status = object.status
  const statuses: RunStatus[] = ['running', 'committed', 'failed', 'interrupted', 'cancelled']
  if (resultSessionID !== sessionID || resultRunID !== runID || typeof status !== 'string' || !statuses.includes(status as RunStatus)) throw new Error('result does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, status: status as RunStatus }
}

function decodeRunContinueResult(value: unknown, sessionID: string, runID: string): RunContinueResult {
  const object = exactObject(value, ['session_id', 'run_id', 'status'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const status = object.status
  const statuses: RunStatus[] = ['running', 'committed', 'failed', 'interrupted', 'cancelled']
  if (resultSessionID !== sessionID || resultRunID !== runID || typeof status !== 'string' || !statuses.includes(status as RunStatus)) throw new Error('result does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, status: status as RunStatus }
}

function decodeRunPromptAppendResult(value: unknown, sessionID: string, runID: string, operationID: string): RunPromptAppendResult {
  const object = exactObject(value, ['operation_id', 'session_id', 'run_id', 'accepted'])
  const resultOperationID = resultString(object, 'operation_id')
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  if (resultOperationID !== operationID || resultSessionID !== sessionID || resultRunID !== runID || object.accepted !== true) throw new Error('result identity does not match request')
  return { operation_id: resultOperationID, session_id: resultSessionID, run_id: resultRunID, accepted: true }
}

function decodeRunPromptRemoveResult(value: unknown, sessionID: string, runID: string, promptID: string): RunPromptRemoveResult {
  const object = exactObject(value, ['session_id', 'run_id', 'prompt_id', 'removed'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const resultPromptID = resultString(object, 'prompt_id')
  if (resultSessionID !== sessionID || resultRunID !== runID || resultPromptID !== promptID || object.removed !== true) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, prompt_id: resultPromptID, removed: true }
}

function decodeRunPromptSteerResult(value: unknown, sessionID: string, runID: string, promptID: string, steer: boolean): RunPromptSteerResult {
  const object = exactObject(value, ['session_id', 'run_id', 'prompt_id', 'steer'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const resultPromptID = resultString(object, 'prompt_id')
  if (resultSessionID !== sessionID || resultRunID !== runID || resultPromptID !== promptID || object.steer !== steer) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, prompt_id: resultPromptID, steer }
}

function decodeRunPromptMoveResult(value: unknown, sessionID: string, runID: string, promptID: string): RunPromptMoveResult {
  const object = exactObject(value, ['session_id', 'run_id', 'prompt_id', 'moved'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const resultPromptID = resultString(object, 'prompt_id')
  if (resultSessionID !== sessionID || resultRunID !== runID || resultPromptID !== promptID || typeof object.moved !== 'boolean') throw new Error('result identity does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, prompt_id: resultPromptID, moved: object.moved as boolean }
}

function decodeRunToolCancelResult(value: unknown, sessionID: string, runID: string, toolCallID: string): RunToolCancelResult {
  const object = exactObject(value, ['session_id', 'run_id', 'tool_call_id', 'cancelled'])
  const resultSessionID = resultString(object, 'session_id')
  const resultRunID = resultString(object, 'run_id')
  const resultToolCallID = resultString(object, 'tool_call_id')
  if (resultSessionID !== sessionID || resultRunID !== runID || resultToolCallID !== toolCallID || object.cancelled !== true) throw new Error('result identity does not match request')
  return { session_id: resultSessionID, run_id: resultRunID, tool_call_id: resultToolCallID, cancelled: true }
}

export interface CommandFacadeDebugSnapshot {
  readonly started: boolean
  readonly pendingCount: number
}

/**
 * Typed application command boundary. A command result is only a promise
 * result; it never mutates a replica. Durable authority still arrives through
 * the resource snapshot/change stream.
 */
export class CommandFacade implements SessionCommands, RunCommands, ProjectCommands, ProviderCommands, CodexLoginCommands {
  private readonly transport: RuntimeTransport
  private readonly blobClient?: BlobClient
  private readonly timeoutMS: number
  private readonly maxPendingCommands: number
  private readonly maxRecentRequestIDs: number
  private readonly maxRecentEntityIDs: number
  private readonly requestIDGenerator: () => string
  private readonly sessionIDGenerator: () => string
  private readonly runIDGenerator: () => string
  private readonly operationIDGenerator: () => string
  private readonly setTimer: NonNullable<CommandFacadeOptions['setTimeout']>
  private readonly clearTimer: NonNullable<CommandFacadeOptions['clearTimeout']>
  private pending = new Map<string, PendingCommand<unknown>>()
  private recentRequestIDs = new Set<string>()
  private recentRequestIDOrder: string[] = []
  private recentEntityIDs = new Set<string>()
  private recentEntityIDOrder: string[] = []
  private recentRunIDs = new Set<string>()
  private recentRunIDOrder: string[] = []
  private recentOperationIDs = new Set<string>()
  private recentOperationIDOrder: string[] = []
  private detach: (() => void)[] = []
  private started = false

  constructor(options: CommandFacadeOptions) {
    this.transport = options.transport
    this.blobClient = options.blobClient
    this.timeoutMS = options.timeoutMS ?? 10_000
    this.maxPendingCommands = options.maxPendingCommands ?? 128
    this.maxRecentRequestIDs = options.maxRecentRequestIDs ?? 256
    this.maxRecentEntityIDs = options.maxRecentEntityIDs ?? 256
    this.requestIDGenerator = options.requestIDGenerator ?? defaultRequestID
    this.sessionIDGenerator = options.sessionIDGenerator ?? defaultSessionID
    this.runIDGenerator = options.runIDGenerator ?? defaultRunID
    this.operationIDGenerator = options.operationIDGenerator ?? defaultOperationID
    this.setTimer = options.setTimeout ?? ((handler, timeout) => globalThis.setTimeout(handler, timeout))
    this.clearTimer = options.clearTimeout ?? ((handle) => globalThis.clearTimeout(handle))
    if (this.timeoutMS <= 0 || this.maxPendingCommands <= 0 || this.maxRecentRequestIDs <= 0 || this.maxRecentEntityIDs <= 0) throw new Error('command bounds must be positive')
  }

  /** Infrastructure-only observation for the browser debug bridge. */
  getDebugSnapshot(): CommandFacadeDebugSnapshot {
    return { started: this.started, pendingCount: this.pending.size }
  }

  createProvider(provider: string, target: ProviderUpdateTarget, options: ProviderCreateOptions = {}): Promise<ProviderCreateResult> {
    if (!isProviderCreateName(provider)) return Promise.reject(new CommandFacadeError('invalid', 'provider create name is invalid'))
    let encodedTarget: JsonObject
    try {
      encodedTarget = encodeProviderTarget(target)
      validateProviderCommandJSON({ provider, ...encodedTarget })
    } catch {
      return Promise.reject(new CommandFacadeError('invalid', 'provider target is invalid'))
    }
    const explicitOperationID = options.operationID !== undefined
    let operationID: string
    if (explicitOperationID) {
      if (typeof options.operationID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'operation_id is invalid'))
      operationID = this.cleanID(options.operationID)
      if (!this.validProjectOperationID(operationID)) return Promise.reject(new CommandFacadeError('invalid', 'operation_id is invalid'))
    } else {
      try {
        operationID = this.cleanID(this.operationIDGenerator())
        if (!this.validProjectOperationID(operationID)) throw new Error('operation ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic operation ID generation failed'))
      }
    }
    const args: JsonObject = { operation_id: operationID, provider, ...encodedTarget }
    try {
      validateProviderCommandJSON(args)
    } catch {
      return Promise.reject(new CommandFacadeError('invalid', 'provider target is invalid'))
    }
    if (!explicitOperationID) {
      if (this.recentOperationIDs.has(operationID)) return Promise.reject(new CommandFacadeError('id_generation', 'operation ID collided with an active or recently used command', { collision: true }))
      this.rememberOperationID(operationID)
    }
    // Create has no durable execution claim, so a pending command is not
    // replayed after a server epoch change. Same-epoch reconnects still use
    // the gateway request cache and retain ordinary request-id dedupe.
    return this.submit('provider.create', args, false, (value) => decodeProviderCreateResult(value, provider, operationID), options)
  }

  update(provider: string, target: ProviderUpdateTarget, options: CommandOptions = {}): Promise<ProviderMutationResult> {
    if (!isProviderName(provider)) return Promise.reject(new CommandFacadeError('invalid', 'provider is invalid'))
    let args: JsonObject
    try {
      args = { provider, ...encodeProviderTarget(target) }
      validateProviderCommandJSON(args)
    } catch {
      return Promise.reject(new CommandFacadeError('invalid', 'provider target is invalid'))
    }
    return this.submit('provider.update', args, true, (value) => decodeProviderMutationResult(value, provider), options)
  }

  setDefault(provider: string, model: string, options: CommandOptions = {}): Promise<ProviderDefaultResult> {
    if (!isProviderName(provider) || typeof model !== 'string' || model.length === 0 || !isWellFormedString(model) || model !== model.trim() || this.utf8Bytes(model) > 4096) return Promise.reject(new CommandFacadeError('invalid', 'provider default is invalid'))
    return this.submit('provider.set_default', { provider, model }, true, (value) => decodeProviderDefaultResult(value, provider, model), options)
  }

  discoverModels(provider: string, options: CommandOptions = {}): Promise<ProviderDiscoverModelsResult> {
    if (!isProviderName(provider)) return Promise.reject(new CommandFacadeError('invalid', 'provider is invalid'))
    return this.submit('provider.discover_models', { provider }, true, (value, signal) => decodeProviderDiscoverResult(value, provider, this.blobClient, signal), options)
  }

  searchModelCatalog(query: string, options: CommandOptions = {}): Promise<ModelCatalogSearchResult> {
    if (typeof query !== 'string' || query.trim().length === 0 || !isWellFormedString(query) || this.utf8Bytes(query) > 256) return Promise.reject(new CommandFacadeError('invalid', 'model catalog query is invalid'))
    const normalized = query.trim()
    return this.submit('model_catalog.search', { query: normalized, limit: 50 }, true, (value, signal) => decodeModelCatalogSearchResult(value, normalized, this.blobClient, signal), options)
  }

  readCodexUsage(provider: string, options: CommandOptions = {}): Promise<ProviderCodexUsageResult> {
    if (!isProviderName(provider)) return Promise.reject(new CommandFacadeError('invalid', 'provider is invalid'))
    return this.submit('provider.codex_usage.read', { provider }, true, (value, signal) => decodeProviderCodexUsageResult(value, provider, this.blobClient, signal), options)
  }

  startCodexLogin(provider: string, options: CodexLoginCommandOptions = {}): Promise<CodexLoginStartResult> {
    if (!isProviderName(provider)) return Promise.reject(new CommandFacadeError('invalid', 'provider is invalid'))
    return this.submit('codex_login.start', { provider }, false, (value) => decodeCodexLoginResult(value, provider, 'accepted') as CodexLoginStartResult, options)
  }

  clearCodexLogin(provider: string, options: CodexLoginCommandOptions = {}): Promise<CodexLoginClearResult> {
    if (!isProviderName(provider)) return Promise.reject(new CommandFacadeError('invalid', 'provider is invalid'))
    return this.submit('codex_login.clear', { provider }, true, (value) => decodeCodexLoginResult(value, provider, 'cleared') as CodexLoginClearResult, options)
  }

  private ensureStarted(): void {
    if (this.started) return
    this.started = true
    this.detach = [
      this.transport.onMessage((message, generation) => this.handleMessage(message, generation)),
      this.transport.onReady((event) => this.handleReady(event.generation, event.serverEpoch, event.previousServerEpoch)),
    ]
    if (this.transport.isReady) this.handleReady(this.transport.connectionGeneration, this.transport.serverEpoch ?? '', this.transport.serverEpoch)
  }

  stop(): void {
    if (!this.started && this.pending.size === 0) return
    this.started = false
    for (const detach of this.detach.splice(0)) detach()
    for (const pending of [...this.pending.values()]) this.rejectPending(pending, new CommandFacadeError('stopped', 'command facade stopped'))
  }

  create(projectID: string, options: SessionCreateOptions = {}, commandOptions: CommandOptions = {}): Promise<SessionCreateResult> {
    const cleanProjectID = this.cleanID(projectID)
    if (!cleanProjectID || cleanProjectID.length > 128 || !/^[A-Za-z0-9_.-]+$/.test(cleanProjectID) || cleanProjectID === '.' || cleanProjectID === '..') return Promise.reject(new CommandFacadeError('invalid', 'project_id is invalid'))

    const explicitSessionID = options.sessionID !== undefined
    let sessionID: string
    if (explicitSessionID) {
      // A caller-owned entity ID is the durable idempotency key. It is
      // intentionally not checked against the local recent-ID cache: after a
      // timeout, reload, or epoch change the same ID must reach the server so
      // the durable claim can return the original result or a conflict.
      if (typeof options.sessionID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
      sessionID = this.cleanID(options.sessionID)
      if (!this.validSessionID(sessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
    } else {
      try {
        sessionID = this.cleanID(this.sessionIDGenerator())
        if (!this.validSessionID(sessionID)) throw new Error('session ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic session ID generation failed'))
      }
      if (this.recentEntityIDs.has(sessionID)) return Promise.reject(new CommandFacadeError('id_generation', 'session ID collided with an active or recently used create command', { collision: true }))
    }

    const args: JsonObject = { session_id: sessionID, project_id: cleanProjectID }
    const strings: Array<[keyof SessionCreateOptions, string]> = [
      ['displayName', 'display_name'],
      ['parentSessionID', 'parent_session_id'],
      ['cwd', 'cwd'],
      ['configPath', 'config_path'],
      ['provider', 'provider'],
      ['modelProfile', 'model_profile'],
      ['reasoningLevel', 'reasoning_level'],
    ]
    for (const [source, wire] of strings) {
      const value = options[source]
      if (value !== undefined) {
        if (typeof value !== 'string' || !value.trim() || value.length > 4096) return Promise.reject(new CommandFacadeError('invalid', `${wire} is invalid`))
        args[wire] = value.trim()
      }
    }
    if (options.fullAccess !== undefined) {
      if (typeof options.fullAccess !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'full_access is invalid'))
      args.full_access = options.fullAccess
    }
    this.rememberEntityID(sessionID)
    return this.submit('session.create', args, true, (value) => decodeCreateResult(value, sessionID, cleanProjectID), commandOptions)
  }

  createProject(root: string, displayName: string, options: ProjectCreateOptions = {}, commandOptions: CommandOptions = {}): Promise<ProjectCreateResult> {
    if (typeof root !== 'string' || root.trim() === '' || !isWellFormedString(root) || this.utf8Bytes(root) > 4096) return Promise.reject(new CommandFacadeError('invalid', 'root is invalid'))
    if (typeof displayName !== 'string' || !isWellFormedString(displayName) || this.utf8Bytes(displayName.trim()) > 4096) return Promise.reject(new CommandFacadeError('invalid', 'display_name is invalid'))
    const cleanDisplayName = displayName.trim()
    const explicitOperationID = options.operationID !== undefined
    let operationID: string
    if (explicitOperationID) {
      if (typeof options.operationID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'operation_id is invalid'))
      operationID = this.cleanID(options.operationID)
      if (!this.validProjectOperationID(operationID)) return Promise.reject(new CommandFacadeError('invalid', 'operation_id is invalid'))
    } else {
      try {
        operationID = this.cleanID(this.operationIDGenerator())
        if (!this.validProjectOperationID(operationID)) throw new Error('operation ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic operation ID generation failed'))
      }
      if (this.recentOperationIDs.has(operationID)) return Promise.reject(new CommandFacadeError('id_generation', 'operation ID collided with an active or recently used command', { collision: true }))
      this.rememberOperationID(operationID)
    }
    // Do not resolve, clean, or otherwise normalize a filesystem path on the
    // client. CanonicalRoot on the server is the only authority for symlinks,
    // case rules, and path identity.
    return this.submit('project.create', { operation_id: operationID, root, display_name: cleanDisplayName }, true, (value) => decodeProjectCreateResult(value, operationID), commandOptions)
  }

  renameProject(projectID: string, displayName: string, options: CommandOptions = {}): Promise<ProjectRenameResult> {
    const cleanProjectID = this.cleanID(projectID)
    const cleanDisplayName = this.cleanID(displayName)
    if (!this.validProjectID(cleanProjectID) || !cleanDisplayName || !isWellFormedString(cleanDisplayName) || this.utf8Bytes(cleanDisplayName) > 4096) return Promise.reject(new CommandFacadeError('invalid', 'project_id and display_name are invalid'))
    return this.submit('project.rename', { project_id: cleanProjectID, display_name: cleanDisplayName }, true, (value) => decodeProjectRenameResult(value, cleanProjectID, cleanDisplayName), options)
  }

  archiveProject(projectID: string, options: CommandOptions = {}): Promise<ProjectArchiveResult> {
    const cleanProjectID = this.cleanID(projectID)
    if (!this.validProjectID(cleanProjectID)) return Promise.reject(new CommandFacadeError('invalid', 'project_id is invalid'))
    return this.submit('project.archive', { project_id: cleanProjectID }, true, (value) => decodeProjectArchiveResult(value, cleanProjectID, true), options)
  }

  restoreProject(projectID: string, options: CommandOptions = {}): Promise<ProjectArchiveResult> {
    const cleanProjectID = this.cleanID(projectID)
    if (!this.validProjectID(cleanProjectID)) return Promise.reject(new CommandFacadeError('invalid', 'project_id is invalid'))
    return this.submit('project.restore', { project_id: cleanProjectID }, true, (value) => decodeProjectArchiveResult(value, cleanProjectID, false), options)
  }

  deleteProject(projectID: string, options: CommandOptions = {}): Promise<ProjectDeleteResult> {
    const cleanProjectID = this.cleanID(projectID)
    if (!this.validProjectID(cleanProjectID)) return Promise.reject(new CommandFacadeError('invalid', 'project_id is invalid'))
    return this.submit('project.delete', { project_id: cleanProjectID }, false, (value) => decodeProjectDeleteResult(value, cleanProjectID), options)
  }

  readModels(projectID: string, options: CommandOptions = {}): Promise<ProjectModelsResult> {
    const cleanProjectID = this.cleanID(projectID)
    if (!this.validProjectID(cleanProjectID)) return Promise.reject(new CommandFacadeError('invalid', 'project_id is invalid'))
    return this.submit('project.models.read', { project_id: cleanProjectID }, true, (value, signal) => decodeProjectModelsResult(value, cleanProjectID, this.blobClient, signal), options)
  }

  markRead(sessionID: string, runID: string, projectID?: string, options: CommandOptions = {}): Promise<SessionMarkReadResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    const cleanProjectID = projectID === undefined ? undefined : this.cleanID(projectID)
    if (!cleanSessionID || !cleanRunID || (projectID !== undefined && !cleanProjectID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id and run_id are required'))
    const args: JsonObject = { session_id: cleanSessionID, run_id: cleanRunID }
    if (cleanProjectID) args.project_id = cleanProjectID
    return this.submit('session.mark_read', args, true, (value) => decodeMarkReadResult(value, cleanSessionID, cleanRunID), options)
  }

  rename(sessionID: string, displayName: string, options: CommandOptions = {}): Promise<SessionRenameResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanDisplayName = this.cleanID(displayName)
    if (!cleanSessionID || !cleanDisplayName) return Promise.reject(new CommandFacadeError('invalid', 'session_id and display_name are required'))
    return this.submit('session.rename', { session_id: cleanSessionID, display_name: cleanDisplayName }, true, (value) => decodeRenameResult(value, cleanSessionID, cleanDisplayName), options)
  }

  archive(sessionID: string, options: CommandOptions = {}): Promise<SessionArchiveResult> {
    return this.submitSessionToggle('session.archive', sessionID, true, options)
  }

  restore(sessionID: string, options: CommandOptions = {}): Promise<SessionArchiveResult> {
    return this.submitSessionToggle('session.restore', sessionID, false, options)
  }

  delete(sessionID: string, options: CommandOptions = {}): Promise<SessionDeleteResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!this.validSessionID(cleanSessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
    return this.submit('session.delete', { session_id: cleanSessionID }, false, (value) => decodeDeleteResult(value, cleanSessionID), options)
  }

  deleteSession(sessionID: string, options: CommandOptions = {}): Promise<SessionDeleteResult> {
    return this.delete(sessionID, options)
  }

  compact(sessionID: string, options: CommandOptions = {}): Promise<SessionCompactResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!this.validSessionID(cleanSessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
    return this.submit('session.compact', { session_id: cleanSessionID }, false, (value) => decodeCompactResult(value, cleanSessionID), options)
  }

  historyRead(sessionID: string, historyOptions: SessionHistoryReadOptions = {}, commandOptions: CommandOptions = {}): Promise<SessionHistoryReadResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!this.validSessionID(cleanSessionID) || historyOptions === null || typeof historyOptions !== 'object' || Array.isArray(historyOptions)) return Promise.reject(new CommandFacadeError('invalid', 'session_id or history options are invalid'))
    const args: JsonObject = { session_id: cleanSessionID }
    const cursor = historyOptions.cursor
    const direction = historyOptions.direction
    if (cursor !== undefined) {
      if (!Number.isSafeInteger(cursor) || cursor <= 0) return Promise.reject(new CommandFacadeError('invalid', 'cursor is invalid'))
      if (direction !== 'before' && direction !== 'after') return Promise.reject(new CommandFacadeError('invalid', 'direction is invalid'))
      args.cursor = cursor
      args.direction = direction
    } else if (direction !== undefined) {
      return Promise.reject(new CommandFacadeError('invalid', 'direction requires cursor'))
    }
    if (historyOptions.limit !== undefined) {
      if (!Number.isSafeInteger(historyOptions.limit) || historyOptions.limit < 1 || historyOptions.limit > 200) return Promise.reject(new CommandFacadeError('invalid', 'limit is invalid'))
      args.limit = historyOptions.limit
    }
    if (historyOptions.alignTurn !== undefined) {
      if (typeof historyOptions.alignTurn !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'align_turn is invalid'))
      args.align_turn = historyOptions.alignTurn
    }
    return this.submit('session.history.read', args, true, (value, signal) => decodeHistoryReadResult(value, cleanSessionID, cursor ?? 0, direction ?? '', historyOptions.limit ?? 50, historyOptions.alignTurn ?? false, this.blobClient, signal), commandOptions)
  }

  readHistory(sessionID: string, historyOptions: SessionHistoryReadOptions = {}, commandOptions: CommandOptions = {}): Promise<SessionHistoryReadResult> {
    return this.historyRead(sessionID, historyOptions, commandOptions)
  }

  setFullAccess(sessionID: string, fullAccess: boolean, options: CommandOptions = {}): Promise<SessionFullAccessResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID || typeof fullAccess !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'session_id and full_access are required'))
    return this.submit('session.set_full_access', { session_id: cleanSessionID, full_access: fullAccess }, true, (value) => decodeFullAccessResult(value, cleanSessionID, fullAccess), options)
  }

  setDebug(sessionID: string, requestBodies: boolean, options: CommandOptions = {}): Promise<SessionDebugResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID || typeof requestBodies !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'session_id and request_bodies are required'))
    return this.submit('session.set_debug', { session_id: cleanSessionID, request_bodies: requestBodies }, true, (value) => decodeDebugResult(value, cleanSessionID, requestBodies), options)
  }

  cancelRun(runID: string, options: CommandOptions = {}): Promise<RunCancelResult> {
    const cleanRunID = this.cleanID(runID)
    if (!cleanRunID) return Promise.reject(new CommandFacadeError('invalid', 'run_id is required'))
    return this.submit('run.cancel', { run_id: cleanRunID }, false, (value) => decodeRunCancelResult(value, cleanRunID), options)
  }

  appendPrompt(sessionID: string, runID: string, content: string, options: RunPromptAppendOptions = {}): Promise<RunPromptAppendResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    if (!this.validSessionID(cleanSessionID) || !this.validRunID(cleanRunID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id and run_id are invalid'))
    if (typeof content !== 'string' || content.trim() === '' || this.utf8Bytes(content) > 64 * 1024) return Promise.reject(new CommandFacadeError('invalid', 'content is invalid'))

    const explicitOperationID = options.operationID !== undefined
    let operationID: string
    if (explicitOperationID) {
      if (typeof options.operationID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'operation_id is invalid'))
      operationID = this.cleanID(options.operationID)
      if (!this.validOperationID(operationID)) return Promise.reject(new CommandFacadeError('invalid', 'operation_id is invalid'))
    } else {
      try {
        operationID = this.cleanID(this.operationIDGenerator())
        if (!this.validOperationID(operationID)) throw new Error('operation ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic operation ID generation failed'))
      }
      if (this.recentOperationIDs.has(operationID)) return Promise.reject(new CommandFacadeError('id_generation', 'operation ID collided with an active or recently used command', { collision: true }))
      this.rememberOperationID(operationID)
    }
    // Content is user data and is intentionally not trimmed. The exact text
    // is part of the durable operation identity and is resent byte-for-byte.
    return this.submit('run.prompt.append', { session_id: cleanSessionID, run_id: cleanRunID, operation_id: operationID, content }, true, (value) => decodeRunPromptAppendResult(value, cleanSessionID, cleanRunID, operationID), options)
  }

  removePrompt(sessionID: string, runID: string, promptID: string, options: RunControlOptions = {}): Promise<RunPromptRemoveResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    const cleanPromptID = this.cleanControlID(promptID)
    if (!this.validSessionID(cleanSessionID) || !this.validRunID(cleanRunID) || !cleanPromptID) return Promise.reject(new CommandFacadeError('invalid', 'session_id, run_id, and prompt_id are invalid'))
    return this.submit('run.prompt.remove', { session_id: cleanSessionID, run_id: cleanRunID, prompt_id: cleanPromptID }, false, (value) => decodeRunPromptRemoveResult(value, cleanSessionID, cleanRunID, cleanPromptID), options)
  }

  steerPrompt(sessionID: string, runID: string, promptID: string, steer: boolean, options: RunControlOptions = {}): Promise<RunPromptSteerResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    const cleanPromptID = this.cleanControlID(promptID)
    if (!this.validSessionID(cleanSessionID) || !this.validRunID(cleanRunID) || !cleanPromptID || typeof steer !== 'boolean') return Promise.reject(new CommandFacadeError('invalid', 'session_id, run_id, prompt_id, and steer are invalid'))
    return this.submit('run.prompt.steer', { session_id: cleanSessionID, run_id: cleanRunID, prompt_id: cleanPromptID, steer }, false, (value) => decodeRunPromptSteerResult(value, cleanSessionID, cleanRunID, cleanPromptID, steer), options)
  }

  movePrompt(sessionID: string, runID: string, promptID: string, delta: number, options: RunControlOptions = {}): Promise<RunPromptMoveResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    const cleanPromptID = this.cleanControlID(promptID)
    if (!this.validSessionID(cleanSessionID) || !this.validRunID(cleanRunID) || !cleanPromptID || !Number.isInteger(delta) || delta < -64 || delta > 64) return Promise.reject(new CommandFacadeError('invalid', 'session_id, run_id, prompt_id, and delta are invalid'))
    return this.submit('run.prompt.move', { session_id: cleanSessionID, run_id: cleanRunID, prompt_id: cleanPromptID, delta }, false, (value) => decodeRunPromptMoveResult(value, cleanSessionID, cleanRunID, cleanPromptID), options)
  }

  cancelTool(sessionID: string, runID: string, toolCallID: string, options: RunControlOptions = {}): Promise<RunToolCancelResult> {
    const cleanSessionID = this.cleanID(sessionID)
    const cleanRunID = this.cleanID(runID)
    const cleanToolCallID = this.cleanControlID(toolCallID)
    if (!this.validSessionID(cleanSessionID) || !this.validRunID(cleanRunID) || !cleanToolCallID) return Promise.reject(new CommandFacadeError('invalid', 'session_id, run_id, and tool_call_id are invalid'))
    return this.submit('run.tool.cancel', { session_id: cleanSessionID, run_id: cleanRunID, tool_call_id: cleanToolCallID }, false, (value) => decodeRunToolCancelResult(value, cleanSessionID, cleanRunID, cleanToolCallID), options)
  }

  start(): void
  start(sessionID: string, content: string, options?: RunStartOptions): Promise<RunStartResult>
  start(sessionID?: string, content?: string, options: RunStartOptions = {}): void | Promise<RunStartResult> {
    // Keep the existing lifecycle attach call page-independent. RunCommands'
    // callers use the argument-bearing overload below.
    if (sessionID === undefined && content === undefined) {
      this.ensureStarted()
      return
    }
    if (typeof sessionID !== 'string' || typeof content !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'session_id and content are required'))
    const cleanSessionID = this.cleanID(sessionID)
    if (!this.validSessionID(cleanSessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))
    const images = (options.images ?? []).map((image) => ({
      hash: image.hash,
      media_type: image.media_type,
      size_bytes: image.size_bytes,
      ...(image.detail !== undefined ? { detail: image.detail } : {}),
    }))
    if (typeof content !== 'string' || (content.trim() === '' && images.length === 0) || this.utf8Bytes(content) > 256 * 1024) return Promise.reject(new CommandFacadeError('invalid', 'content is invalid'))
    if (!Array.isArray(images) || images.length > 5 || images.some((image) => !/^[0-9a-f]{64}$/u.test(image.hash) || !['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(image.media_type) || !Number.isSafeInteger(image.size_bytes) || image.size_bytes <= 0 || image.size_bytes > 4 * 1024 * 1024 || (image.detail !== undefined && !['auto', 'low', 'high'].includes(image.detail)))) return Promise.reject(new CommandFacadeError('invalid', 'images are invalid'))
    if (images.reduce((total, image) => total + image.size_bytes, 0) > 12 * 1024 * 1024) return Promise.reject(new CommandFacadeError('invalid', 'images are invalid'))

    const explicitRunID = options.runID !== undefined
    let runID: string
    if (explicitRunID) {
      if (typeof options.runID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'run_id is invalid'))
      runID = this.cleanID(options.runID)
      if (!this.validRunID(runID)) return Promise.reject(new CommandFacadeError('invalid', 'run_id is invalid'))
    } else {
      try {
        runID = this.cleanID(this.runIDGenerator())
        if (!this.validRunID(runID)) throw new Error('run ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic run ID generation failed'))
      }
      if (this.recentRunIDs.has(runID)) return Promise.reject(new CommandFacadeError('id_generation', 'run ID collided with an active or recently used command', { collision: true }))
      this.rememberRunID(runID)
    }
    // Do not trim content in the wire payload: text is the normalized input
    // and its exact bytes are part of the durable run fingerprint.
    return this.submit('run.start', { session_id: cleanSessionID, run_id: runID, content, ...(images.length > 0 ? { images } : {}) }, true, (value) => decodeRunStartResult(value, cleanSessionID, runID), options)
  }

  startRun(sessionID: string, content: string, options: RunStartOptions = {}): Promise<RunStartResult> {
    return this.start(sessionID, content, options)
  }

  continueRun(sessionID: string, options: RunContinueOptions = {}): Promise<RunContinueResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!this.validSessionID(cleanSessionID)) return Promise.reject(new CommandFacadeError('invalid', 'session_id is invalid'))

    const explicitRunID = options.runID !== undefined
    let runID: string
    if (explicitRunID) {
      // Explicit IDs intentionally bypass the process-local collision cache:
      // the durable claim is the authority after a timeout, restore, or epoch
      // change, and a new request_id will be generated for this retry.
      if (typeof options.runID !== 'string') return Promise.reject(new CommandFacadeError('invalid', 'run_id is invalid'))
      runID = this.cleanID(options.runID)
      if (!this.validRunID(runID)) return Promise.reject(new CommandFacadeError('invalid', 'run_id is invalid'))
    } else {
      try {
        runID = this.cleanID(this.runIDGenerator())
        if (!this.validRunID(runID)) throw new Error('run ID is invalid')
      } catch {
        return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic run ID generation failed'))
      }
      if (this.recentRunIDs.has(runID)) return Promise.reject(new CommandFacadeError('id_generation', 'run ID collided with an active or recently used command', { collision: true }))
      this.rememberRunID(runID)
    }
    return this.submit('run.continue', { session_id: cleanSessionID, run_id: runID }, true, (value) => decodeRunContinueResult(value, cleanSessionID, runID), options)
  }

  private submitSessionToggle(name: 'session.archive' | 'session.restore', sessionID: string, archived: boolean, options: CommandOptions): Promise<SessionArchiveResult> {
    const cleanSessionID = this.cleanID(sessionID)
    if (!cleanSessionID) return Promise.reject(new CommandFacadeError('invalid', 'session_id is required'))
    return this.submit(name, { session_id: cleanSessionID }, true, (value) => decodeArchiveResult(value, cleanSessionID, archived), options)
  }

  private cleanID(value: string): string {
    return typeof value === 'string' ? value.trim() : ''
  }

  private validSessionID(value: string): boolean {
    if (value.length === 0 || value.length > 128 || !/^[A-Za-z0-9_.-]+$/.test(value) || value === '.' || value === '..') return false
    // Keep this client-side boundary byte-for-byte aligned with Go's
    // ValidateSessionCreateID: path-safe IDs whose trailing dots/spaces are
    // removed are compared case-insensitively against reserved directories.
    const reservedKey = value.replace(/[. ]+$/g, '').toLowerCase()
    return reservedKey !== 'blobs' && reservedKey !== '.session-claims'
  }

  private validProjectID(value: string): boolean {
    return value.length > 0 && value.length <= 128 && /^[A-Za-z0-9_.-]+$/.test(value) && value !== '.' && value !== '..'
  }

  private validRunID(value: string): boolean {
    if (value.length === 0 || value.length > 128 || !/^[A-Za-z0-9_.-]+$/.test(value) || value === '.' || value === '..') return false
    const reservedKey = value.replace(/[. ]+$/g, '').toLowerCase()
    return reservedKey !== 'blobs' && reservedKey !== '.session-claims'
  }

  private validOperationID(value: string): boolean {
    return this.validRunID(value)
  }

  private validProjectOperationID(value: string): boolean {
    return value.length > 0 && value.length <= 128 && /^[A-Za-z0-9_.-]+$/.test(value) && value !== '.' && value !== '..'
  }

  private cleanControlID(value: string): string {
    const cleaned = this.cleanID(value)
    return this.utf8Bytes(cleaned) <= 256 ? cleaned : ''
  }

  private utf8Bytes(value: string): number {
    return typeof TextEncoder === 'function' ? new TextEncoder().encode(value).byteLength : value.length
  }

  private submit<T>(name: string, args: JsonObject, crossEpochRetrySafe: boolean, decodeResult: (value: unknown, signal?: AbortSignal) => T | PromiseLike<T>, options: CommandOptions): Promise<T> {
    if (this.pending.size >= this.maxPendingCommands) return Promise.reject(new CommandFacadeError('capacity', 'too many pending commands'))
    let id: string
    try {
      id = this.requestIDGenerator()
      if (typeof id !== 'string' || id.trim() === '') throw new Error('request ID is empty')
    } catch {
      return Promise.reject(new CommandFacadeError('id_generation', 'cryptographic request ID generation failed'))
    }
    if (this.recentRequestIDs.has(id) || this.pending.has(id)) return Promise.reject(new CommandFacadeError('id_generation', 'request ID collided with an active or recently used command', { collision: true }))
    this.rememberRequestID(id)
    this.ensureStarted()
    const message: CommandMessage = {
      version: 1,
      type: 'command',
      id: `command_${id}`,
      payload: { name, schema_version: 1, request_id: id, arguments: args },
    }
    return new Promise<T>((resolve, reject) => {
      const timer = this.setTimer(() => {
        const pending = this.pending.get(id)
        if (pending) this.rejectPending(pending, new CommandFacadeError('timeout', 'command timed out'))
      }, this.timeoutMS)
      const pending: PendingCommand<T> = { requestID: id, message, crossEpochRetrySafe, decodeResult, resolve, reject, timer, signal: options.signal }
      this.pending.set(id, pending as PendingCommand<unknown>)
      if (options.signal) {
        const abort = () => {
          const current = this.pending.get(id)
          if (current) this.rejectPending(current, new CommandFacadeError('cancelled', 'command was cancelled'))
        }
        pending.abortListener = abort
        options.signal.addEventListener('abort', abort, { once: true })
        if (options.signal.aborted) abort()
      }
      this.sendPending(pending)
    })
  }

  private rememberRequestID(id: string): void {
    this.recentRequestIDs.add(id)
    this.recentRequestIDOrder.push(id)
    while (this.recentRequestIDOrder.length > this.maxRecentRequestIDs) {
      const retired = this.recentRequestIDOrder.shift()
      if (retired !== undefined) this.recentRequestIDs.delete(retired)
    }
  }

  private rememberEntityID(id: string): void {
    if (this.recentEntityIDs.has(id)) return
    this.recentEntityIDs.add(id)
    this.recentEntityIDOrder.push(id)
    while (this.recentEntityIDOrder.length > this.maxRecentEntityIDs) {
      const retired = this.recentEntityIDOrder.shift()
      if (retired !== undefined) this.recentEntityIDs.delete(retired)
    }
  }

  private rememberRunID(id: string): void {
    this.recentRunIDs.add(id)
    this.recentRunIDOrder.push(id)
    while (this.recentRunIDOrder.length > this.maxRecentEntityIDs) {
      const retired = this.recentRunIDOrder.shift()
      if (retired !== undefined) this.recentRunIDs.delete(retired)
    }
  }

  private rememberOperationID(id: string): void {
    this.recentOperationIDs.add(id)
    this.recentOperationIDOrder.push(id)
    while (this.recentOperationIDOrder.length > this.maxRecentEntityIDs) {
      const retired = this.recentOperationIDOrder.shift()
      if (retired !== undefined) this.recentOperationIDs.delete(retired)
    }
  }

  private handleReady(generation: number, serverEpoch: string, _previousServerEpoch?: string): void {
    if (!this.started) return
    for (const pending of [...this.pending.values()]) {
      if (pending.sentGeneration === undefined) {
        this.sendPending(pending, generation, serverEpoch)
        continue
      }
      const sameEpoch = pending.sentEpoch !== undefined && pending.sentEpoch === serverEpoch
      const epochChanged = pending.sentEpoch !== undefined && pending.sentEpoch !== serverEpoch
      if (sameEpoch || (epochChanged && pending.crossEpochRetrySafe)) {
        this.sendPending(pending, generation, serverEpoch)
      } else if (epochChanged) {
        this.rejectPending(pending, new CommandFacadeError('outcome_unknown', 'command outcome is unknown after the server epoch changed'))
      }
    }
  }

  private sendPending<T>(pending: PendingCommand<T>, generation = this.transport.connectionGeneration, epoch = this.transport.serverEpoch ?? ''): void {
    if (!this.transport.isReady || !this.pending.has(pending.requestID)) return
    if (pending.sentGeneration !== undefined && pending.sentGeneration !== generation) this.cancelDecode(pending)
    pending.sentGeneration = generation
    pending.sentEpoch = epoch
    try {
      this.transport.send(pending.message)
    } catch (reason) {
      pending.sentGeneration = undefined
      pending.sentEpoch = undefined
      if (reason instanceof SyncReadError && reason.code === 'protocol') this.rejectPending(pending, new CommandFacadeError('transport', 'command could not be sent'))
    }
  }

  private handleMessage(message: ProtocolMessage, generation: number): void {
    if (message.type === 'error' && message.payload.code.startsWith('web_debug_')) return
    if (message.type === 'command_accepted') return
    if (message.type === 'command_result') {
      this.handleResult(message as CommandResultMessage, generation)
      return
    }
    if (message.type === 'error' && message.payload.request_id) {
      const pending = this.pending.get(message.payload.request_id)
      if (pending && pending.sentGeneration === generation) {
        const error = message as ErrorMessage
        this.rejectPending(pending, errorFromCommand(error.payload.code, error.payload.message, error.payload.details))
      }
    }
  }

  private handleResult(message: CommandResultMessage, generation: number): void {
    const pending = this.pending.get(message.payload.request_id)
    if (!pending || pending.sentGeneration !== generation || pending.decodeGeneration === generation) return
    if (message.payload.status === 'failed') {
      const error = message.payload.error
      this.rejectPending(pending, errorFromCommand(error?.code ?? 'command_failed', error?.message ?? 'command failed', error?.details))
      return
    }
    pending.decodeGeneration = generation
    const controller = typeof AbortController === 'function' ? new AbortController() : undefined
    pending.decodeAbort = controller
    try {
      const decoded = pending.decodeResult(message.payload.result, controller?.signal)
      if (decoded && typeof (decoded as PromiseLike<unknown>).then === 'function') {
        Promise.resolve(decoded)
          .then((value) => {
            if (this.pending.get(pending.requestID) === pending && pending.sentGeneration === generation && pending.decodeGeneration === generation) this.resolvePending(pending, value)
          })
          .catch(() => {
            if (this.pending.get(pending.requestID) === pending && pending.sentGeneration === generation && pending.decodeGeneration === generation) this.rejectPending(pending, new CommandFacadeError('invalid', 'command result was invalid'))
          })
      } else if (this.pending.get(pending.requestID) === pending && pending.sentGeneration === generation && pending.decodeGeneration === generation) {
        this.resolvePending(pending, decoded)
      }
    } catch {
      if (this.pending.get(pending.requestID) === pending && pending.sentGeneration === generation && pending.decodeGeneration === generation) this.rejectPending(pending, new CommandFacadeError('invalid', 'command result was invalid'))
    }
  }

  private cancelDecode<T>(pending: PendingCommand<T>): void {
    pending.decodeAbort?.abort()
    pending.decodeAbort = undefined
    pending.decodeGeneration = undefined
  }

  private resolvePending(pending: PendingCommand<unknown>, value: unknown): void {
    if (!this.pending.delete(pending.requestID)) return
    this.cancelDecode(pending)
    this.clearTimer(pending.timer)
    if (pending.signal && pending.abortListener) pending.signal.removeEventListener('abort', pending.abortListener)
    pending.resolve(value)
  }

  private rejectPending<T>(pending: PendingCommand<T>, error: CommandFacadeError): void {
    if (!this.pending.delete(pending.requestID)) return
    this.cancelDecode(pending as PendingCommand<unknown>)
    this.clearTimer(pending.timer)
    if (pending.signal && pending.abortListener) pending.signal.removeEventListener('abort', pending.abortListener)
    pending.reject(error)
  }
}
