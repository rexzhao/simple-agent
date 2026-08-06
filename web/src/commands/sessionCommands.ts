export interface CommandOptions {
  readonly signal?: AbortSignal
}

export interface SessionMarkReadResult {
  readonly session_id: string
  readonly run_id: string
  readonly marked_read: boolean
}

export interface SessionRenameResult {
  readonly session_id: string
  readonly display_name: string
}

export interface SessionArchiveResult {
  readonly session_id: string
  readonly archived: boolean
}

export interface SessionFullAccessResult {
  readonly session_id: string
  readonly full_access: boolean
}

export interface SessionDebugResult {
  readonly session_id: string
  readonly request_bodies: boolean
}

export interface SessionCommands {
  markRead(
    sessionID: string,
    runID: string,
    projectID?: string,
    options?: CommandOptions,
  ): Promise<SessionMarkReadResult>
  rename(sessionID: string, displayName: string, options?: CommandOptions): Promise<SessionRenameResult>
  archive(sessionID: string, options?: CommandOptions): Promise<SessionArchiveResult>
  restore(sessionID: string, options?: CommandOptions): Promise<SessionArchiveResult>
  setFullAccess(sessionID: string, fullAccess: boolean, options?: CommandOptions): Promise<SessionFullAccessResult>
  setDebug(sessionID: string, requestBodies: boolean, options?: CommandOptions): Promise<SessionDebugResult>
}
