// Kept in lockstep with execution.SessionRunStatus. CoordinatedSessionRun
// returns the same enum, including the admission-time running status.
export type RunStatus = 'running' | 'committed' | 'failed' | 'cancelled'

export interface RunCancelResult {
  readonly run_id: string
  readonly status: RunStatus
}

export interface RunCommands {
  /**
   * Cancels an active in-memory run. A resend in the same server epoch is
   * gateway-deduped; after an epoch change the outcome is intentionally
   * unknown because active run handles are not durable yet.
   */
  cancelRun(runID: string, options?: { readonly signal?: AbortSignal }): Promise<RunCancelResult>
}
