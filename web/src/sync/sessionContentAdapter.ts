import { isRFC3339Timestamp } from '../protocol/datetime'
import { isCanonicalWireIdentifier, isWellFormedString } from '../protocol/identifiers'
import { compareRunCursor, isRunCursor, isResourceRevision } from '../protocol/sequence'
import type { ChangeOperation, SubscriptionEventData } from '../protocol/types'
import type { ReplicaApplyContext, ResourceAdapter, TransientResumeToken } from './localReplica'
import {
  type DataAvailability,
  type SessionContentActiveRun,
  type SessionContentBlob,
  type SessionContentCompaction,
  type SessionContentCompactionCheckpoint,
  type SessionContentHistoryDescriptor,
  type SessionContentHistoryWindow,
  type SessionContentItem,
  type SessionContentItemKey,
  type SessionContentMessage,
  type SessionContentMetadata,
  type SessionContentSettlementWatermark,
  type SessionContentSnapshot,
  type SessionContentState,
  type SessionContentText,
  type SessionContentTransientItemWatermark,
  type SessionPrompt,
  type SessionRunState,
  type SessionToolState,
  type SessionTransientText,
} from '../domain/sessionContent'

export type { DataAvailability, SessionRunState } from '../domain/sessionContent'

const sessionStatuses = new Set(['idle', 'running', 'failed', 'interrupted'])
const runStatuses = new Set(['running', 'committed', 'failed', 'interrupted', 'cancelled'])
const itemKinds = new Set(['message', 'compaction', 'runtime_context'])
const visibilities = new Set(['visible', 'hidden', 'debug'])
const audiences = new Set(['user', 'model', 'internal'])
const itemStatuses = new Set(['pending', 'completed', 'error', 'interrupted'])
const roles = new Set(['system', 'developer', 'user', 'assistant', 'tool', 'provider'])
const createdBy = new Set(['user', 'agent'])
const contentTypes = new Set(['text/plain', 'text/plain; charset=utf-8', 'application/json'])
const operationNames = new Set([
  'metadata.replace', 'item.upsert', 'item.remove', 'history.window.replace',
  'history.window.descriptor.replace', 'active_run.replace', 'active_run.clear', 'compaction.replace',
])

const metadataRequired = ['id', 'version', 'created_at', 'updated_at', 'archived', 'last_used_at', 'has_unread_result', 'status', 'show_reasoning', 'full_access', 'debug', 'context', 'save_tool_results']
const metadataOptional = [
  'display_name', 'created_by', 'parent_session_id', 'root_session_id', 'spawn_depth', 'archived_at',
  'current_run_id', 'running_run_id', 'running_turn_id', 'interrupted_run_id', 'interrupted_turn_id',
  'interrupted_at', 'latest_run_id', 'last_run_id', 'last_run_status', 'provider', 'model_profile',
  'model_id', 'pricing', 'reasoning_level', 'model_parameters', 'project_id', 'cwd', 'created_cwd',
  'config_path', 'config_dir', 'enabled_tools', 'enabled_mcp', 'enabled_skills', 'active_history',
]
const descriptorRequired = ['limit', 'align_turn', 'visible_only', 'has_more_before', 'has_more_after']
const descriptorOptional = ['before_item_seq', 'after_item_seq', 'oldest_item_seq', 'newest_item_seq']
const itemRequired = ['key', 'seq', 'created_at', 'kind', 'visibility', 'audience']
const itemOptional = ['status', 'message']
const activeRunRequired = ['run_id', 'session_id', 'started_at', 'status', 'recoverable']
const activeRunOptional = ['turn_id', 'run_epoch', 'run_cursor', 'replay_available', 'replay_from_cursor', 'replay_to_cursor', 'recovery_required', 'durable_settlement_watermark']
const compactionRequired = ['checkpoints', 'truncated']
const checkpointRequired = ['id', 'created_at', 'reason', 'phase', 'trigger', 'summary_item_id', 'replacement_history']
const checkpointOptional = ['from_item_id', 'to_item_id', 'previous_active_history', 'summary_provider', 'summary_model']

function object(value: unknown, name: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error(`${name} must be an object`)
  return value as Record<string, unknown>
}

function own(value: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key)
}

function exactKeys(value: Record<string, unknown>, required: readonly string[], optional: readonly string[] = [], name = 'object'): void {
  const allowed = new Set([...required, ...optional])
  for (const key of Object.keys(value)) if (!allowed.has(key)) throw new Error(`${name} has unknown field ${key}`)
  for (const key of required) if (!own(value, key)) throw new Error(`${name}.${key} is required`)
}

function stringValue(value: unknown, name: string, allowEmpty = false): string {
  if (typeof value !== 'string' || (!allowEmpty && value.trim() === '') || !isWellFormedString(value)) throw new Error(`${name} must be a valid string`)
  return value
}

function identifier(value: unknown, name: string, allowEmpty = false): string {
  if (!isCanonicalWireIdentifier(value, allowEmpty)) throw new Error(`${name} is not canonical`)
  return value
}

function booleanValue(value: unknown, name: string): boolean {
  if (typeof value !== 'boolean') throw new Error(`${name} must be a boolean`)
  return value
}

function integer(value: unknown, name: string, minimum: number): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) throw new Error(`${name} must be an integer >= ${minimum}`)
  return value
}

function timestamp(value: unknown, name: string): string {
  const result = stringValue(value, name)
  if (!isRFC3339Timestamp(result)) throw new Error(`${name} must be an RFC3339 timestamp`)
  return result
}

function decimal(value: unknown, name: string, canonical = false): string {
  const result = stringValue(value, name)
  if (!/^[0-9]+$/u.test(result) || (canonical && result.length > 1 && result.startsWith('0'))) throw new Error(`${name} must be a decimal string`)
  return result
}

function historyCursor(value: unknown, name: string): string {
  const result = decimal(value, name, true)
  if (BigInt(result) > 9223372036854775807n) throw new Error(`${name} exceeds the D1 cursor bound`)
  return result
}

function revision(value: unknown, name: string): string {
  if (!isResourceRevision(value)) throw new Error(`${name} must be a non-empty resource revision`)
  return value
}

function stringArray(value: unknown, name: string, IDs = false): readonly string[] {
  if (!Array.isArray(value)) throw new Error(`${name} must be an array`)
  return value.map((entry, index) => IDs ? identifier(entry, `${name}[${index}]`) : stringValue(entry, `${name}[${index}]`, true))
}

function jsonObject(value: unknown, name: string): Record<string, unknown> {
  const source = object(value, name)
  return { ...source }
}

