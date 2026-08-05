export interface Bootstrap {
  version: string
  cwd: string
  server_root: string
  config_path: string
}

export interface Project {
  id: string
  root: string
  display_name: string
  archived: boolean
  created_at: string
  updated_at: string
}

export interface SessionModelOption {
  provider: string
  model_profile: string
  model_id: string
  reasoning_levels?: string[]
  default_reasoning_level?: string
}

export interface SessionModelOptions {
  models: SessionModelOption[]
  default_provider: string
  default_model: string
}

export interface ReasoningConfig {
  parameter?: string
  default?: string
  levels?: Record<string, unknown>
}

/** Prices are currency units per one million tokens. */
export interface ModelPricing {
  input_cache_hit: number
  input_cache_miss: number
  cache_write?: number
  output: number
  currency?: string
  long_context_threshold?: number
  long_context?: ModelPricingTier
}

export interface ModelPricingTier {
  input_cache_hit: number
  input_cache_miss: number
  cache_write: number
  output: number
}

export interface ProviderModelSettings {
  profile: string
  id: string
  type: string
  compatibility?: string
  input?: string[]
  developer_role?: string
  context_window?: number
  input_limit?: number
  output_limit?: number
  parameters?: Record<string, unknown>
  reasoning_config?: ReasoningConfig
  pricing?: ModelPricing
}

export interface CodexAuthStatus {
  status: 'signed_out' | 'pending' | 'signed_in' | 'expired' | 'error' | string
  account_id?: string
  expires_at?: string
  refreshable?: boolean
  message?: string
  login_id?: string
  user_code?: string
  verification_url?: string
}

export interface ProviderSettings {
  name: string
  base_url: string
  api_key?: string
  api_key_configured: boolean
  auth_file?: string
  request_timeout?: string
  http_proxy?: string
  https_proxy?: string
  max_concurrent_requests?: number
  models: ProviderModelSettings[]
  codex_auth?: CodexAuthStatus
}

export interface ProviderSettingsInput {
  name: string
  base_url: string
  api_key?: string
  keep_api_key?: boolean
  auth_file?: string
  request_timeout?: string
  http_proxy?: string
  https_proxy?: string
  max_concurrent_requests?: number
  models: ProviderModelSettings[]
}

export interface ProviderSettingsDocument {
  server_root: string
  config_path: string
  default_provider: string
  default_model: string
  providers: ProviderSettings[]
}

export interface ContextMetadata {
  context_window: number
  context_window_source: string
  warning_threshold_percent: number
  last_request_tokens?: number
  last_input_tokens?: number
  last_output_tokens?: number
  last_total_tokens?: number
  last_usage_count_tokens?: number
  last_cached_tokens?: number
  last_cache_write_tokens?: number
  last_reasoning_tokens?: number
  last_usage_source?: string
  total_input_tokens?: number
  total_output_tokens?: number
  total_tokens?: number
  total_requests?: number
  total_cached_tokens?: number
  total_cache_write_tokens?: number
  total_reasoning_tokens?: number
  total_short_input_tokens?: number
  total_short_output_tokens?: number
  total_short_cached_tokens?: number
  total_short_cache_write_tokens?: number
  total_long_input_tokens?: number
  total_long_output_tokens?: number
  total_long_cached_tokens?: number
  total_long_cache_write_tokens?: number
  warning_issued?: boolean
}

export interface SessionDebugSettings {
  request_bodies: boolean
}

export interface Session {
  id: string
  created_at: string
  updated_at: string
  display_name: string
  created_by: 'user' | 'agent' | string
  parent_session_id?: string
  root_session_id: string
  spawn_depth: number
  archived: boolean
  last_used_at: string
  current_run_id?: string
  running_run_id?: string
  running_turn_id?: string
  interrupted_run_id?: string
  interrupted_turn_id?: string
  latest_run_id?: string
  last_run_id?: string
  last_run_status?: string
  provider: string
  model_profile: string
  model_id: string
  pricing?: ModelPricing
  reasoning_level?: string
  project_id: string
  created_cwd: string
  last_seq: number
  /** Precision-safe decimal form of the session last_seq watermark. */
  revision?: string
  status?: string
  show_reasoning?: boolean
  full_access: boolean
  debug?: SessionDebugSettings
  config_path?: string
  context?: ContextMetadata
}

export interface ActiveRunDescriptor {
  run_id: string
  session_id: string
  turn_id?: string
  started_at: string
  status: string
}

/** HTTP 202 response after the coordinator has admitted a run. */
export interface RunAdmission {
  run_id: string
  session_id: string
  status: string
}

export type LifecycleEventType =
  | 'session.created'
  | 'session.updated'
  | 'session.archived'
  | 'session.deleted'
  | 'run.started'
  | 'run.settled'

/** Payload emitted by the process-wide /api/events SSE stream. */
export interface LifecycleEvent {
  type: LifecycleEventType
  session?: Session | string
  session_id?: string
  project_id?: string
  project?: string
  descendants?: string[]
  reason?: string
  run?: string
  run_id?: string
  status?: string
  last_seq?: number
  committed_revision?: string
  turn_id?: string
  metadata?: Session
  session_metadata?: Session
  message?: string
}

export interface MessageContent {
  inline?: string
  preview?: string
}

export interface ImageAttachmentInput {
  data_url: string
  detail?: 'auto' | 'low' | 'high'
}

export interface SessionImageAttachment {
  hash: string
  media_type: string
  size_bytes: number
}

