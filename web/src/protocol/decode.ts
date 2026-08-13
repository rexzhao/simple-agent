import { isRFC3339Timestamp } from './datetime'
import { ProtocolDecodeError } from './errors'
import { isBoundedDebugIdentity, isWellFormedString } from './identifiers'
import { compareRunCursor, isRunCursor, isSequence } from './sequence'
import { unicodeCodePointLength } from './strings'
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
  'debug_register',
  'debug_registered',
  'debug_focus',
  'debug_focused',
  'debug_unregister',
  'debug_unregistered',
  'debug_execute',
  'debug_execution_result',
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
    case 'debug_register':
    case 'debug_registered':
    case 'debug_focus':
    case 'debug_focused':
    case 'debug_unregister':
    case 'debug_unregistered':
      validateDebugExecutorPayload(payload)
      return
    case 'debug_execute':
      validateDebugExecutionPayload(payload)
      return
    case 'debug_execution_result':
      validateDebugExecutionResultPayload(payload)
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

function validateDebugExecutorPayload(payload: RawObject): void {
  for (const key of ['page_id', 'page_epoch', 'session_id']) {
    if (!isBoundedDebugIdentity(payload[key])) {
      fail(
        'invalid_field',
        'must be a bounded non-empty identity without edge whitespace or control characters',
        `payload.${key}`,
      )
    }
  }
  if (!has(payload, 'focused') || typeof payload.focused !== 'boolean') {
    fail('invalid_field', 'must be a boolean', 'payload.focused')
  }
}

function validateDebugExecutionPayload(payload: RawObject): void {
  validateDebugExecutionIdentity(payload)
  const code = payload.code
  if (typeof code !== 'string' || code.length === 0 || !isWellFormedString(code)) {
    fail('invalid_field', 'must be a non-empty well-formed string', 'payload.code')
  }
  if (new TextEncoder().encode(code).byteLength > 64 * 1024) {
    fail('invalid_field', 'exceeds the maximum execution code length', 'payload.code')
  }
  boundedTimeout(payload.timeout_ms, 'payload.timeout_ms')
}

function validateDebugExecutionResultPayload(payload: RawObject): void {
  validateDebugExecutionIdentity(payload)
  const status = requiredString(payload, 'status', 'payload.status')
  if (status !== 'succeeded' && status !== 'failed') {
    fail('invalid_field', 'must be succeeded or failed', 'payload.status')
  }
  if (status === 'succeeded' && !has(payload, 'value')) {
    fail('invalid_field', 'is required for a succeeded execution', 'payload.value')
  }
  if (status === 'failed') {
    const error = object(payload.error, 'payload.error')
    requiredString(error, 'code', 'payload.error.code')
    requiredString(error, 'message', 'payload.error.message')
    if (has(payload, 'value')) {
      fail('invalid_field', 'must be omitted for a failed execution', 'payload.value')
    }
  } else if (has(payload, 'error')) {
    fail('invalid_field', 'must be omitted for a succeeded execution', 'payload.error')
  }
  if (has(payload, 'console')) {
    const consoleEntries = requiredArray(payload, 'console', 'payload.console')
    if (consoleEntries.length > 128) fail('invalid_field', 'contains too many console entries', 'payload.console')
    consoleEntries.forEach((value, index) => {
      const entry = object(value, `payload.console[${index}]`)
      const level = requiredString(entry, 'level', `payload.console[${index}].level`)
      if (!['log', 'info', 'warn', 'error', 'debug'].includes(level)) {
        fail('invalid_field', 'must be log, info, warn, error, or debug', `payload.console[${index}].level`)
      }
      const args = requiredArray(entry, 'arguments', `payload.console[${index}].arguments`)
      if (args.length > 32) fail('invalid_field', 'contains too many arguments', `payload.console[${index}].arguments`)
    })
  }
  if (jsonByteLength(payload) > 64 * 1024) {
    fail('invalid_field', 'execution result exceeds the inline result budget', 'payload')
  }
}