function cloneBlob(value: unknown, name: string): SessionContentBlob {
  const source = object(value, name)
  exactKeys(source, ['id', 'url', 'content_type', 'size', 'sha256', 'etag', 'expires_at'], [], name)
  const size = integer(source.size, `${name}.size`, 0)
  const contentType = stringValue(source.content_type, `${name}.content_type`)
  const sha256 = stringValue(source.sha256, `${name}.sha256`)
  return {
    id: stringValue(source.id, `${name}.id`),
    url: stringValue(source.url, `${name}.url`),
    content_type: contentType,
    size,
    sha256,
    etag: stringValue(source.etag, `${name}.etag`),
    expires_at: timestamp(source.expires_at, `${name}.expires_at`),
  }
}

function cloneText(value: unknown, name: string): SessionContentText {
  const source = object(value, name)
  exactKeys(source, [], ['inline', 'preview', 'blob', 'content_type', 'truncated'], name)
  const inline = source.inline === undefined ? undefined : stringValue(source.inline, `${name}.inline`, true)
  const preview = source.preview === undefined ? undefined : stringValue(source.preview, `${name}.preview`, true)
  const blob = source.blob === undefined ? undefined : cloneBlob(source.blob, `${name}.blob`)
  const contentType = source.content_type === undefined ? undefined : stringValue(source.content_type, `${name}.content_type`)
  if (contentType !== undefined && !contentTypes.has(contentType)) throw new Error(`${name}.content_type is not supported`)
  const truncated = source.truncated === undefined ? undefined : booleanValue(source.truncated, `${name}.truncated`)
  if (inline !== undefined && blob !== undefined) throw new Error(`${name} cannot contain both inline and blob`)
  if (inline !== undefined && preview !== undefined) throw new Error(`${name} cannot contain both inline and preview`)
  if (blob !== undefined && truncated) throw new Error(`${name}.blob cannot be truncated`)
  if (blob !== undefined && contentType !== undefined && blob.content_type !== contentType) throw new Error(`${name}.content_type does not match blob`)
  if (contentType === 'application/json' && inline !== undefined) {
    try { JSON.parse(inline) } catch { throw new Error(`${name}.inline is not valid JSON`) }
  }
  if ((inline === undefined || inline.length === 0) && (preview === undefined || preview.length === 0) && blob === undefined && truncated !== true) throw new Error(`${name} must contain content`)
  return {
    ...(inline !== undefined ? { inline } : {}),
    ...(preview !== undefined ? { preview } : {}),
    ...(blob !== undefined ? { blob } : {}),
    ...(contentType !== undefined ? { content_type: contentType as SessionContentText['content_type'] } : {}),
    ...(truncated !== undefined ? { truncated } : {}),
  }
}

function cloneMessage(value: unknown, name: string): SessionContentMessage {
  const source = object(value, name)
  exactKeys(source, ['role'], ['content', 'reasoning', 'images', 'tool_call_id', 'tool_calls', 'is_error'], name)
  const role = stringValue(source.role, `${name}.role`)
  if (!roles.has(role)) throw new Error(`${name}.role is not supported`)
  const images = source.images === undefined ? undefined : (() => {
    if (!Array.isArray(source.images)) throw new Error(`${name}.images must be an array`)
    return source.images.map((entry, index) => {
      const image = object(entry, `${name}.images[${index}]`)
      exactKeys(image, ['hash', 'media_type', 'size_bytes'], [], `${name}.images[${index}]`)
      return { hash: identifier(image.hash, `${name}.images[${index}].hash`), media_type: stringValue(image.media_type, `${name}.images[${index}].media_type`), size_bytes: integer(image.size_bytes, `${name}.images[${index}].size_bytes`, 1) }
    })
  })()
  const toolCalls = source.tool_calls === undefined ? undefined : (() => {
    if (!Array.isArray(source.tool_calls)) throw new Error(`${name}.tool_calls must be an array`)
    return source.tool_calls.map((entry, index) => {
      const call = object(entry, `${name}.tool_calls[${index}]`)
      exactKeys(call, ['id', 'name'], ['arguments'], `${name}.tool_calls[${index}]`)
      return {
        id: identifier(call.id, `${name}.tool_calls[${index}].id`),
        name: identifier(call.name, `${name}.tool_calls[${index}].name`),
        ...(call.arguments === undefined ? {} : { arguments: cloneText(call.arguments, `${name}.tool_calls[${index}].arguments`) }),
      }
    })
  })()
  return {
    role: role as SessionContentMessage['role'],
    ...(source.content === undefined ? {} : { content: cloneText(source.content, `${name}.content`) }),
    ...(source.reasoning === undefined ? {} : { reasoning: cloneText(source.reasoning, `${name}.reasoning`) }),
    ...(images === undefined ? {} : { images }),
    ...(source.tool_call_id === undefined ? {} : { tool_call_id: identifier(source.tool_call_id, `${name}.tool_call_id`) }),
    ...(toolCalls === undefined ? {} : { tool_calls: toolCalls }),
    ...(source.is_error === undefined ? {} : { is_error: booleanValue(source.is_error, `${name}.is_error`) }),
  }
}

function cloneKey(value: unknown, name: string): SessionContentItemKey {
  const source = object(value, name)
  exactKeys(source, ['turn_id', 'agent_iteration', 'item_id'], [], name)
  return {
    turn_id: identifier(source.turn_id, `${name}.turn_id`, true),
    agent_iteration: integer(source.agent_iteration, `${name}.agent_iteration`, 0),
    item_id: identifier(source.item_id, `${name}.item_id`),
  }
}

export function sessionContentItemKeyString(key: SessionContentItemKey): string {
  return JSON.stringify([key.turn_id, key.agent_iteration, key.item_id])
}

function cloneItem(value: unknown, name: string): SessionContentItem {
  const source = object(value, name)
  exactKeys(source, itemRequired, itemOptional, name)
  const key = cloneKey(source.key, `${name}.key`)
  const kind = stringValue(source.kind, `${name}.kind`)
  if (!itemKinds.has(kind)) throw new Error(`${name}.kind is not supported`)
  const visibility = stringValue(source.visibility, `${name}.visibility`)
  if (!visibilities.has(visibility)) throw new Error(`${name}.visibility is not supported`)
  const audience = stringValue(source.audience, `${name}.audience`)
  if (!audiences.has(audience)) throw new Error(`${name}.audience is not supported`)
  const status = source.status === undefined ? undefined : stringValue(source.status, `${name}.status`)
  if (status !== undefined && !itemStatuses.has(status)) throw new Error(`${name}.status is not supported`)
  return {
    key,
    seq: integer(source.seq, `${name}.seq`, 1),
    created_at: timestamp(source.created_at, `${name}.created_at`),
    kind: kind as SessionContentItem['kind'],
    visibility: visibility as SessionContentItem['visibility'],
    audience: audience as SessionContentItem['audience'],
    ...(status === undefined ? {} : { status: status as SessionContentItem['status'] }),
    ...(source.message === undefined ? {} : { message: cloneMessage(source.message, `${name}.message`) }),
  }
}

