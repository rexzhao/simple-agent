import { isRFC3339Timestamp } from './datetime'
import { ProtocolDecodeError } from './errors'
import { compareRunCursor, isRunCursor, isSequence } from './sequence'
import type {
  ChangeOperation,
  ProtocolMessage,
  ResourceKey,
  SnapshotContent,
} from './types'

const messageTypes = new Set([
  'hello',
  'welcome',
  'ping',
  'pong',
  'command',
  'command_accepted',
  'command_result',
  'subscribe',
  'subscribed',
  'unsubscribe',
  'unsubscribed',
  'snapshot',
  'change',
  'subscription_event',
  'ack',
  'resync_required',
  'error',
])

const resourceTypes = new Set([
  'project_index',
  'session_index',
  'session_content',
  'provider_settings',
  'model_catalog',
  'codex_login',
])

type RawObject = Record<string, unknown>

export function decodeMessage(input: string): ProtocolMessage {
  let parsed: unknown
  try {
    parsed = JSON.parse(input)
  } catch {
    throw new ProtocolDecodeError('invalid_json', 'message is not valid JSON')
  }

  const envelope = object(parsed, 'envelope')
  if (envelope.version !== 1) {
    fail('unsupported_version', `version ${String(envelope.version)} is not supported`, 'version')
  }
  const type = requiredString(envelope, 'type', 'type')
  if (!messageTypes.has(type)) {
    fail('unknown_type', `unknown message type ${JSON.stringify(type)}`, 'type')
  }
  requiredString(envelope, 'id', 'id')
  if (has(envelope, 'timestamp')) timestamp(envelope.timestamp, 'timestamp')
  if (has(envelope, 'trace_id')) requiredString(envelope, 'trace_id', 'trace_id')
  const payload = object(envelope.payload, 'payload')

  validatePayload(type, payload)
  return envelope as unknown as ProtocolMessage
}