function validateDebugExecutionIdentity(payload: RawObject): void {
  for (const key of ['execution_id', 'page_id', 'page_epoch', 'session_id']) {
    if (!isBoundedDebugIdentity(payload[key])) {
      fail('invalid_field', 'must be a bounded non-empty identity without edge whitespace or control characters', `payload.${key}`)
    }
  }
}

function boundedTimeout(value: unknown, field: string): void {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 100 || value > 30_000) {
    fail('invalid_field', 'must be between 100 and 30000 milliseconds', field)
  }
}

function jsonByteLength(value: unknown): number {
  const encoded = JSON.stringify(value)
  return typeof encoded === 'string' ? new TextEncoder().encode(encoded).byteLength : Number.POSITIVE_INFINITY
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
  const requireMessageIdentity = () => {
    requiredString(event, 'turn_id', `${field}.turn_id`)
    positiveInteger(event.agent_iteration, `${field}.agent_iteration`)
    requiredString(event, 'item_id', `${field}.item_id`)
	if (typeof event.message_revision !== 'string' || !/^(0|[1-9][0-9]*)$/u.test(event.message_revision) || BigInt(event.message_revision) > 18446744073709551615n) fail('invalid_field', 'must be a canonical uint64 decimal', `${field}.message_revision`)
    add('item_id', 'message_revision')
  }
  const requireToolIdentity = () => {
    requiredString(event, 'turn_id', `${field}.turn_id`)
    positiveInteger(event.agent_iteration, `${field}.agent_iteration`)
    requiredString(event, 'tool_call_id', `${field}.tool_call_id`)
    requiredString(event, 'name', `${field}.name`)
    add('tool_call_id', 'name')
  }
  switch (type) {
    case 'assistant.message.started':
      requireMessageIdentity()
	  if (event.message_revision !== '0') fail('invalid_field', 'must be zero', `${field}.message_revision`)
      break
    case 'assistant.message.updated':
    case 'assistant.message.completed':
    case 'assistant.message.failed':
      requireMessageIdentity(); add('content', 'reasoning', 'tool_calls', 'snapshot_omitted')
	  if (event.message_revision === '0') fail('invalid_field', 'must be positive', `${field}.message_revision`)
	  if (type === 'assistant.message.updated' && has(event, 'snapshot_omitted')) fail('invalid_field', 'is only valid for terminal messages', `${field}.snapshot_omitted`)
      if (has(event, 'snapshot_omitted') && event.snapshot_omitted !== true) fail('invalid_field', 'must be true', `${field}.snapshot_omitted`)
      if (event.snapshot_omitted !== true && typeof event.content !== 'string') fail('invalid_field', 'must be a string', `${field}.content`)
      if (event.snapshot_omitted === true && (has(event, 'content') || has(event, 'reasoning') || has(event, 'tool_calls'))) fail('invalid_field', 'omitted snapshot cannot carry message fields', field)
      if (has(event, 'reasoning') && typeof event.reasoning !== 'string') fail('invalid_field', 'must be a string', `${field}.reasoning`)
      if (has(event, 'tool_calls')) {
        requiredArray(event, 'tool_calls', `${field}.tool_calls`).forEach((value, index) => {
          const call = object(value, `${field}.tool_calls[${index}]`)
          for (const key of Object.keys(call)) if (!['id', 'name', 'arguments'].includes(key)) fail('invalid_field', `unknown field ${key}`, `${field}.tool_calls[${index}]`)
          requiredString(call, 'id', `${field}.tool_calls[${index}].id`)
          requiredString(call, 'name', `${field}.tool_calls[${index}].name`)
          if (has(call, 'arguments') && typeof call.arguments !== 'string') fail('invalid_field', 'must be a string', `${field}.tool_calls[${index}].arguments`)
        })
      }
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
    case 'turn.failed':
      requiredString(event, 'turn_id', `${field}.turn_id`)
      requiredString(event, 'code', `${field}.code`)
      requiredString(event, 'message', `${field}.message`)
      if (unicodeCodePointLength(String(event.message)) > 600) fail('invalid_field', 'message is too long', `${field}.message`)
      add('code', 'message')
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
