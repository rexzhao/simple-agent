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

export interface CodexUsageWindow {
  used_percent: number
  limit_window_seconds: number
  reset_after_seconds: number
  reset_at: number
}

export interface CodexUsageWindowSet {
  allowed: boolean
  limit_reached: boolean
  primary_window?: CodexUsageWindow | null
  secondary_window?: CodexUsageWindow | null
}

export interface CodexUsageAdditional {
  limit_name: string
  metered_feature: string
  rate_limit?: CodexUsageWindowSet
}

export interface CodexUsageCredits {
  has_credits: boolean
  unlimited: boolean
  overage_limit_reached: boolean
  balance: string
  approx_local_messages?: number[]
  approx_cloud_messages?: number[]
}

export interface CodexUsage {
  user_id: string
  account_id: string
  email: string
  plan_type: string
  rate_limit?: CodexUsageWindowSet
  additional_rate_limits?: CodexUsageAdditional[]
  credits?: CodexUsageCredits
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
  /** These are optional in Session Content; absence means server default. */
  provider?: string
  model_profile?: string
  model_id?: string
  pricing?: ModelPricing
  reasoning_level?: string
  project_id: string
  created_cwd?: string
  last_seq: number
  /** Precision-safe decimal form of the session last_seq watermark. */
  revision?: string
  status?: string
  show_reasoning?: boolean
  full_access: boolean
  debug?: SessionDebugSettings
  cwd?: string
  config_path?: string
  context?: ContextMetadata
}

export interface MessageContent {
  inline?: string
  preview?: string
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
	/** Event-envelope timing retained by the session-content overlay when available. */
	reasoningTiming?: { startedAt?: string; endedAt?: string }
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
	queuedPrompts?: QueuedPrompt[]
	/** Transient process boundaries for drained prompts; never a user item. */
	processBoundaries?: Array<{ id: string; stepIndex: number }>
	assistantText: string
	/** Content-backed transient tails. Each entry is owned by exactly one full
	 * (turn_id, agent_iteration, item_id) identity. It is intentionally a map,
	 * never an unkeyed concatenated string. */
	assistantTails?: Record<string, { turnID: string; agentIteration: number; itemID: string; text: string; durableTextLength: number }>
	/** Explicit transient-to-durable assistant identity, keyed by turn/iteration. */
	assistantItems?: Record<string, { itemID: string; durableTextLength: number }>
	/** Full identity bindings. assistantItems remains a compatibility index. */
	assistantItemBindings?: Record<string, { turnID: string; agentIteration: number; itemID: string; durableTextLength: number }>
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