function cloneDescriptor(value: unknown, name = 'history.descriptor'): SessionContentHistoryDescriptor {
  const source = object(value, name)
  exactKeys(source, descriptorRequired, descriptorOptional, name)
  const result: SessionContentHistoryDescriptor = {
    limit: integer(source.limit, `${name}.limit`, 1),
    align_turn: booleanValue(source.align_turn, `${name}.align_turn`),
    visible_only: booleanValue(source.visible_only, `${name}.visible_only`),
    has_more_before: booleanValue(source.has_more_before, `${name}.has_more_before`),
    has_more_after: booleanValue(source.has_more_after, `${name}.has_more_after`),
    ...(source.before_item_seq === undefined ? {} : { before_item_seq: historyCursor(source.before_item_seq, `${name}.before_item_seq`) }),
    ...(source.after_item_seq === undefined ? {} : { after_item_seq: historyCursor(source.after_item_seq, `${name}.after_item_seq`) }),
    ...(source.oldest_item_seq === undefined ? {} : { oldest_item_seq: historyCursor(source.oldest_item_seq, `${name}.oldest_item_seq`) }),
    ...(source.newest_item_seq === undefined ? {} : { newest_item_seq: historyCursor(source.newest_item_seq, `${name}.newest_item_seq`) }),
  }
  if (result.before_item_seq !== undefined && result.after_item_seq !== undefined) throw new Error(`${name} cannot contain both before and after cursors`)
  if (result.align_turn) throw new Error(`${name}.align_turn is not supported by D1`)
  if (result.limit > 1000) throw new Error(`${name}.limit is too large`)
  return result
}

function cloneHistory(value: unknown, name = 'history'): SessionContentHistoryWindow {
  const source = object(value, name)
  exactKeys(source, ['items', 'descriptor'], [], name)
  if (!Array.isArray(source.items)) throw new Error(`${name}.items must be an array`)
  const items = source.items.map((entry, index) => cloneItem(entry, `${name}.items[${index}]`))
  const descriptor = cloneDescriptor(source.descriptor, `${name}.descriptor`)
  if (items.length > descriptor.limit) throw new Error(`${name}.items exceeds descriptor.limit`)
  const seen = new Set<string>()
  for (let index = 0; index < items.length; index += 1) {
    const item = items[index]
    const key = sessionContentItemKeyString(item.key)
    if (seen.has(key)) throw new Error(`${name} contains duplicate item identity`)
    seen.add(key)
    if (index > 0 && items[index - 1].seq >= item.seq) throw new Error(`${name}.items are not ordered by seq`)
  }
  if (items.length === 0) {
    if (descriptor.oldest_item_seq !== undefined || descriptor.newest_item_seq !== undefined) throw new Error(`${name} has bounds without items`)
  } else {
    if (descriptor.oldest_item_seq !== String(items[0].seq) || descriptor.newest_item_seq !== String(items[items.length - 1].seq)) throw new Error(`${name} bounds do not match items`)
  }
  if (descriptor.before_item_seq !== undefined && items.length > 0 && BigInt(items[items.length - 1].seq) >= BigInt(descriptor.before_item_seq)) throw new Error(`${name} is not before before_item_seq`)
  if (descriptor.after_item_seq !== undefined && items.length > 0 && BigInt(items[0].seq) <= BigInt(descriptor.after_item_seq)) throw new Error(`${name} is not after after_item_seq`)
  if (descriptor.before_item_seq === undefined && descriptor.after_item_seq === undefined && descriptor.has_more_after) throw new Error(`${name} latest window cannot have newer items`)
  if (items.length === 0 && descriptor.before_item_seq === undefined && descriptor.after_item_seq === undefined && (descriptor.has_more_before || descriptor.has_more_after)) throw new Error(`${name} empty latest window cannot advertise more items`)
  return { items, descriptor }
}

function cloneSettlement(value: unknown, name: string): SessionContentSettlementWatermark {
  const source = object(value, name)
  exactKeys(source, ['resource_revision', 'run_cursor', 'verified', 'covered_items'], [], name)
  const verified = booleanValue(source.verified, `${name}.verified`)
  if (!Array.isArray(source.covered_items)) throw new Error(`${name}.covered_items must be an array`)
  const covered_items: SessionContentTransientItemWatermark[] = source.covered_items.map((entry, index) => {
    const item = object(entry, `${name}.covered_items[${index}]`)
    exactKeys(item, ['turn_id', 'agent_iteration', 'item_id', 'run_cursor'], [], `${name}.covered_items[${index}]`)
    return {
      turn_id: identifier(item.turn_id, `${name}.covered_items[${index}].turn_id`),
      agent_iteration: integer(item.agent_iteration, `${name}.covered_items[${index}].agent_iteration`, 1),
      item_id: identifier(item.item_id, `${name}.covered_items[${index}].item_id`),
      run_cursor: decimal(item.run_cursor, `${name}.covered_items[${index}].run_cursor`, true),
    }
  })
  if (!verified && covered_items.length !== 0) throw new Error(`${name} unverified watermark cannot cover items`)
  const run_cursor = decimal(source.run_cursor, `${name}.run_cursor`, true)
  for (const item of covered_items) if (compareRunCursor(item.run_cursor, run_cursor) > 0) throw new Error(`${name} covered item is newer than watermark`)
  return { resource_revision: revision(source.resource_revision, `${name}.resource_revision`), run_cursor, verified, covered_items }
}

function cloneActiveRun(value: unknown, sessionID: string, name = 'active_run'): SessionContentActiveRun {
  const source = object(value, name)
  exactKeys(source, activeRunRequired, activeRunOptional, name)
  const runID = identifier(source.run_id, `${name}.run_id`)
  if (stringValue(source.session_id, `${name}.session_id`) !== sessionID) throw new Error(`${name}.session_id does not match resource`)
  if (source.status !== 'running') throw new Error(`${name}.status must be running`)
  if (!booleanValue(source.recoverable, `${name}.recoverable`)) throw new Error(`${name} must be recoverable`)
  const result: SessionContentActiveRun = {
    run_id: runID,
    session_id: sessionID,
    started_at: timestamp(source.started_at, `${name}.started_at`),
    status: 'running',
    recoverable: true,
    replay_available: source.replay_available === undefined ? false : booleanValue(source.replay_available, `${name}.replay_available`),
    recovery_required: source.recovery_required === undefined ? false : booleanValue(source.recovery_required, `${name}.recovery_required`),
    ...(source.turn_id === undefined ? {} : { turn_id: identifier(source.turn_id, `${name}.turn_id`) }),
    ...(source.run_epoch === undefined ? {} : { run_epoch: identifier(source.run_epoch, `${name}.run_epoch`) }),
    ...(source.run_cursor === undefined ? {} : { run_cursor: decimal(source.run_cursor, `${name}.run_cursor`, true) }),
    ...(source.replay_from_cursor === undefined ? {} : { replay_from_cursor: decimal(source.replay_from_cursor, `${name}.replay_from_cursor`, true) }),
    ...(source.replay_to_cursor === undefined ? {} : { replay_to_cursor: decimal(source.replay_to_cursor, `${name}.replay_to_cursor`, true) }),
    ...(source.durable_settlement_watermark === undefined ? {} : { durable_settlement_watermark: cloneSettlement(source.durable_settlement_watermark, `${name}.durable_settlement_watermark`) }),
  }
  if (result.replay_available && (result.run_epoch === undefined || result.run_cursor === undefined || result.replay_from_cursor === undefined || result.replay_to_cursor === undefined)) throw new Error(`${name} replay range is incomplete`)
  if (result.replay_available && compareRunCursor(result.replay_from_cursor as string, result.replay_to_cursor as string) > 0) throw new Error(`${name}.replay_from_cursor must not be greater than replay_to_cursor`)
  if (result.replay_available && compareRunCursor(result.replay_to_cursor as string, result.run_cursor as string) > 0) throw new Error(`${name}.replay_to_cursor must not be greater than run_cursor`)
  return result
}

