import type { CommandOptions } from './sessionCommands'

export interface ProjectCreateOptions {
  /** Caller-owned durable identity. Reuse it after a timeout or epoch change. */
  readonly operationID?: string
}

export interface ProjectCreateResult {
  readonly operation_id: string
  readonly project_id: string
  readonly created: boolean
}

export interface ProjectRenameResult {
  readonly project_id: string
  readonly display_name: string
}

export interface ProjectArchiveResult {
  readonly project_id: string
  readonly archived: boolean
}

export interface ProjectDeleteResult {
  readonly project_id: string
  readonly status: 'removed'
  readonly removed_sessions: number
}

/** Page-independent typed project command boundary. */
export interface ProjectCommands {
  createProject(root: string, displayName: string, options?: ProjectCreateOptions, commandOptions?: CommandOptions): Promise<ProjectCreateResult>
  renameProject(projectID: string, displayName: string, options?: CommandOptions): Promise<ProjectRenameResult>
  archiveProject(projectID: string, options?: CommandOptions): Promise<ProjectArchiveResult>
  restoreProject(projectID: string, options?: CommandOptions): Promise<ProjectArchiveResult>
  deleteProject(projectID: string, options?: CommandOptions): Promise<ProjectDeleteResult>
}
