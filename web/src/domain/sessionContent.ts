export interface DomainReadError {
  readonly code: string
  readonly message: string
}

export type DataAvailability =
  | { readonly status: 'loading' }
  | { readonly status: 'ready' }
  | { readonly status: 'stale'; readonly dataUpdatedAt: string }
  | { readonly status: 'error'; readonly error: DomainReadError }

export interface SessionContentBlob {
  readonly id: string
  readonly url: string
  readonly content_type: string
  readonly size: number
  readonly sha256: string
  readonly etag: string
  readonly expires_at: string
}

export type SessionContentCreatedBy = 'user' | 'agent'
export type SessionContentItemKind = 'message' | 'compaction' | 'runtime_context'
export type SessionContentVisibility = 'visible' | 'hidden' | 'debug'
export type SessionContentAudience = 'user' | 'model' | 'internal'
export type SessionContentItemStatus = 'pending' | 'completed' | 'error' | 'interrupted'
export type SessionContentRunStatus = 'running' | 'committed' | 'failed' | 'interrupted' | 'cancelled'
export type SessionContentRole = 'system' | 'developer' | 'user' | 'assistant' | 'tool' | 'provider'

export interface SessionContentMetadata {
  readonly id: string
  readonly version: number
  readonly created_at: string
  readonly updated_at: string
  readonly display_name?: string
  readonly created_by?: SessionContentCreatedBy
  readonly parent_session_id?: string
  readonly root_session_id?: string
  readonly spawn_depth?: number
  readonly archived: boolean
  readonly archived_at?: string
  readonly last_used_at: string
  readonly current_run_id?: string
  readonly running_run_id?: string
  readonly running_turn_id?: string
  readonly interrupted_run_id?: string
  readonly interrupted_turn_id?: string
  readonly interrupted_at?: string
  readonly latest_run_id?: string
  readonly last_run_id?: string
  readonly last_run_status?: SessionContentRunStatus
  readonly has_unread_result: boolean
  readonly provider?: string
  readonly model_profile?: string
  readonly model_id?: string
  readonly pricing?: Record<string, unknown>
  readonly reasoning_level?: string
  readonly model_parameters?: Record<string, unknown>
  readonly status: 'idle' | 'running' | 'failed' | 'interrupted'
  readonly project_id?: string
  readonly cwd?: string
  readonly created_cwd?: string
  readonly config_path?: string
  readonly config_dir?: string
  readonly enabled_tools?: readonly string[]
  readonly enabled_mcp?: readonly string[]
  readonly enabled_skills?: readonly string[]
  readonly show_reasoning: boolean
  readonly full_access: boolean
  readonly debug: { readonly request_bodies: boolean }
  readonly context: Record<string, unknown>
  readonly save_tool_results: boolean
  readonly active_history?: readonly string[]
}

export interface SessionContentHistoryDescriptor {
  readonly limit: number
  readonly before_item_seq?: string
  readonly after_item_seq?: string
  readonly align_turn: boolean
  readonly visible_only: boolean
  readonly oldest_item_seq?: string
  readonly newest_item_seq?: string
  readonly has_more_before: boolean
  readonly has_more_after: boolean
}

export interface SessionContentItemKey {
  readonly turn_id: string
  readonly agent_iteration: number
  readonly item_id: string
}

export interface SessionContentText {
  readonly inline?: string
  readonly preview?: string
  readonly blob?: SessionContentBlob
  readonly content_type?: 'text/plain' | 'text/plain; charset=utf-8' | 'application/json'
  readonly truncated?: boolean
}

export interface SessionContentMessage {
  readonly role: SessionContentRole
  readonly content?: SessionContentText
  readonly reasoning?: SessionContentText
  readonly images?: readonly { readonly hash: string; readonly media_type: string; readonly size_bytes: number }[]
  readonly tool_call_id?: string
  readonly tool_calls?: readonly { readonly id: string; readonly name: string; readonly arguments?: SessionContentText }[]
  readonly is_error?: boolean
}

export interface SessionContentItem {
  readonly key: SessionContentItemKey
  readonly seq: number
  readonly created_at: string
  readonly kind: SessionContentItemKind
  readonly visibility: SessionContentVisibility
  readonly audience: SessionContentAudience
  readonly status?: SessionContentItemStatus
  readonly message?: SessionContentMessage
}

export interface SessionContentHistoryWindow {
  readonly items: readonly SessionContentItem[]
  readonly descriptor: SessionContentHistoryDescriptor
}

/** A page read is an application operation, not a wire/blob operation. */
export interface SessionContentHistoryReadOptions {
  readonly cursor?: number
  readonly direction?: 'before' | 'after'
  readonly limit?: number
  readonly alignTurn?: boolean
}

export interface SessionContentHistoryState {
  readonly loading: boolean
  /** Monotonic operation identity.  A history operation is observable even
   * when the durable content resource itself did not change. */
  readonly version: number
  readonly error?: DomainReadError
}

export interface SessionContentActiveRun {
  readonly run_id: string
  readonly session_id: string
  readonly turn_id?: string
  readonly started_at: string
  readonly status: 'running'
  readonly recoverable: boolean
  readonly run_epoch?: string
  readonly run_cursor?: string
  readonly replay_available: boolean
  readonly replay_from_cursor?: string
  readonly replay_to_cursor?: string
  readonly recovery_required: boolean
  readonly durable_settlement_watermark?: SessionContentSettlementWatermark
}