function cloneCheckpoint(value: unknown, name: string): SessionContentCompactionCheckpoint {
  const source = object(value, name)
  exactKeys(source, checkpointRequired, checkpointOptional, name)
  return {
    id: identifier(source.id, `${name}.id`),
    created_at: timestamp(source.created_at, `${name}.created_at`),
    reason: stringValue(source.reason, `${name}.reason`),
    phase: stringValue(source.phase, `${name}.phase`),
    trigger: stringValue(source.trigger, `${name}.trigger`),
    summary_item_id: identifier(source.summary_item_id, `${name}.summary_item_id`),
    replacement_history: stringArray(source.replacement_history, `${name}.replacement_history`, true),
    ...(source.from_item_id === undefined ? {} : { from_item_id: identifier(source.from_item_id, `${name}.from_item_id`) }),
    ...(source.to_item_id === undefined ? {} : { to_item_id: identifier(source.to_item_id, `${name}.to_item_id`) }),
    ...(source.previous_active_history === undefined ? {} : { previous_active_history: stringArray(source.previous_active_history, `${name}.previous_active_history`, true) }),
    ...(source.summary_provider === undefined ? {} : { summary_provider: stringValue(source.summary_provider, `${name}.summary_provider`, true) }),
    ...(source.summary_model === undefined ? {} : { summary_model: stringValue(source.summary_model, `${name}.summary_model`, true) }),
  }
}

function cloneCompaction(value: unknown, name = 'compaction'): SessionContentCompaction {
  const source = object(value, name)
  exactKeys(source, compactionRequired, [], name)
  if (!Array.isArray(source.checkpoints)) throw new Error(`${name}.checkpoints must be an array`)
  const checkpoints = source.checkpoints.map((entry, index) => cloneCheckpoint(entry, `${name}.checkpoints[${index}]`))
  const seen = new Set<string>()
  for (const checkpoint of checkpoints) {
    if (seen.has(checkpoint.id)) throw new Error(`${name} contains duplicate checkpoint id`)
    seen.add(checkpoint.id)
  }
  const truncated = booleanValue(source.truncated, `${name}.truncated`)
  if (truncated && checkpoints.length === 0) throw new Error(`${name}.truncated requires a checkpoint`)
  return { checkpoints, truncated }
}

function cloneMetadata(value: unknown, sessionID: string, name = 'session'): SessionContentMetadata {
  const source = object(value, name)
  exactKeys(source, metadataRequired, metadataOptional, name)
  if (stringValue(source.id, `${name}.id`) !== sessionID) throw new Error(`${name}.id does not match resource`)
  if (integer(source.version, `${name}.version`, 1) !== 2) throw new Error(`${name}.version is not supported`)
  const status = stringValue(source.status, `${name}.status`)
  if (!sessionStatuses.has(status)) throw new Error(`${name}.status is not supported`)
  const result: SessionContentMetadata = {
    id: sessionID,
    version: 2,
    created_at: timestamp(source.created_at, `${name}.created_at`),
    updated_at: timestamp(source.updated_at, `${name}.updated_at`),
    archived: booleanValue(source.archived, `${name}.archived`),
    last_used_at: timestamp(source.last_used_at, `${name}.last_used_at`),
    has_unread_result: booleanValue(source.has_unread_result, `${name}.has_unread_result`),
    status: status as SessionContentMetadata['status'],
    show_reasoning: booleanValue(source.show_reasoning, `${name}.show_reasoning`),
    full_access: booleanValue(source.full_access, `${name}.full_access`),
    debug: (() => { const debug = object(source.debug, `${name}.debug`); exactKeys(debug, ['request_bodies'], [], `${name}.debug`); return { request_bodies: booleanValue(debug.request_bodies, `${name}.debug.request_bodies`) } })(),
    context: jsonObject(source.context, `${name}.context`),
    save_tool_results: booleanValue(source.save_tool_results, `${name}.save_tool_results`),
  }
  const optionalStringFields = ['display_name', 'parent_session_id', 'root_session_id', 'archived_at', 'current_run_id', 'running_run_id', 'running_turn_id', 'interrupted_run_id', 'interrupted_turn_id', 'interrupted_at', 'latest_run_id', 'last_run_id', 'last_run_status', 'provider', 'model_profile', 'model_id', 'reasoning_level', 'project_id', 'cwd', 'created_cwd', 'config_path', 'config_dir']
  for (const field of optionalStringFields) if (source[field] !== undefined) {
    const value = field.endsWith('_at') ? timestamp(source[field], `${name}.${field}`) : stringValue(source[field], `${name}.${field}`, field === 'display_name')
    ;(result as unknown as Record<string, unknown>)[field] = value
  }
  if (source.created_by !== undefined) {
    const value = stringValue(source.created_by, `${name}.created_by`)
    if (!createdBy.has(value)) throw new Error(`${name}.created_by is not supported`)
    ;(result as unknown as Record<string, unknown>).created_by = value
  }
  if (source.spawn_depth !== undefined) (result as unknown as Record<string, unknown>).spawn_depth = integer(source.spawn_depth, `${name}.spawn_depth`, 0)
  if (source.last_run_status !== undefined) {
    const value = stringValue(source.last_run_status, `${name}.last_run_status`)
    if (!runStatuses.has(value)) throw new Error(`${name}.last_run_status is not supported`)
    ;(result as unknown as Record<string, unknown>).last_run_status = value
  }
  if (source.archived && source.archived_at === undefined) throw new Error(`${name}.archived_at is required when archived`)
  if (!source.archived && source.archived_at !== undefined) throw new Error(`${name}.archived_at must be omitted when not archived`)
  if (source.pricing !== undefined) (result as unknown as Record<string, unknown>).pricing = jsonObject(source.pricing, `${name}.pricing`)
  if (source.model_parameters !== undefined) (result as unknown as Record<string, unknown>).model_parameters = jsonObject(source.model_parameters, `${name}.model_parameters`)
  for (const field of ['enabled_tools', 'enabled_mcp', 'enabled_skills']) if (source[field] !== undefined) (result as unknown as Record<string, unknown>)[field] = stringArray(source[field], `${name}.${field}`)
  if (source.active_history !== undefined) (result as unknown as Record<string, unknown>).active_history = stringArray(source.active_history, `${name}.active_history`, true)
  if (source.status === 'running' && source.running_run_id === undefined && source.running_turn_id === undefined) {
    // The Go schema allows a legacy turn-only running state, but a running
    // descriptor is still required to carry an identity when one is present.
  }
  return result
}

