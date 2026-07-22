export interface Bootstrap {
  version: string
  cwd: string
}

export interface Project {
  id: string
  root: string
  display_name: string
  archived: boolean
  created_at: string
  updated_at: string
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
}

export interface MessageContent {
  inline?: string
  preview?: string
}

export interface SessionItem {
  seq: number
  id: string
  turn_id?: string
  created_at: string
  kind: string
  visibility: string
  audience: string
  status?: string
  message?: {
    role: 'user' | 'assistant' | string
    content?: MessageContent
  }
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
  | { type: 'text.delta'; turn_id: string; text: string }
  | { type: 'reasoning.delta'; turn_id: string; text: string }
  | { type: 'tool.requested' | 'tool.started'; turn_id: string; tool_call_id: string; name: string }
  | { type: 'tool.finished'; turn_id: string; tool_call_id: string; name: string; is_error: boolean }
  | { type: 'usage.updated'; turn_id: string; input_tokens: number; output_tokens: number; total_tokens: number }
  | { type: 'turn.committed'; turn_id: string; last_seq: number }
  | { type: 'turn.failed'; turn_id: string; code: string; message: string }
  | { type: 'run.settled'; run_id: string; status: string; turn_id?: string; last_seq?: number; message?: string }
  | { type: string; [key: string]: unknown }

export interface ToolActivity {
  id: string
  name: string
  status: 'requested' | 'running' | 'finished' | 'error'
}

export interface ActiveRun {
  id: string
  userText: string
  assistantText: string
  reasoningText: string
  tools: ToolActivity[]
  totalTokens?: number
  status: 'running' | 'failed' | 'cancelled'
  error?: string
}