function validatePayload(type: string, payload: RawObject): void {
  switch (type) {
    case 'hello':
      const versions = requiredArray(payload, 'supported_versions', 'payload.supported_versions')
      if (versions.length === 0) fail('invalid_field', 'must contain at least one version', 'payload.supported_versions')
      let supportsV1 = false
      versions.forEach((version, index) => {
        positiveInteger(version, `payload.supported_versions[${index}]`)
        if (version === 1) supportsV1 = true
      })
      if (!supportsV1) fail('invalid_field', 'must include version 1', 'payload.supported_versions')
      requiredString(payload, 'client_id', 'payload.client_id')
      return
    case 'welcome':
      if (payload.selected_version !== 1) fail('invalid_field', 'must be 1', 'payload.selected_version')
      requiredString(payload, 'connection_id', 'payload.connection_id')
      requiredString(payload, 'server_epoch', 'payload.server_epoch')
      positiveInteger(payload.heartbeat_interval_ms, 'payload.heartbeat_interval_ms')
      positiveInteger(payload.max_message_bytes, 'payload.max_message_bytes')
      return
    case 'ping':
    case 'pong':
      return
    case 'command':
      requiredString(payload, 'name', 'payload.name')
      positiveInteger(payload.schema_version, 'payload.schema_version')
      requiredString(payload, 'request_id', 'payload.request_id')
      if (has(payload, 'expected_revision')) revision(payload.expected_revision, 'payload.expected_revision')
      object(payload.arguments, 'payload.arguments')
      return
    case 'command_accepted':
      requiredString(payload, 'request_id', 'payload.request_id')
      return
    case 'command_result':
      requiredString(payload, 'request_id', 'payload.request_id')
      const status = requiredString(payload, 'status', 'payload.status')
      if (status !== 'succeeded' && status !== 'failed') {
        fail('invalid_field', 'must be succeeded or failed', 'payload.status')
      }
      if (status === 'failed') {
        const commandError = object(payload.error, 'payload.error')
        validateCommandError(commandError, 'payload.error')
      } else if (has(payload, 'error')) {
        fail('invalid_field', 'must be omitted for a succeeded command', 'payload.error')
      }
      if (has(payload, 'result') && payload.result === null) {
        fail('invalid_field', 'cannot be null when provided', 'payload.result')
      }
      return
    case 'subscribe':
      requiredString(payload, 'subscription_id', 'payload.subscription_id')
      validateResource(payload.resource, 'payload.resource')
      if (has(payload, 'resume')) {
        const resume = object(payload.resume, 'payload.resume')
        requiredString(resume, 'stream_epoch', 'payload.resume.stream_epoch')
        decimal(resume.sequence, 'payload.resume.sequence', isSequence)
      }
      if (has(payload, 'active_run_resume')) {
        if (payload.resource.type !== 'session_content') {
          fail('invalid_field', 'is only valid for session_content', 'payload.active_run_resume')
        }
        const active = object(payload.active_run_resume, 'payload.active_run_resume')
        requiredString(active, 'run_epoch', 'payload.active_run_resume.run_epoch')
        requiredString(active, 'run_id', 'payload.active_run_resume.run_id')
        decimal(active.run_cursor, 'payload.active_run_resume.run_cursor', isRunCursor)
      }
      return
    case 'subscribed':
      requiredString(payload, 'subscription_id', 'payload.subscription_id')
      validateResource(payload.resource, 'payload.resource')
      requiredString(payload, 'stream_epoch', 'payload.stream_epoch')
      decimal(payload.sequence, 'payload.sequence', isSequence)
      return
    case 'unsubscribe':
    case 'unsubscribed':
      requiredString(payload, 'subscription_id', 'payload.subscription_id')
      return
    case 'snapshot':
      validateSubscriptionMetadata(payload)
      revision(payload.resource_revision, 'payload.resource_revision')
      validateSnapshotContent(payload.content)
      return
    case 'change':
      validateSubscriptionMetadata(payload)
      decimal(payload.previous_sequence, 'payload.previous_sequence', isSequence)
      revision(payload.resource_revision, 'payload.resource_revision')
      validateOperations(payload.operations)
      return
    case 'subscription_event':
      requiredString(payload, 'subscription_id', 'payload.subscription_id')
      validateResource(payload.resource, 'payload.resource')
      const event = object(payload.event, 'payload.event')
      validateSubscriptionEvent(event)
      if (payload.resource.type === 'session_content' && event.session_id !== payload.resource.id) {
        fail('invalid_field', 'must match payload.resource.id', 'payload.event.session_id')
      }
      return
    case 'ack':
      requiredString(payload, 'subscription_id', 'payload.subscription_id')
      requiredString(payload, 'stream_epoch', 'payload.stream_epoch')
      decimal(payload.sequence, 'payload.sequence', isSequence)
      return
    case 'resync_required':
      requiredString(payload, 'subscription_id', 'payload.subscription_id')
      validateResource(payload.resource, 'payload.resource')
      requiredString(payload, 'reason', 'payload.reason')
      return
    case 'error':
      requiredString(payload, 'code', 'payload.code')
      requiredString(payload, 'message', 'payload.message')
      if (has(payload, 'request_id')) requiredString(payload, 'request_id', 'payload.request_id')
      return
    default:
      fail('unknown_type', `unknown message type ${JSON.stringify(type)}`, 'type')
  }
}

function validateSubscriptionMetadata(payload: RawObject): void {
  requiredString(payload, 'subscription_id', 'payload.subscription_id')
  validateResource(payload.resource, 'payload.resource')
  requiredString(payload, 'stream_epoch', 'payload.stream_epoch')
  decimal(payload.sequence, 'payload.sequence', isSequence)
}

function validateSnapshotContent(value: unknown): asserts value is SnapshotContent {
  const content = object(value, 'payload.content')
  const inlinePresent = has(content, 'inline')
  const blobPresent = has(content, 'blob')
  if (inlinePresent === blobPresent) {
    fail('invalid_field', 'must contain exactly one of inline or blob', 'payload.content')
  }
  if (inlinePresent) {
    object(content.inline, 'payload.content.inline')
    return
  }
  const blob = object(content.blob, 'payload.content.blob')
  requiredString(blob, 'id', 'payload.content.blob.id')
  requiredString(blob, 'url', 'payload.content.blob.url')
  requiredString(blob, 'content_type', 'payload.content.blob.content_type')
  nonNegativeInteger(blob.size, 'payload.content.blob.size')
  requiredString(blob, 'sha256', 'payload.content.blob.sha256')
  requiredString(blob, 'etag', 'payload.content.blob.etag')
  timestamp(blob.expires_at, 'payload.content.blob.expires_at')
}

function validateOperations(value: unknown): asserts value is ChangeOperation[] {
  const operations = requiredArrayValue(value, 'payload.operations')
  if (operations.length === 0) fail('invalid_field', 'must contain at least one operation', 'payload.operations')
  operations.forEach((value, index) => {
    const operation = object(value, `payload.operations[${index}]`)
    const field = `payload.operations[${index}]`
    requiredString(operation, 'op', `${field}.op`)
  })
}