function cloneSnapshot(value: unknown, sessionID: string): SessionContentSnapshot {
  const source = object(value, 'session_content snapshot')
  exactKeys(source, ['schema_version', 'session', 'history', 'active_run', 'compaction'], [], 'session_content snapshot')
  if (source.schema_version !== 1) throw new Error('unsupported session content schema version')
  const session = cloneMetadata(source.session, sessionID)
  const history = cloneHistory(source.history)
  const activeRun = source.active_run === null ? null : cloneActiveRun(source.active_run, sessionID)
  const compaction = cloneCompaction(source.compaction)
  if (session.status === 'running' && activeRun === null && session.running_turn_id === undefined) throw new Error('running metadata requires active_run')
  if (session.status !== 'running' && activeRun !== null) throw new Error('active_run requires running metadata')
  if (activeRun && session.running_run_id !== activeRun.run_id) throw new Error('active run does not match metadata')
  if (activeRun && (session.running_turn_id ?? '') !== (activeRun.turn_id ?? '')) throw new Error('active turn does not match metadata')
  return { schema_version: 1, session, history, active_run: activeRun, compaction }
}

function copyState(snapshot: SessionContentSnapshot, revisionValue: string, transientRun: SessionRunState | null): SessionContentState {
  return { snapshot, durableResourceRevision: revisionValue, transientRun }
}

function copyMap<T>(value: Readonly<Record<string, T>>): Record<string, T> {
  return { ...value }
}

function durableInlineLength(snapshot: SessionContentSnapshot, key: SessionContentItemKey, reasoning: boolean): number {
  const item = findItem(snapshot, key)
  const text = textValue(item, reasoning)
  return text === undefined ? 0 : text.length
}

function keyEqual(left: SessionContentItemKey, right: SessionContentItemKey): boolean {
  return left.turn_id === right.turn_id && left.agent_iteration === right.agent_iteration && left.item_id === right.item_id
}

function findItem(snapshot: SessionContentSnapshot, key: SessionContentItemKey): SessionContentItem | undefined {
  return snapshot.history.items.find((item) => keyEqual(item.key, key))
}

function textValue(item: SessionContentItem | undefined, reasoning: boolean): string | undefined {
  const text = reasoning ? item?.message?.reasoning : item?.message?.content
  return text?.inline
}

function coveredByDurable(snapshot: SessionContentSnapshot, watermark: SessionContentSettlementWatermark): boolean {
  return watermark.covered_items.every((covered) => findItem(snapshot, covered) !== undefined)
}

function revisionCovers(current: string, target: string): boolean {
  if (/^[0-9]+$/u.test(current) && /^[0-9]+$/u.test(target)) return BigInt(current) >= BigInt(target)
  return current === target
}

function maybeClearSettled(state: SessionContentState, revisionValue: string): SessionContentState {
  const run = state.transientRun
  if (!run?.settlement || !run.settlement.verified || !revisionCovers(revisionValue, run.settlement.resource_revision) || !coveredByDurable(state.snapshot, run.settlement)) return state
  return copyState(state.snapshot, revisionValue, null)
}

function normalizeTextOverlay(snapshot: SessionContentSnapshot, run: SessionRunState | null): SessionRunState | null {
  if (!run) return null
  const text = copyMap(run.text)
  const reasoning = copyMap(run.reasoning)
  const normalize = (map: Record<string, SessionTransientText>, isReasoning: boolean): void => {
    for (const key of Object.keys(map)) {
      const entry = map[key]
      const item = findItem(snapshot, entry.key)
      const durable = textValue(item, isReasoning)
      if (durable === undefined) continue
      // Reasoning has no durable checkpoint marker in D2. Once the durable
      // item for the stable identity exists, it is authoritative; this is an
      // identity lookup, never a text-based bubble match.
      if (isReasoning && entry.checkpointLength === undefined) {
        delete map[key]
        continue
      }
      if (!entry.checkpointed) continue
      const consumed = Math.max(0, Math.min(entry.text.length, durable.length - entry.baseLength))
      const tail = entry.text.slice(consumed)
      if (tail.length === 0) delete map[key]
      else map[key] = { ...entry, text: tail, baseLength: durable.length }
    }
  }
  normalize(text, false)
  normalize(reasoning, true)
  return { ...run, text, reasoning }
}

function eventObject(value: unknown): Record<string, unknown> {
  const source = object(value, 'subscription event')
  const type = stringValue(source.type, 'subscription event.type')
  exactKeys(source, ['type', 'session_id', 'run_id', 'run_cursor'], [
    'turn_id', 'agent_iteration', 'item_id', 'delta', 'durable_text_length', 'durable_checkpointed',
    'tool_call_id', 'name', 'arguments', 'arguments_delta', 'content', 'is_error', 'prompts', 'status',
    'durable_settlement_watermark',
  ], 'subscription event')
  if (!['text.delta', 'reasoning.delta', 'tool.requested', 'tool.running', 'tool.progress', 'tool.finished', 'run.prompt_queue', 'run.prompt_appended', 'run.started', 'run.settled'].includes(type)) throw new Error(`unknown subscription event type ${type}`)
  return source
}

function eventIdentity(source: Record<string, unknown>, sessionID: string): { type: string; sessionID: string; runID: string; cursor: string; turnID?: string; iteration: number } {
  if (stringValue(source.session_id, 'subscription event.session_id') !== sessionID) throw new Error('subscription event session does not match resource')
  const iteration = source.agent_iteration === undefined ? 0 : integer(source.agent_iteration, 'subscription event.agent_iteration', 1)
  return {
    type: stringValue(source.type, 'subscription event.type'),
    sessionID,
    runID: identifier(source.run_id, 'subscription event.run_id'),
    cursor: decimal(source.run_cursor, 'subscription event.run_cursor', true),
    ...(source.turn_id === undefined ? {} : { turnID: identifier(source.turn_id, 'subscription event.turn_id') }),
    iteration,
  }
}

