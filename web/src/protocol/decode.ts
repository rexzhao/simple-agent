import { isRFC3339Timestamp } from './datetime'
import { ProtocolDecodeError } from './errors'
import { isSequence } from './sequence'
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
      requiredString(event, 'type', 'payload.event.type')
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