function validateSubscriptionEvent(event: RawObject): void {
  const field = 'payload.event'
  const type = requiredString(event, 'type', `${field}.type`)
  const allowed = new Set(['type', 'session_id', 'run_id', 'run_cursor', 'turn_id', 'agent_iteration'])
  requiredString(event, 'session_id', `${field}.session_id`)
  requiredString(event, 'run_id', `${field}.run_id`)
  decimal(event.run_cursor, `${field}.run_cursor`, isRunCursor)
  if (has(event, 'turn_id')) requiredString(event, 'turn_id', `${field}.turn_id`)
  if (has(event, 'agent_iteration')) positiveInteger(event.agent_iteration, `${field}.agent_iteration`)
  const add = (...keys: string[]) => keys.forEach((key) => allowed.add(key))
  const requireDelta = () => {
    requiredString(event, 'turn_id', `${field}.turn_id`)
    positiveInteger(event.agent_iteration, `${field}.agent_iteration`)
    requiredString(event, 'item_id', `${field}.item_id`)
    requiredString(event, 'delta', `${field}.delta`)
    add('item_id', 'delta', 'durable_text_length', 'durable_checkpointed')
    if (has(event, 'durable_text_length')) nonNegativeInteger(event.durable_text_length, `${field}.durable_text_length`)
    if (has(event, 'durable_checkpointed') && typeof event.durable_checkpointed !== 'boolean') fail('invalid_field', 'must be boolean', `${field}.durable_checkpointed`)
  }
  const requireToolIdentity = () => {
    requiredString(event, 'turn_id', `${field}.turn_id`)
    positiveInteger(event.agent_iteration, `${field}.agent_iteration`)
    requiredString(event, 'tool_call_id', `${field}.tool_call_id`)
    requiredString(event, 'name', `${field}.name`)
    add('tool_call_id', 'name')
  }
  switch (type) {
    case 'text.delta':
    case 'reasoning.delta':
      requireDelta()
      break
    case 'tool.requested':
    case 'tool.running':
      requireToolIdentity(); add('arguments')
      if (has(event, 'arguments')) requiredString(event, 'arguments', `${field}.arguments`)
      break
    case 'tool.progress':
      requireToolIdentity(); add('arguments_delta'); requiredString(event, 'arguments_delta', `${field}.arguments_delta`)
      break
    case 'tool.finished':
      requireToolIdentity(); add('is_error', 'content')
      if (typeof event.is_error !== 'boolean') fail('invalid_field', 'must be boolean', `${field}.is_error`)
      if (has(event, 'content')) requiredString(event, 'content', `${field}.content`)
      break
    case 'run.prompt_queue': {
      add('prompts')
      const prompts = requiredArray(event, 'prompts', `${field}.prompts`)
      prompts.forEach((value, index) => {
        const prompt = object(value, `${field}.prompts[${index}]`)
        for (const key of Object.keys(prompt)) if (!['id', 'content', 'steer'].includes(key)) fail('invalid_field', `unknown field ${key}`, `${field}.prompts[${index}]`)
        requiredString(prompt, 'id', `${field}.prompts[${index}].id`)
        requiredString(prompt, 'content', `${field}.prompts[${index}].content`)
        if (typeof prompt.steer !== 'boolean') fail('invalid_field', 'must be boolean', `${field}.prompts[${index}].steer`)
      })
      break
    }
    case 'run.prompt_appended':
      add('prompts')
      requiredArray(event, 'prompts', `${field}.prompts`).forEach((value, index) => {
        if (typeof value !== 'string' || value.trim() === '') fail('invalid_field', 'must be a non-empty string', `${field}.prompts[${index}]`)
      })
      break
    case 'run.started':
      add('status')
      if (event.status !== 'running') fail('invalid_field', 'must be running', `${field}.status`)
      break
    case 'run.settled':
      add('status', 'durable_settlement_watermark')
      if (!['committed', 'failed', 'interrupted', 'cancelled'].includes(String(event.status))) fail('invalid_field', 'invalid run status', `${field}.status`)
      validateSettlementWatermark(event.durable_settlement_watermark, `${field}.durable_settlement_watermark`)
      if (compareRunCursor(String((event.durable_settlement_watermark as RawObject).run_cursor), String(event.run_cursor)) > 0) {
        fail('invalid_field', 'must not be after run.settled cursor', `${field}.durable_settlement_watermark.run_cursor`)
      }
      break
    default:
      fail('invalid_field', `unknown subscription event type ${JSON.stringify(type)}`, `${field}.type`)
  }
  for (const key of Object.keys(event)) if (!allowed.has(key)) fail('invalid_field', `unknown field ${key}`, `${field}.${key}`)
}