function eventFields(source: Record<string, unknown>, required: readonly string[], optional: readonly string[], name: string): void {
  exactKeys(source, [...['type', 'session_id', 'run_id', 'run_cursor', ...required]], ['turn_id', 'agent_iteration', ...optional], name)
  for (const key of required) if (!own(source, key)) throw new Error(`${name}.${key} is required`)
}

function baseRunState(runEpoch: string, identity: ReturnType<typeof eventIdentity>, cursor: string, turnID?: string): SessionRunState {
  return {
    runEpoch,
    runID: identity.runID,
    runCursor: cursor,
    ...(turnID === undefined ? {} : { turnID }),
    status: 'running',
    text: {}, reasoning: {}, tools: {}, promptQueue: [], appendedPrompts: [], stale: false, recoveryRequired: false,
  }
}

function snapshotTransientBaseline(snapshot: SessionContentSnapshot): SessionRunState | null {
  const active = snapshot.active_run
  if (!active?.replay_available) return null
  // active_run.run_cursor is the server's current cursor, not the cursor
  // applied by this client. A fresh subscription starts immediately before
  // the retained replay window and advances from there as events arrive.
  // Keep this arithmetic in bigint form because cursors are decimal uint64
  // values and may exceed Number.MAX_SAFE_INTEGER.
  const first = BigInt(active.replay_from_cursor as string)
  // Run events produced by the server currently start at one. The protocol
  // validator nevertheless permits the decimal zero, so keep a valid
  // non-negative client cursor for that forward-compatible descriptor rather
  // than rejecting a snapshot that Go accepts.
  return {
    runEpoch: active.run_epoch as string,
    runID: active.run_id,
    runCursor: (first === 0n ? 0n : first - 1n).toString(),
    ...(active.turn_id === undefined ? {} : { turnID: active.turn_id }),
    status: 'running',
    text: {}, reasoning: {}, tools: {}, promptQueue: [], appendedPrompts: [], stale: false, recoveryRequired: false,
  }
}

