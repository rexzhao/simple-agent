export interface SessionMarkReadResult {
  readonly session_id: string
  readonly run_id: string
  readonly marked_read: boolean
}

export interface SessionCommands {
  markRead(
    sessionID: string,
    runID: string,
    projectID?: string,
    options?: { signal?: AbortSignal },
  ): Promise<SessionMarkReadResult>
}