function validateSettlementWatermark(value: unknown, field: string): void {
  const watermark = object(value, field)
  for (const key of Object.keys(watermark)) if (!['resource_revision', 'run_cursor', 'verified', 'covered_items'].includes(key)) fail('invalid_field', `unknown field ${key}`, `${field}.${key}`)
  revision(watermark.resource_revision, `${field}.resource_revision`)
  decimal(watermark.run_cursor, `${field}.run_cursor`, isRunCursor)
  if (typeof watermark.verified !== 'boolean') fail('invalid_field', 'must be boolean', `${field}.verified`)
  const coveredItems = requiredArray(watermark, 'covered_items', `${field}.covered_items`)
  if (watermark.verified === false && coveredItems.length !== 0) fail('invalid_field', 'unverified watermark must not contain covered items', `${field}.covered_items`)
  coveredItems.forEach((value, index) => {
    const item = object(value, `${field}.covered_items[${index}]`)
    for (const key of Object.keys(item)) if (!['turn_id', 'agent_iteration', 'item_id', 'run_cursor'].includes(key)) fail('invalid_field', `unknown field ${key}`, `${field}.covered_items[${index}]`)
    requiredString(item, 'turn_id', `${field}.covered_items[${index}].turn_id`)
    positiveInteger(item.agent_iteration, `${field}.covered_items[${index}].agent_iteration`)
    requiredString(item, 'item_id', `${field}.covered_items[${index}].item_id`)
    decimal(item.run_cursor, `${field}.covered_items[${index}].run_cursor`, isRunCursor)
    if (compareRunCursor(String(item.run_cursor), String(watermark.run_cursor)) > 0) {
      fail('invalid_field', 'must not be after settlement run_cursor', `${field}.covered_items[${index}].run_cursor`)
    }
  })
}

function validateCommandError(value: RawObject, field: string): void {
  requiredString(value, 'code', `${field}.code`)
  requiredString(value, 'message', `${field}.message`)
}

function validateResource(value: unknown, field: string): asserts value is ResourceKey {
  const resource = object(value, field)
  const type = requiredString(resource, 'type', `${field}.type`)
  if (!resourceTypes.has(type)) fail('invalid_field', `unknown resource type ${JSON.stringify(type)}`, `${field}.type`)
  requiredString(resource, 'id', `${field}.id`)
}

function object(value: unknown, field: string): RawObject {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    fail('invalid_field', 'must be a JSON object', field)
  }
  return value as RawObject
}

function requiredArray(source: RawObject, key: string, field: string): unknown[] {
  return requiredArrayValue(source[key], field)
}

function requiredArrayValue(value: unknown, field: string): unknown[] {
  if (!Array.isArray(value)) fail('invalid_field', 'must be an array', field)
  return value
}

function requiredString(source: RawObject, key: string, field: string): string {
  const value = source[key]
  if (typeof value !== 'string' || value.trim() === '') fail('invalid_field', 'must be a non-empty string', field)
  return value
}

function timestamp(value: unknown, field: string): void {
  if (!isRFC3339Timestamp(value)) fail('invalid_field', 'must be an RFC3339 timestamp', field)
}

function decimal(value: unknown, field: string, predicate: (value: unknown) => boolean): void {
  if (!predicate(value)) fail('invalid_field', 'must be a non-negative decimal string', field)
}

function revision(value: unknown, field: string): void {
  if (typeof value !== 'string' || value.trim() === '') fail('invalid_field', 'must be a non-empty string', field)
}

function positiveInteger(value: unknown, field: string): void {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value <= 0) {
    fail('invalid_field', 'must be a positive integer', field)
  }
}

function nonNegativeInteger(value: unknown, field: string): void {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    fail('invalid_field', 'must be a non-negative integer', field)
  }
}

function has(source: RawObject, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(source, key)
}

function fail(code: 'invalid_field' | 'unsupported_version' | 'unknown_type', message: string, field?: string): never {
  throw new ProtocolDecodeError(code, message, field)
}