export interface SessionItem {
  seq: number
  id: string
  turn_id?: string
	agent_iteration?: number
  created_at: string
  kind: string
  visibility: string
  audience: string
  status?: string
  message?: {
    role: 'user' | 'assistant' | string
    content?: MessageContent
    /** Durable assistant reasoning, present only when show_reasoning is enabled. */
    reasoning?: string
    images?: SessionImageAttachment[]
		tool_call_id?: string
		tool_calls?: SessionToolCall[]
		is_error?: boolean
  }
}

export interface SessionToolCall {
	id: string
	name: string
	arguments?: string
}

export interface ItemsPage {
  items: SessionItem[]
  oldest_seq: number
  newest_seq: number
  has_more_before: boolean
  has_more_after: boolean
}

export interface SessionSnapshot {
  session_id: string
  revision: string
  session: Session
  history: ItemsPage
}

export interface SessionItemProjectionEvent {
  type: 'item.appended' | 'item.created' | 'item.updated'
  session_id: string
  run_id?: string
  turn_id?: string
  /** Durable record sequence that caused this notification. */
  seq: number
  /** Precision-safe session watermark after the committed transaction. */
  revision: string
  item_id: string
  item: SessionItem
  /** Present for assistant items; the full committed text length in runes. */
  assistant_text_length?: number
}

export type RunEvent =
  | { type: 'run.started'; run_id?: string; session_id?: string; turn_id?: string; status?: string }
  | { type: 'turn.started'; turn_id: string }
  | { type: 'compaction.started'; turn_id: string; trigger: 'auto' | 'manual' }
  | { type: 'compaction.completed'; turn_id: string; trigger: 'auto' | 'manual'; compaction_id: string; active_context_tokens?: number; context_window?: number }
  | { type: 'provider.retrying'; turn_id: string; agent_iteration: number; attempt: number; max_attempts: number; delay_ms: number; reason: string }
	| { type: 'agent.iteration.started'; turn_id: string; agent_iteration: number }
	| { type: 'text.delta'; turn_id: string; agent_iteration: number; text: string; item_id?: string; durable_text_length?: number; durable_checkpointed?: boolean }
	| { type: 'reasoning.delta'; turn_id: string; agent_iteration: number; text: string; item_id?: string }
	| { type: 'tool.requested' | 'tool.started'; turn_id: string; agent_iteration: number; tool_call_id: string; name: string; arguments?: string }
	| { type: 'tool.finished'; turn_id: string; agent_iteration: number; tool_call_id: string; name: string; is_error: boolean; content?: string }
	| { type: 'usage.updated'; turn_id: string; agent_iteration: number; input_tokens: number; output_tokens: number; total_tokens: number; cached_tokens: number; cache_write_tokens: number; reasoning_tokens: number }
	| SessionItemProjectionEvent
  | { type: 'run.prompt_queue'; turn_id?: string; prompts?: QueuedPrompt[] }
  | { type: 'run.prompt_appended'; turn_id?: string; prompts?: string[] }
  | { type: 'run.resync_required'; run_id: string; session_id: string; oldest_seq: number; oldest_stream_event_id?: number; required_revision?: string }
  | { type: 'turn.committed'; turn_id: string; last_seq: number }
  | { type: 'turn.failed'; turn_id: string; code: string; message: string }
  | { type: 'run.settled'; run_id: string; status: string; turn_id?: string; last_seq?: number; committed_revision?: string; message?: string }
  | { type: string; [key: string]: unknown }

export interface ToolActivity {
	kind: 'tool'
  id: string
	name: string
	iteration: number
	arguments?: string
	result?: string
  status: 'requested' | 'running' | 'finished' | 'error'
}

export interface ReasoningActivity {
	kind: 'reasoning'
	id: string
	text: string
	iteration: number
	/** Explicit identity of the assistant request that produced this step. */
	turnID?: string
	itemID?: string
	label?: string
}

export interface ModelOutputActivity {
	kind: 'output'
	id: string
	text: string
	iteration: number
}

export type RunStep = ReasoningActivity | ModelOutputActivity | ToolActivity

export interface QueuedPrompt {
  id: string
  content: string
  steer?: boolean
}

export interface ActiveRun {
  id: string
	sessionID: string
	turnID?: string
  restored?: boolean
	queuedPrompts?: QueuedPrompt[]
	/** Transient process boundaries for drained prompts; never a user item. */
	processBoundaries?: Array<{ id: string; stepIndex: number }>
	assistantText: string
	/** Explicit transient-to-durable assistant identity, keyed by turn/iteration. */
	assistantItems?: Record<string, { itemID: string; durableTextLength: number }>
	steps: RunStep[]
  agentIteration: number
  inputTokens?: number
  outputTokens?: number
  totalTokens?: number
	cachedTokens?: number
	cacheWriteTokens?: number
  reasoningTokens?: number
  /** Additive usage for every provider request in this turn. */
  usage?: {
    inputTokens: number
    outputTokens: number
    totalTokens: number
    cachedTokens: number
    cacheWriteTokens: number
    reasoningTokens: number
  }
  /** Individual provider usages are retained so short/long pricing can be selected per request. */
  usageEvents?: Array<{
    agentIteration: number
    inputTokens: number
    outputTokens: number
    totalTokens: number
    cachedTokens: number
    cacheWriteTokens: number
    reasoningTokens: number
  }>
  compaction?: {
    trigger: 'auto' | 'manual'
    status: 'running' | 'completed'
    activeContextTokens?: number
    contextWindow?: number
  }
  providerRetry?: {
    attempt: number
    maxAttempts: number
    delayMS: number
  }
  status: 'running' | 'failed' | 'cancelled' | 'reconciling' | 'error_pending_refresh'
  /** Settlement completeness watermark; decimal string, never a Number. */
  settledRevision?: string
  error?: string
}