export interface SessionContentTransientItemWatermark {
  readonly turn_id: string
  readonly agent_iteration: number
  readonly item_id: string
  readonly run_cursor: string
}

export interface SessionContentSettlementWatermark {
  readonly resource_revision: string
  readonly run_cursor: string
  readonly verified: boolean
  readonly covered_items: readonly SessionContentTransientItemWatermark[]
}

export interface SessionContentCompactionCheckpoint {
  readonly id: string
  readonly created_at: string
  readonly reason: string
  readonly phase: string
  readonly trigger: string
  readonly summary_item_id: string
  readonly from_item_id?: string
  readonly to_item_id?: string
  readonly previous_active_history?: readonly string[]
  readonly replacement_history: readonly string[]
  readonly summary_provider?: string
  readonly summary_model?: string
}

export interface SessionContentCompaction {
  readonly checkpoints: readonly SessionContentCompactionCheckpoint[]
  readonly truncated: boolean
}

export interface SessionContentSnapshot {
  readonly schema_version: 1
  readonly session: SessionContentMetadata
  readonly history: SessionContentHistoryWindow
  readonly active_run: SessionContentActiveRun | null
  readonly compaction: SessionContentCompaction
}

export interface SessionLiveMessage {
  readonly key: SessionContentItemKey
  readonly revision: string
  readonly status: 'streaming' | 'complete' | 'incomplete'
  /** False when only lifecycle metadata is available and the wire snapshot was omitted. */
  readonly snapshotAvailable?: boolean
  readonly message: SessionContentMessage
}

export interface SessionReasoningTiming {
  readonly startedAt?: string
  readonly endedAt?: string
}

export type SessionToolStatus = 'requested' | 'running' | 'finished'

export interface SessionToolState {
  readonly tool_call_id: string
  readonly turn_id: string
  readonly agent_iteration: number
  readonly name: string
  readonly status: SessionToolStatus
  readonly arguments: string
  readonly content?: string
  readonly is_error?: boolean
}

export interface SessionPrompt {
  readonly id: string
  readonly content: string
  readonly steer: boolean
}

/** First-seen order of live process identities. Lifecycle updates reuse the
 * same entry so status/content changes do not duplicate a rendered row. */
export interface SessionRunStepRef {
  readonly kind: 'reasoning' | 'tool'
  readonly key: string
}

export interface SessionRunFailure {
  readonly turnID: string
  readonly code: string
  readonly message: string
}

/** Domain-only transient run overlay. It contains execution identity/cursors,
 * but never subscription identifiers, stream sequences, generations or blobs. */
export interface SessionRunState {
  readonly runEpoch: string
  readonly runID: string
  readonly runCursor: string
  readonly turnID?: string
  readonly status: 'running' | 'committed' | 'failed' | 'interrupted' | 'cancelled'
  readonly messages: Readonly<Record<string, SessionLiveMessage>>
  readonly reasoningTimings?: Readonly<Record<string, SessionReasoningTiming>>
  readonly tools: Readonly<Record<string, SessionToolState>>
  readonly stepOrder: readonly SessionRunStepRef[]
  readonly promptQueue: readonly SessionPrompt[]
  readonly appendedPrompts: readonly string[]
  readonly settlement?: SessionContentSettlementWatermark
  readonly stale: boolean
  readonly recoveryRequired: boolean
}

export interface SessionContentState {
  readonly snapshot: SessionContentSnapshot
  readonly durableResourceRevision: string
  readonly transientRun: SessionRunState | null
  readonly turnFailure?: SessionRunFailure
}

export interface SessionView {
  readonly availability: DataAvailability
  readonly dataAvailability: DataAvailability
  readonly session?: SessionContentMetadata
  readonly history: SessionContentHistoryWindow
  readonly historyState: SessionContentHistoryState
  readonly activeRun: SessionContentActiveRun | null
  readonly compaction: SessionContentCompaction
  readonly turnFailure?: SessionRunFailure
  readonly runState?: SessionRunState
  readonly error?: DomainReadError
}

/**
 * Returns whether Session Content has observed a run identity at any
 * authority layer. A terminal run can have no live overlay after settlement,
 * so admission barriers must not use the presentation-only active row as their
 * completion condition.
 */
export function hasObservedRun(view: SessionView, runID: string): boolean {
  if (!runID) return false
  if (view.runState?.runID === runID || view.activeRun?.run_id === runID) return true
  const metadata = view.session
  if (!metadata) return false
  return [
    metadata.current_run_id,
    metadata.running_run_id,
    metadata.interrupted_run_id,
    metadata.latest_run_id,
    metadata.last_run_id,
  ].includes(runID)
}

export const emptyHistoryDescriptor: SessionContentHistoryDescriptor = Object.freeze({
  limit: 0,
  align_turn: false,
  visible_only: true,
  has_more_before: false,
  has_more_after: false,
})

export const emptyHistory: SessionContentHistoryWindow = Object.freeze({
  items: Object.freeze([]),
  descriptor: emptyHistoryDescriptor,
})

export const emptyCompaction: SessionContentCompaction = Object.freeze({
  checkpoints: Object.freeze([]),
  truncated: false,
})
