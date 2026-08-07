export interface CodexLoginCommandOptions {
  readonly signal?: AbortSignal
}

/** A bounded acknowledgement. Auth state and device capabilities arrive via
 * the codex_login resource, never through this result. */
export interface CodexLoginStartResult {
  readonly provider: string
  readonly status: 'accepted'
}

export interface CodexLoginClearResult {
  readonly provider: string
  readonly status: 'cleared'
}

export interface CodexLoginCommands {
  startCodexLogin(provider: string, options?: CodexLoginCommandOptions): Promise<CodexLoginStartResult>
  clearCodexLogin(provider: string, options?: CodexLoginCommandOptions): Promise<CodexLoginClearResult>
}
