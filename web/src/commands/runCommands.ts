// Kept in lockstep with execution.SessionRunStatus. CoordinatedSessionRun
// returns the same enum, including the admission-time running status.
export type RunStatus = 'running' | 'committed' | 'failed' | 'interrupted' | 'cancelled'

export interface RunStartOptions {
  /** Caller-owned stable identity. Reuse it for a timeout/page-restore retry. */
  readonly runID?: string
  readonly images?: readonly RunImageReference[]
  readonly signal?: AbortSignal
}

export interface RunImageReference {
  readonly hash: string
  readonly media_type: string
  readonly size_bytes: number
  readonly detail?: 'auto' | 'low' | 'high'
}

export interface RunStartResult {
  readonly session_id: string
  readonly run_id: string
  readonly status: RunStatus
}

export interface RunContinueOptions {
  /** Caller-owned stable identity. Reuse it for a timeout/page-restore retry. */
  readonly runID?: string
  readonly signal?: AbortSignal
}

export interface RunContinueResult {
  readonly session_id: string
  readonly run_id: string
  readonly status: RunStatus
}

export interface RunCancelResult {
  readonly run_id: string
  readonly status: RunStatus
}

export interface RunPromptAppendOptions {
  /** Caller-owned stable identity. Reuse it for timeout/page/epoch retries. */
  readonly operationID?: string
  readonly signal?: AbortSignal
}

export interface RunPromptAppendResult {
  readonly operation_id: string
  readonly session_id: string
  readonly run_id: string
  readonly accepted: boolean
}

export interface RunPromptRemoveResult {
  readonly session_id: string
  readonly run_id: string
  readonly prompt_id: string
  readonly removed: boolean
}

export interface RunPromptSteerResult {
  readonly session_id: string
  readonly run_id: string
  readonly prompt_id: string
  readonly steer: boolean
}

export interface RunPromptMoveResult {
  readonly session_id: string
  readonly run_id: string
  readonly prompt_id: string
  readonly moved: boolean
}

export interface RunToolCancelResult {
  readonly session_id: string
  readonly run_id: string
  readonly tool_call_id: string
  readonly cancelled: boolean
}

export interface RunControlOptions {
  /** Process-local controls are never retried into a new server epoch. */
  readonly signal?: AbortSignal
}

export interface RunCommands {
  /**
   * Starts the bounded text-only durable command. The command request_id is
   * gateway correlation only; run_id is the client-owned execution identity.
   */
  start(sessionID: string, content: string, options?: RunStartOptions): Promise<RunStartResult>
  startRun(sessionID: string, content: string, options?: RunStartOptions): Promise<RunStartResult>
  /**
   * Resumes the durable interrupted context. It never accepts new content;
   * run_id is separate from the gateway request_id and is the retry key.
   */
  continueRun(sessionID: string, options?: RunContinueOptions): Promise<RunContinueResult>
  /**
   * Cancels an active in-memory run. A resend in the same server epoch is
   * gateway-deduped; after an epoch change the outcome is intentionally
   * unknown because active run handles are not durable yet.
   */
  cancelRun(runID: string, options?: { readonly signal?: AbortSignal }): Promise<RunCancelResult>
  /**
   * Durably admits text to the current active run. The operation_id is
   * distinct from the gateway request_id and is the retry/idempotency key.
   */
  appendPrompt(sessionID: string, runID: string, content: string, options?: RunPromptAppendOptions): Promise<RunPromptAppendResult>
  removePrompt(sessionID: string, runID: string, promptID: string, options?: RunControlOptions): Promise<RunPromptRemoveResult>
  steerPrompt(sessionID: string, runID: string, promptID: string, steer: boolean, options?: RunControlOptions): Promise<RunPromptSteerResult>
  movePrompt(sessionID: string, runID: string, promptID: string, delta: number, options?: RunControlOptions): Promise<RunPromptMoveResult>
  cancelTool(sessionID: string, runID: string, toolCallID: string, options?: RunControlOptions): Promise<RunToolCancelResult>
}
