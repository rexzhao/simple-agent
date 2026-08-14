import type { ItemsPage } from '../types'

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

export interface SessionCreateOptions {
  readonly sessionID?: string
  readonly displayName?: string
  readonly parentSessionID?: string
  readonly cwd?: string
  readonly configPath?: string
  readonly provider?: string
  readonly modelProfile?: string
  readonly reasoningLevel?: string
  readonly fullAccess?: boolean
  readonly automaticCompaction?: boolean
}

export interface SessionCreateResult {
  readonly session_id: string
  readonly project_id: string
}

export interface SessionDeleteResult {
  readonly session_id: string
  readonly status: 'removed'
  readonly removed_sessions: number
}

export interface SessionCompactResult {
  readonly session_id: string
  readonly status: 'committed'
  readonly compaction_id: string
  readonly summary_item_id: string
  readonly revision: string
}

export interface SessionHistoryReadOptions {
  readonly cursor?: number
  readonly direction?: 'before' | 'after'
  readonly limit?: number
  readonly alignTurn?: boolean
}

/** Immutable HTTP data-plane metadata; the transport layer owns the fetch. */
export interface SessionHistoryBlobDescriptor {
  readonly id: string
  readonly url: string
  readonly content_type: string
  readonly size: number
  readonly sha256: string
  readonly etag: string
  readonly expires_at: string
}

export interface SessionHistoryReadResult {
  readonly session_id: string
  readonly cursor: number
  readonly direction: '' | 'before' | 'after'
  readonly limit: number
  readonly align_turn: boolean
  readonly history: ItemsPage | null
  readonly blob: SessionHistoryBlobDescriptor | null
}

export interface SessionCommands {
  create(projectID: string, options?: SessionCreateOptions, commandOptions?: CommandOptions): Promise<SessionCreateResult>
  markRead(
    sessionID: string,
    runID: string,
    projectID?: string,
    options?: CommandOptions,
  ): Promise<SessionMarkReadResult>
  rename(sessionID: string, displayName: string, options?: CommandOptions): Promise<SessionRenameResult>
  archive(sessionID: string, options?: CommandOptions): Promise<SessionArchiveResult>
  restore(sessionID: string, options?: CommandOptions): Promise<SessionArchiveResult>
  delete(sessionID: string, options?: CommandOptions): Promise<SessionDeleteResult>
  /** Explicit alias for callers that avoid the JavaScript keyword method. */
  deleteSession(sessionID: string, options?: CommandOptions): Promise<SessionDeleteResult>
  compact(sessionID: string, options?: CommandOptions): Promise<SessionCompactResult>
  historyRead(sessionID: string, historyOptions?: SessionHistoryReadOptions, commandOptions?: CommandOptions): Promise<SessionHistoryReadResult>
  /** Alias matching the read-oriented domain vocabulary. */
  readHistory(sessionID: string, historyOptions?: SessionHistoryReadOptions, commandOptions?: CommandOptions): Promise<SessionHistoryReadResult>
  setFullAccess(sessionID: string, fullAccess: boolean, options?: CommandOptions): Promise<SessionFullAccessResult>
  setDebug(sessionID: string, requestBodies: boolean, options?: CommandOptions): Promise<SessionDebugResult>
}
