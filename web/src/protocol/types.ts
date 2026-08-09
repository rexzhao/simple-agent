import type { JsonObject, JsonValue } from '../domain/json'
export type { JsonObject, JsonPrimitive, JsonValue } from '../domain/json'

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
  | 'debug_register'
  | 'debug_registered'
  | 'debug_focus'
  | 'debug_focused'
  | 'debug_unregister'
  | 'debug_unregistered'
  | 'debug_execute'
  | 'debug_execution_result'
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

export interface DebugExecutorPayload {
  page_id: string
  page_epoch: string
  session_id: string
  focused: boolean
}

export type DebugRegisterPayload = DebugExecutorPayload
export type DebugRegisteredPayload = DebugExecutorPayload
export type DebugFocusPayload = DebugExecutorPayload
export type DebugFocusedPayload = DebugExecutorPayload
export type DebugUnregisterPayload = DebugExecutorPayload
export type DebugUnregisteredPayload = DebugExecutorPayload

export interface DebugExecutionPayload {
  execution_id: string
  page_id: string
  page_epoch: string
  session_id: string
  code: string
  timeout_ms: number
}

export type DebugExecutionStatus = 'succeeded' | 'failed'
export type DebugConsoleLevel = 'log' | 'info' | 'warn' | 'error' | 'debug'

export interface DebugConsoleEntry {
  level: DebugConsoleLevel
  arguments: JsonValue[]
}

export interface DebugExecutionError {
  code: string
  message: string
  details?: JsonValue
}

interface DebugExecutionResultIdentity {
  execution_id: string
  page_id: string
  page_epoch: string
  session_id: string
}

export type DebugExecutionResultPayload =
  | (DebugExecutionResultIdentity & {
      status: 'succeeded'
      value: JsonValue
      console?: DebugConsoleEntry[]
      error?: never
    })
  | (DebugExecutionResultIdentity & {
      status: 'failed'
      value?: never
      console?: DebugConsoleEntry[]
      error: DebugExecutionError
    })

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

export interface RunResumeToken {
  run_epoch: string
  run_id: string
  run_cursor: RunCursor
}

