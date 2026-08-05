export type JsonPrimitive = string | number | boolean | null
export type JsonValue = JsonPrimitive | JsonObject | JsonValue[]
export type JsonObject = { [key: string]: JsonValue }

export type MessageType =
  | 'hello'
  | 'welcome'
  | 'ping'
  | 'pong'
  | 'command'
  | 'command_accepted'
  | 'command_result'
  | 'subscribe'
  | 'subscribed'
  | 'unsubscribe'
  | 'unsubscribed'
  | 'snapshot'
  | 'change'
  | 'subscription_event'
  | 'ack'
  | 'resync_required'
  | 'error'

export type ResourceType =
  | 'project_index'
  | 'session_index'
  | 'session_content'
  | 'provider_settings'
  | 'model_catalog'
  | 'codex_login'

export interface ResourceKey {
  type: ResourceType
  id: string
}

export interface HelloPayload {
  supported_versions: number[]
  client_id: string
}

export interface WelcomePayload {
  selected_version: 1
  connection_id: string
  server_epoch: string
  heartbeat_interval_ms: number
  max_message_bytes: number
}

export interface CommandPayload {
  name: string
  schema_version: number
  request_id: string
  expected_revision?: ResourceRevision
  arguments: JsonObject
}

export interface CommandAcceptedPayload {
  request_id: string
}

export type CommandStatus = 'succeeded' | 'failed'

export interface CommandError {
  code: string
  message: string
  details?: JsonValue
}

export interface CommandResultPayload {
  request_id: string
  status: CommandStatus
  result?: JsonValue
  error?: CommandError
}

export interface ResumeToken {
  stream_epoch: string
  sequence: Sequence
}

export interface SubscribePayload {
  subscription_id: string
  resource: ResourceKey
  resume?: ResumeToken
}

export interface SubscribedPayload {
  subscription_id: string
  resource: ResourceKey
  stream_epoch: string
  sequence: Sequence
}

export interface UnsubscribePayload {
  subscription_id: string
}

export interface UnsubscribedPayload {
  subscription_id: string
}

export interface BlobDescriptor {
  id: string
  url: string
  content_type: string
  size: number
  sha256: string
  etag: string
  expires_at: string
}

export type SnapshotContent =
  | { inline: JsonObject }
  | { blob: BlobDescriptor }

export interface SnapshotPayload {
  subscription_id: string
  resource: ResourceKey
  stream_epoch: string
  sequence: Sequence
  resource_revision: ResourceRevision
  content: SnapshotContent
}

// Resource-specific operations stay open at this boundary. Resource adapters
// own the operation-specific fields and schemas.
export type ChangeOperation = JsonObject & { op: string }

export interface ChangePayload {
  subscription_id: string
  resource: ResourceKey
  stream_epoch: string
  sequence: Sequence
  previous_sequence: Sequence
  resource_revision: ResourceRevision
  operations: ChangeOperation[]
}

// Transient event fields are resource-specific. Only the discriminating type
// is part of the protocol contract; adapters validate the remaining fields.
export type SubscriptionEventData = JsonObject & { type: string }

export interface SubscriptionEventPayload {
  subscription_id: string
  resource: ResourceKey
  event: SubscriptionEventData
}

export interface AckPayload {
  subscription_id: string
  stream_epoch: string
  sequence: Sequence
}

export interface ResyncRequiredPayload {
  subscription_id: string
  resource: ResourceKey
  reason: string
}

export interface ErrorPayload {
  code: string
  message: string
  request_id?: string
  details?: JsonValue
}

export type ProtocolEnvelope<T extends MessageType, P> = {
  version: 1
  type: T
  id: string
  timestamp?: string
  trace_id?: string
  payload: P
}

export type HelloMessage = ProtocolEnvelope<'hello', HelloPayload>
export type WelcomeMessage = ProtocolEnvelope<'welcome', WelcomePayload>
export type PingMessage = ProtocolEnvelope<'ping', Record<string, never>>
export type PongMessage = ProtocolEnvelope<'pong', Record<string, never>>
export type CommandMessage = ProtocolEnvelope<'command', CommandPayload>
export type CommandAcceptedMessage = ProtocolEnvelope<'command_accepted', CommandAcceptedPayload>
export type CommandResultMessage = ProtocolEnvelope<'command_result', CommandResultPayload>
export type SubscribeMessage = ProtocolEnvelope<'subscribe', SubscribePayload>
export type SubscribedMessage = ProtocolEnvelope<'subscribed', SubscribedPayload>
export type UnsubscribeMessage = ProtocolEnvelope<'unsubscribe', UnsubscribePayload>
export type UnsubscribedMessage = ProtocolEnvelope<'unsubscribed', UnsubscribedPayload>
export type SnapshotMessage = ProtocolEnvelope<'snapshot', SnapshotPayload>
export type ChangeMessage = ProtocolEnvelope<'change', ChangePayload>
export type SubscriptionEventMessage = ProtocolEnvelope<'subscription_event', SubscriptionEventPayload>
export type AckMessage = ProtocolEnvelope<'ack', AckPayload>
export type ResyncRequiredMessage = ProtocolEnvelope<'resync_required', ResyncRequiredPayload>
export type ErrorMessage = ProtocolEnvelope<'error', ErrorPayload>

export type ProtocolMessage =
  | HelloMessage
  | WelcomeMessage
  | PingMessage
  | PongMessage
  | CommandMessage
  | CommandAcceptedMessage
  | CommandResultMessage
  | SubscribeMessage
  | SubscribedMessage
  | UnsubscribeMessage
  | UnsubscribedMessage
  | SnapshotMessage
  | ChangeMessage
  | SubscriptionEventMessage
  | AckMessage
  | ResyncRequiredMessage
  | ErrorMessage

// These brands keep the three decimal concepts distinct at compile time even
// though they deliberately share the same decimal string wire representation.
declare const sequenceBrand: unique symbol
declare const resourceRevisionBrand: unique symbol
declare const runCursorBrand: unique symbol

export type Sequence = string & { readonly [sequenceBrand]: 'sequence' }
export type ResourceRevision = string & { readonly [resourceRevisionBrand]: 'resource_revision' }
export type RunCursor = string & { readonly [runCursorBrand]: 'run_cursor' }
