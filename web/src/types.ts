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

export interface ProviderModelSettings {
  profile: string
  id: string
  type: string
  input?: string[]
  developer_role?: string
  context_window?: number
  input_limit?: number
  output_limit?: number
  parameters?: Record<string, unknown>
  reasoning_config?: ReasoningConfig
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
  warning_issued?: boolean
}

export interface Session {
  id: string
  created_at: string
  updated_at: string
  display_name: string
  archived: boolean
  last_used_at: string
  provider: string
  model_profile: string
  model_id: string
  project_id: string
  created_cwd: string
  last_seq: number
  status?: string
  show_reasoning?: boolean
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

export type RunEvent =
  | { type: 'turn.started'; turn_id: string }
	| { type: 'agent.iteration.started'; turn_id: string; agent_iteration: number }
	| { type: 'text.delta'; turn_id: string; agent_iteration: number; text: string }
	| { type: 'reasoning.delta'; turn_id: string; agent_iteration: number; text: string }
	| { type: 'tool.requested' | 'tool.started'; turn_id: string; agent_iteration: number; tool_call_id: string; name: string; arguments?: string }
	| { type: 'tool.finished'; turn_id: string; agent_iteration: number; tool_call_id: string; name: string; is_error: boolean; content?: string }
	| { type: 'usage.updated'; turn_id: string; agent_iteration: number; input_tokens: number; output_tokens: number; total_tokens: number; cached_tokens: number; cache_write_tokens: number; reasoning_tokens: number }
  | { type: 'run.resync_required'; run_id: string; session_id: string; oldest_seq: number }
  | { type: 'turn.committed'; turn_id: string; last_seq: number }
  | { type: 'turn.failed'; turn_id: string; code: string; message: string }
  | { type: 'run.settled'; run_id: string; status: string; turn_id?: string; last_seq?: number; message?: string }
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
	label?: string
}

export interface ModelOutputActivity {
	kind: 'output'
	id: string
	text: string
	iteration: number
}

export type RunStep = ReasoningActivity | ModelOutputActivity | ToolActivity

export interface ActiveRun {
  id: string
	sessionID: string
	turnID?: string
  restored?: boolean
  userText: string
  userImages?: ImageAttachmentInput[]
	assistantText: string
	steps: RunStep[]
  agentIteration: number
  inputTokens?: number
  totalTokens?: number
	cachedTokens?: number
	cacheWriteTokens?: number
	reasoningTokens?: number
  status: 'running' | 'failed' | 'cancelled'
  error?: string
}