// Validate the complete variant before looking at the cursor. In particular,
// a duplicate cursor is only an idempotent duplicate after its event shape is
// known to be valid; otherwise malformed wire data could bypass this adapter
// merely by reusing an already-applied cursor.
function validateEventVariant(event: Record<string, unknown>, identity: ReturnType<typeof eventIdentity>): void {
  switch (identity.type) {
    case 'run.started':
      eventFields(event, ['status'], [], 'run.started')
      if (event.status !== 'running') throw new Error('run.started status must be running')
      break
    case 'text.delta':
    case 'reasoning.delta':
      eventFields(event, ['item_id', 'delta'], ['durable_text_length', 'durable_checkpointed'], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error(`${identity.type} identity is incomplete`)
      identifier(event.item_id, `${identity.type}.item_id`)
      stringValue(event.delta, `${identity.type}.delta`)
      if (event.durable_text_length !== undefined) integer(event.durable_text_length, `${identity.type}.durable_text_length`, 0)
      if (event.durable_checkpointed !== undefined) booleanValue(event.durable_checkpointed, `${identity.type}.durable_checkpointed`)
      break
    case 'tool.requested':
    case 'tool.running':
      eventFields(event, ['tool_call_id', 'name'], ['arguments'], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error(`${identity.type} identity is incomplete`)
      identifier(event.tool_call_id, `${identity.type}.tool_call_id`)
      identifier(event.name, `${identity.type}.name`)
      if (event.arguments !== undefined) stringValue(event.arguments, `${identity.type}.arguments`, true)
      break
    case 'tool.progress':
      eventFields(event, ['tool_call_id', 'name', 'arguments_delta'], [], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error('tool.progress identity is incomplete')
      identifier(event.tool_call_id, 'tool.progress.tool_call_id')
      identifier(event.name, 'tool.progress.name')
      stringValue(event.arguments_delta, 'tool.progress.arguments_delta', true)
      break
    case 'tool.finished':
      eventFields(event, ['tool_call_id', 'name', 'is_error'], ['content'], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error('tool.finished identity is incomplete')
      identifier(event.tool_call_id, 'tool.finished.tool_call_id')
      identifier(event.name, 'tool.finished.name')
      booleanValue(event.is_error, 'tool.finished.is_error')
      if (event.content !== undefined) stringValue(event.content, 'tool.finished.content', true)
      break
    case 'run.prompt_queue': {
      eventFields(event, ['prompts'], [], identity.type)
      if (!Array.isArray(event.prompts)) throw new Error('run.prompt_queue.prompts must be an array')
      event.prompts.forEach((entry, index) => {
        const prompt = object(entry, `run.prompt_queue.prompts[${index}]`)
        exactKeys(prompt, ['id', 'content', 'steer'], [], `run.prompt_queue.prompts[${index}]`)
        identifier(prompt.id, `run.prompt_queue.prompts[${index}].id`)
        stringValue(prompt.content, `run.prompt_queue.prompts[${index}].content`, true)
        booleanValue(prompt.steer, `run.prompt_queue.prompts[${index}].steer`)
      })
      break
    }
    case 'run.prompt_appended':
      eventFields(event, ['prompts'], [], identity.type)
      if (!Array.isArray(event.prompts)) throw new Error('run.prompt_appended.prompts must be an array')
      event.prompts.forEach((entry, index) => stringValue(entry, `run.prompt_appended.prompts[${index}]`, true))
      break
    case 'run.settled': {
      eventFields(event, ['status', 'durable_settlement_watermark'], [], identity.type)
      const status = stringValue(event.status, 'run.settled.status')
      if (!runStatuses.has(status) || status === 'running') throw new Error('invalid run.settled status')
      const settlement = cloneSettlement(event.durable_settlement_watermark, 'run.settled.durable_settlement_watermark')
      if (compareRunCursor(settlement.run_cursor, identity.cursor) > 0) throw new Error('settlement cursor is after event cursor')
      break
    }
  }
}

function updateRun(state: SessionContentState, event: Record<string, unknown>, sessionID: string, context?: ReplicaApplyContext): SessionContentState {
  const identity = eventIdentity(event, sessionID)
  validateEventVariant(event, identity)
  let run = state.transientRun
  if (run && run.runID !== identity.runID) {
    if (run.status !== 'committed' && run.status !== 'failed' && run.status !== 'interrupted' && run.status !== 'cancelled') throw new Error('subscription event run identity changed while run is active')
    if (identity.type !== 'run.started') throw new Error('subscription event belongs to a different run')
    const activeEpoch = state.snapshot.active_run?.run_id === identity.runID ? state.snapshot.active_run.run_epoch : undefined
    run = null
    if (activeEpoch === undefined && state.transientRun?.runEpoch === '') throw new Error('new run epoch is unavailable')
  }
  if (!run) {
    const active = state.snapshot.active_run
    if (active && active.run_id === identity.runID) {
      run = baseRunState(active.run_epoch ?? '', identity, active.run_cursor ?? '0', active.turn_id)
    } else {
      if (identity.type !== 'run.started' || identity.cursor !== '1') throw new Error('run event starts with a cursor gap')
      run = baseRunState('', identity, '0')
    }
  }
  const currentCursor = decimal(run.runCursor, 'run cursor', true)
  const incomingCursor = decimal(identity.cursor, 'run cursor', true)
  if (run.runID !== identity.runID) throw new Error('run identity does not match overlay')
  if (compareRunCursor(incomingCursor, currentCursor) <= 0) return state
  if (BigInt(incomingCursor) !== BigInt(currentCursor) + 1n) throw new Error('run cursor is not contiguous')
  if (run.status !== 'running') throw new Error('terminal run received another event')
  let next: SessionRunState = { ...run, runCursor: incomingCursor, turnID: identity.turnID ?? run.turnID, stale: false }
  switch (identity.type) {
    case 'run.started':
      eventFields(event, ['status'], [], 'run.started')
      if (event.status !== 'running') throw new Error('run.started status must be running')
      next = { ...next, status: 'running' }
      break
    case 'text.delta':
    case 'reasoning.delta': {
      eventFields(event, ['item_id', 'delta'], ['durable_text_length', 'durable_checkpointed'], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error(`${identity.type} identity is incomplete`)
      const key: SessionContentItemKey = { turn_id: identity.turnID, agent_iteration: identity.iteration, item_id: identifier(event.item_id, `${identity.type}.item_id`) }
      const map = identity.type === 'text.delta' ? copyMap(next.text) : copyMap(next.reasoning)
      const keyString = sessionContentItemKeyString(key)
      const old = map[keyString]
      const checkpointLength = event.durable_text_length === undefined ? old?.checkpointLength : integer(event.durable_text_length, `${identity.type}.durable_text_length`, 0)
      const checkpointed = event.durable_checkpointed === undefined ? old?.checkpointed ?? false : booleanValue(event.durable_checkpointed, `${identity.type}.durable_checkpointed`)
      const delta = stringValue(event.delta, `${identity.type}.delta`, true)
      const durableLength = durableInlineLength(state.snapshot, key, identity.type === 'reasoning.delta')
      // A committed checkpoint is published before its corresponding
      // transient delta. If the durable item already covers that checkpoint,
      // consume the frame without ever manufacturing a duplicate bubble.
      if (checkpointed && checkpointLength !== undefined && durableLength >= checkpointLength && old === undefined) {
        next = identity.type === 'text.delta' ? { ...next, text: map } : { ...next, reasoning: map }
        break
      }
      const text = `${old?.text ?? ''}${delta}`
      map[keyString] = {
        key,
        text,
        baseLength: old?.baseLength ?? durableLength,
        ...(checkpointLength === undefined ? {} : { checkpointLength }),
        checkpointed,
      }
      next = identity.type === 'text.delta' ? { ...next, text: map } : { ...next, reasoning: map }
      break
    }
    case 'tool.requested':
    case 'tool.running': {
      eventFields(event, ['tool_call_id', 'name'], ['arguments'], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error(`${identity.type} identity is incomplete`)
      const toolID = identifier(event.tool_call_id, `${identity.type}.tool_call_id`)
      const old = next.tools[toolID]
      if (old?.status === 'finished') throw new Error('finished tool received another lifecycle event')
      const tools = copyMap(next.tools)
      tools[toolID] = {
        tool_call_id: toolID, turn_id: identity.turnID, agent_iteration: identity.iteration,
        name: identifier(event.name, `${identity.type}.name`), status: identity.type === 'tool.requested' ? 'requested' : 'running',
        arguments: event.arguments === undefined ? old?.arguments ?? '' : stringValue(event.arguments, `${identity.type}.arguments`, true),
      }
      next = { ...next, tools }
      break
    }
    case 'tool.progress': {
      eventFields(event, ['tool_call_id', 'name', 'arguments_delta'], [], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error('tool.progress identity is incomplete')
      const toolID = identifier(event.tool_call_id, 'tool.progress.tool_call_id')
      const old = next.tools[toolID]
      if (old?.status === 'finished') throw new Error('finished tool received progress')
      const tools = copyMap(next.tools)
      tools[toolID] = {
        tool_call_id: toolID, turn_id: identity.turnID, agent_iteration: identity.iteration,
        name: identifier(event.name, 'tool.progress.name'), status: old?.status ?? 'running',
        arguments: `${old?.arguments ?? ''}${stringValue(event.arguments_delta, 'tool.progress.arguments_delta', true)}`,
      }
      next = { ...next, tools }
      break
    }
    case 'tool.finished': {
      eventFields(event, ['tool_call_id', 'name', 'is_error'], ['content'], identity.type)
      if (!identity.turnID || identity.iteration <= 0) throw new Error('tool.finished identity is incomplete')
      const toolID = identifier(event.tool_call_id, 'tool.finished.tool_call_id')
      const old = next.tools[toolID]
      if (old?.status === 'finished') throw new Error('finished tool received another finish')
      const tools = copyMap(next.tools)
      tools[toolID] = {
        tool_call_id: toolID, turn_id: identity.turnID, agent_iteration: identity.iteration,
        name: identifier(event.name, 'tool.finished.name'), status: 'finished', arguments: old?.arguments ?? '',
        ...(event.content === undefined ? {} : { content: stringValue(event.content, 'tool.finished.content', true) }),
        is_error: booleanValue(event.is_error, 'tool.finished.is_error'),
      }
      next = { ...next, tools }
      break
    }
    case 'run.prompt_queue': {
      eventFields(event, ['prompts'], [], identity.type)
      if (!Array.isArray(event.prompts)) throw new Error('run.prompt_queue.prompts must be an array')
      const prompts: SessionPrompt[] = event.prompts.map((entry, index) => {
        const prompt = object(entry, `run.prompt_queue.prompts[${index}]`)
        exactKeys(prompt, ['id', 'content', 'steer'], [], `run.prompt_queue.prompts[${index}]`)
        return { id: identifier(prompt.id, `run.prompt_queue.prompts[${index}].id`), content: stringValue(prompt.content, `run.prompt_queue.prompts[${index}].content`, true), steer: booleanValue(prompt.steer, `run.prompt_queue.prompts[${index}].steer`) }
      })
      next = { ...next, promptQueue: prompts }
      break
    }
    case 'run.prompt_appended': {
      eventFields(event, ['prompts'], [], identity.type)
      if (!Array.isArray(event.prompts)) throw new Error('run.prompt_appended.prompts must be an array')
      const appended = event.prompts.map((entry, index) => stringValue(entry, `run.prompt_appended.prompts[${index}]`, true))
      next = { ...next, appendedPrompts: [...next.appendedPrompts, ...appended] }
      break
    }
    case 'run.settled': {
      eventFields(event, ['status', 'durable_settlement_watermark'], [], identity.type)
      const status = stringValue(event.status, 'run.settled.status')
      if (!runStatuses.has(status) || status === 'running') throw new Error('invalid run.settled status')
      const settlement = cloneSettlement(event.durable_settlement_watermark, 'run.settled.durable_settlement_watermark')
      if (compareRunCursor(settlement.run_cursor, incomingCursor) > 0) throw new Error('settlement cursor is after event cursor')
      next = { ...next, status: status as SessionRunState['status'], settlement, stale: !settlement.verified, recoveryRequired: !settlement.verified }
      break
    }
  }
  const updated = copyState(state.snapshot, context?.resourceRevision ?? state.durableResourceRevision, next)
  return maybeClearSettled(updated, updated.durableResourceRevision)
}

function applyDurableOperations(previous: SessionContentState, operations: readonly ChangeOperation[], context?: ReplicaApplyContext): SessionContentState {
  if (!Array.isArray(operations) || operations.length === 0) throw new Error('session content change must contain operations')
  let snapshot = previous.snapshot
  for (const raw of operations) {
    const operation = object(raw, 'session content operation')
    const op = stringValue(operation.op, 'session content operation.op')
    if (!operationNames.has(op)) throw new Error(`unknown session content operation ${op}`)
    switch (op) {
      case 'metadata.replace': {
        exactKeys(operation, ['op', 'metadata'], [], op)
        snapshot = { ...snapshot, session: cloneMetadata(operation.metadata, previous.snapshot.session.id, `${op}.metadata`) }
        break
      }
      case 'item.upsert': {
        exactKeys(operation, ['op', 'item'], [], op)
        const item = cloneItem(operation.item, `${op}.item`)
        const items = snapshot.history.items.filter((current) => !keyEqual(current.key, item.key))
        items.push(item)
        items.sort((left, right) => left.seq - right.seq)
        snapshot = { ...snapshot, history: { ...snapshot.history, items } }
        break
      }
      case 'item.remove': {
        exactKeys(operation, ['op', 'key'], [], op)
        const key = cloneKey(operation.key, `${op}.key`)
        snapshot = { ...snapshot, history: { ...snapshot.history, items: snapshot.history.items.filter((item) => !keyEqual(item.key, key)) } }
        break
      }
      case 'history.window.replace': {
        exactKeys(operation, ['op', 'window'], [], op)
        snapshot = { ...snapshot, history: cloneHistory(operation.window, `${op}.window`) }
        break
      }
      case 'history.window.descriptor.replace': {
        exactKeys(operation, ['op', 'descriptor'], [], op)
        const descriptor = cloneDescriptor(operation.descriptor, `${op}.descriptor`)
        snapshot = { ...snapshot, history: { ...snapshot.history, descriptor } }
        break
      }
      case 'active_run.replace': {
        exactKeys(operation, ['op', 'active_run'], [], op)
        const activeRun = cloneActiveRun(operation.active_run, previous.snapshot.session.id, `${op}.active_run`)
        snapshot = { ...snapshot, active_run: activeRun }
        break
      }
      case 'active_run.clear':
        exactKeys(operation, ['op'], [], op)
        snapshot = { ...snapshot, active_run: null }
        break
      case 'compaction.replace':
        exactKeys(operation, ['op', 'compaction'], [], op)
        snapshot = { ...snapshot, compaction: cloneCompaction(operation.compaction, `${op}.compaction`) }
        break
    }
  }
  // Re-validate cross-field relationships after an atomic multi-operation
  // change. This also catches a descriptor/item combination that is valid in
  // isolation but invalid as a completed resource projection.
  const checked = cloneSnapshot(snapshot, previous.snapshot.session.id)
  if (previous.transientRun && checked.active_run && checked.active_run.run_id === previous.transientRun.runID &&
    checked.active_run.run_epoch !== undefined && previous.transientRun.runEpoch !== '' &&
    checked.active_run.run_epoch !== previous.transientRun.runEpoch) {
    throw new Error('active run epoch changed while a transient overlay is active')
  }
  const transientRun = previous.transientRun && checked.active_run?.run_id === previous.transientRun.runID &&
    previous.transientRun.runEpoch === '' && checked.active_run.run_epoch !== undefined
    ? { ...previous.transientRun, runEpoch: checked.active_run.run_epoch }
    : previous.transientRun
  const next = copyState(checked, context?.resourceRevision ?? previous.durableResourceRevision, transientRun)
  return maybeClearSettled({ ...next, transientRun: normalizeTextOverlay(checked, next.transientRun) }, next.durableResourceRevision)
}

export class SessionContentAdapter implements ResourceAdapter<SessionContentState, SubscriptionEventData> {
  readonly resourceType = 'session_content' as const

  constructor(readonly sessionID: string) {
    identifier(sessionID, 'session id')
  }

  validateResourceRevision(value: string): void {
    revision(value, 'resource_revision')
  }

  private validateContext(context?: ReplicaApplyContext): void {
    if (!context) return
    if (context.resource.type !== 'session_content' || context.resource.id !== this.sessionID) throw new Error('resource/session identity mismatch')
    revision(context.resourceRevision, 'resource_revision')
    if (!Number.isSafeInteger(context.generation) || context.generation < 0) throw new Error('generation is invalid')
  }

  decodeSnapshot(value: unknown, previous: SessionContentState | undefined, context?: ReplicaApplyContext): SessionContentState {
    this.validateContext(context)
    const snapshot = cloneSnapshot(value, this.sessionID)
    return copyState(snapshot, context?.resourceRevision ?? previous?.durableResourceRevision ?? '', snapshotTransientBaseline(snapshot))
  }

  applyChange(previous: SessionContentState, operations: readonly ChangeOperation[], context?: ReplicaApplyContext): SessionContentState {
    this.validateContext(context)
    if (previous.snapshot.session.id !== this.sessionID) throw new Error('resource/session identity mismatch')
    return applyDurableOperations(previous, operations, context)
  }

  applyTransient(previous: SessionContentState, event: SubscriptionEventData, context?: ReplicaApplyContext): SessionContentState {
    this.validateContext(context)
    if (previous.snapshot.session.id !== this.sessionID) throw new Error('resource/session identity mismatch')
    const source = eventObject(event)
    const next = updateRun(previous, source, this.sessionID, context)
    return next
  }

  clearTransient(previous: SessionContentState): SessionContentState {
    if (!previous.transientRun) return previous
    return copyState(previous.snapshot, previous.durableResourceRevision, null)
  }

  getTransientResume(previous: SessionContentState): TransientResumeToken | undefined {
    const run = previous.transientRun
    if (!run || !run.runEpoch || !run.runID) return undefined
    return { runEpoch: run.runEpoch, runID: run.runID, runCursor: run.runCursor }
  }
}

export function sessionContentSnapshotValue(state: SessionContentState): SessionContentSnapshot {
  return state.snapshot
}