export interface SubscribePayload {
  subscription_id: string
  resource: ResourceKey
  resume?: ResumeToken
  active_run_resume?: RunResumeToken
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

// Durable session-content schema (D1). These types describe the strict
// resource adapter payload while ChangeOperation remains open for other
// resources at the protocol boundary.
export interface SessionContentSnapshot {
  schema_version: 1
  session: SessionContentMetadata
  history: SessionContentHistoryWindow
  active_run: SessionContentActiveRun | null
  compaction: SessionContentCompaction
}

export type SessionContentSessionStatus = 'idle' | 'running' | 'failed' | 'interrupted'
export type SessionContentCreatedBy = 'user' | 'agent'
export type SessionContentItemKind = 'message' | 'compaction' | 'runtime_context'
export type SessionContentVisibility = 'visible' | 'hidden' | 'debug'
export type SessionContentAudience = 'user' | 'model' | 'internal'
export type SessionContentItemStatus = 'pending' | 'completed' | 'error' | 'interrupted'
export type SessionContentRunStatus = 'running' | 'committed' | 'failed' | 'interrupted' | 'cancelled'
export type SessionContentRole = 'system' | 'developer' | 'user' | 'assistant' | 'tool' | 'provider'

export interface SessionContentMetadata {
  id: string
  version: number
  created_at: string
  updated_at: string
  display_name?: string
  created_by?: SessionContentCreatedBy
  parent_session_id?: string
  root_session_id?: string
  spawn_depth?: number
  archived: boolean
  archived_at?: string
  last_used_at: string
  current_run_id?: string
  running_run_id?: string
  running_turn_id?: string
  interrupted_run_id?: string
  interrupted_turn_id?: string
  interrupted_at?: string
  latest_run_id?: string
  last_run_id?: string
  last_run_status?: SessionContentRunStatus
  has_unread_result: boolean
  provider?: string
  model_profile?: string
  model_id?: string
  pricing?: JsonObject
  reasoning_level?: string
  model_parameters?: JsonObject
  status: SessionContentSessionStatus
  project_id?: string
  cwd?: string
  created_cwd?: string
  config_path?: string
  config_dir?: string
  enabled_tools?: string[]
  enabled_mcp?: string[]
  enabled_skills?: string[]
  show_reasoning: boolean
  full_access: boolean
  debug: { request_bodies: boolean }
  context: JsonObject
  save_tool_results: boolean
  active_history?: string[]
}

export interface SessionContentHistoryWindow {
  items: SessionContentItem[]
  descriptor: SessionContentHistoryDescriptor
}

export interface SessionContentHistoryDescriptor {
  limit: number
  before_item_seq?: string
  after_item_seq?: string
  align_turn: boolean
  visible_only: boolean
  oldest_item_seq?: string
  newest_item_seq?: string
  has_more_before: boolean
  has_more_after: boolean
}

export interface SessionContentItemKey {
  turn_id: string
  agent_iteration: number
  item_id: string
}

export interface SessionContentItem {
  key: SessionContentItemKey
  seq: number
  created_at: string
  kind: SessionContentItemKind
  visibility: SessionContentVisibility
  audience: SessionContentAudience
  status?: SessionContentItemStatus
  message?: SessionContentMessage
}

export interface SessionContentMessage {
  role: SessionContentRole
  content?: SessionContentText
  reasoning?: SessionContentText
  images?: { hash: string; media_type: string; size_bytes: number }[]
  tool_call_id?: string
  tool_calls?: { id: string; name: string; arguments?: SessionContentText }[]
  is_error?: boolean
}

export interface SessionContentText {
  inline?: string
  preview?: string
  blob?: BlobDescriptor
  /** Optional only for legacy inline records; D1 projections always set it. */
  content_type?: 'text/plain' | 'text/plain; charset=utf-8' | 'application/json'
  truncated?: boolean
}

export interface SessionContentActiveRun {
  run_id: string
  session_id: string
  turn_id?: string
  started_at: string
  status: 'running'
  recoverable: boolean
  run_epoch?: string
  run_cursor?: RunCursor
  replay_available: boolean
  replay_from_cursor?: RunCursor
  replay_to_cursor?: RunCursor
  recovery_required: boolean
  durable_settlement_watermark?: DurableSettlementWatermark
}

export interface TransientItemWatermark {
  turn_id: string
  agent_iteration: number
  item_id: string
  run_cursor: RunCursor
}

export interface DurableSettlementWatermark {
  resource_revision: ResourceRevision
  run_cursor: RunCursor
  verified: boolean
  covered_items: TransientItemWatermark[]
}

export interface SessionContentCompaction {
  checkpoints: SessionContentCompactionCheckpoint[]
  truncated: boolean
}

export interface SessionContentCompactionCheckpoint {
  id: string
  created_at: string
  reason: string
  phase: string
  trigger: string
  summary_item_id: string
  from_item_id?: string
  to_item_id?: string
  previous_active_history?: string[]
  replacement_history: string[]
  summary_provider?: string
  summary_model?: string
}

export type SessionContentOperation =
  | { op: 'metadata.replace'; metadata: SessionContentMetadata }
  | { op: 'item.upsert'; item: SessionContentItem }
  | { op: 'item.remove'; key: SessionContentItemKey }
  | { op: 'history.window.replace'; window: SessionContentHistoryWindow }
  | { op: 'history.window.descriptor.replace'; descriptor: SessionContentHistoryDescriptor }
  | { op: 'active_run.replace'; active_run: SessionContentActiveRun }
  | { op: 'active_run.clear' }
  | { op: 'compaction.replace'; compaction: SessionContentCompaction }

export interface ChangePayload {
  subscription_id: string
  resource: ResourceKey
  stream_epoch: string
  sequence: Sequence
  previous_sequence: Sequence
  resource_revision: ResourceRevision
  operations: ChangeOperation[]
}

export interface SubscriptionEventBase {
  session_id: string
  run_id: string
  run_cursor: RunCursor
  turn_id?: string
  agent_iteration?: number
}

export type SubscriptionEventData =
  | (SubscriptionEventBase & { type: 'text.delta' | 'reasoning.delta'; item_id: string; delta: string; durable_text_length?: number; durable_checkpointed?: boolean })
  | (SubscriptionEventBase & { type: 'tool.requested' | 'tool.running'; tool_call_id: string; name: string; arguments?: string })
  | (SubscriptionEventBase & { type: 'tool.progress'; tool_call_id: string; name: string; arguments_delta: string })
  | (SubscriptionEventBase & { type: 'tool.finished'; tool_call_id: string; name: string; is_error: boolean; content?: string })
  | (SubscriptionEventBase & { type: 'run.prompt_queue'; turn_id?: string; prompts: { id: string; content: string; steer: boolean }[] })
  | (SubscriptionEventBase & { type: 'run.prompt_appended'; turn_id?: string; prompts: string[] })
  | (SubscriptionEventBase & { type: 'run.started'; status: 'running' })
  | (SubscriptionEventBase & { type: 'turn.failed'; turn_id: string; code: string; message: string })
  | (SubscriptionEventBase & { type: 'run.settled'; status: 'committed' | 'failed' | 'interrupted' | 'cancelled'; durable_settlement_watermark: DurableSettlementWatermark })

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
export type DebugRegisterMessage = ProtocolEnvelope<'debug_register', DebugRegisterPayload>
export type DebugRegisteredMessage = ProtocolEnvelope<'debug_registered', DebugRegisteredPayload>
export type DebugFocusMessage = ProtocolEnvelope<'debug_focus', DebugFocusPayload>
export type DebugFocusedMessage = ProtocolEnvelope<'debug_focused', DebugFocusedPayload>
export type DebugUnregisterMessage = ProtocolEnvelope<'debug_unregister', DebugUnregisterPayload>
export type DebugUnregisteredMessage = ProtocolEnvelope<'debug_unregistered', DebugUnregisteredPayload>
export type DebugExecuteMessage = ProtocolEnvelope<'debug_execute', DebugExecutionPayload>
export type DebugExecutionResultMessage = ProtocolEnvelope<'debug_execution_result', DebugExecutionResultPayload>
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
  | DebugRegisterMessage
  | DebugRegisteredMessage
  | DebugFocusMessage
  | DebugFocusedMessage
  | DebugUnregisterMessage
  | DebugUnregisteredMessage
  | DebugExecuteMessage
  | DebugExecutionResultMessage
  | ErrorMessage

// These brands keep the three decimal concepts distinct at compile time even
// though they deliberately share the same decimal string wire representation.
declare const sequenceBrand: unique symbol
declare const resourceRevisionBrand: unique symbol
declare const runCursorBrand: unique symbol

export type Sequence = string & { readonly [sequenceBrand]: 'sequence' }
export type ResourceRevision = string & { readonly [resourceRevisionBrand]: 'resource_revision' }
export type RunCursor = string & { readonly [runCursorBrand]: 'run_cursor' }
