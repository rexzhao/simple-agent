// Kept in lockstep with execution.SessionRunStatus. CoordinatedSessionRun
// returns the same enum, including the admission-time running status.
export type RunStatus = 'running' | 'committed' | 'failed' | 'interrupted' | 'cancelled'

export interface RunStartOptions {
  /** Caller-owned stable identity. Reuse it for a timeout/page-restore retry. */
  readonly runID?: string
  readonly signal?: AbortSignal
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
}
