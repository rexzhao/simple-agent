import type { ActiveRunDescriptor, Bootstrap, CodexAuthStatus, ImageAttachmentInput, ItemsPage, LifecycleEvent, Project, ProviderSettingsDocument, ProviderSettingsInput, RunEvent, Session, SessionDebugSettings, SessionModelOptions, SessionSnapshot } from './types'

const tokenStorageKey = 'sai-capability-token'

function capabilityToken(): string {
  const hash = new URLSearchParams(window.location.hash.slice(1))
  const fromHash = hash.get('token')
  if (fromHash) {
    window.sessionStorage.setItem(tokenStorageKey, fromHash)
    window.history.replaceState(null, '', window.location.pathname + window.location.search)
    return fromHash
  }
  return window.sessionStorage.getItem(tokenStorageKey) ?? ''
}

const token = capabilityToken()

export class APIError extends Error {
  status: number
  code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'APIError'
    this.status = status
    this.code = code
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')
  const response = await fetch(path, { ...init, headers })
  if (!response.ok) {
    let code = 'request_failed'
    let message = `Request failed (${response.status})`
    try {
      const payload = await response.json() as { error?: { code?: string; message?: string } }
      code = payload.error?.code ?? code
      message = payload.error?.message ?? message
    } catch {
      // Preserve the safe fallback above.
    }
    throw new APIError(response.status, code, message)
  }
  if (response.status === 204) return undefined as T
  return await response.json() as T
}

export const api = {
  bootstrap: () => request<Bootstrap>('/api/bootstrap'),
  projects: () => request<{ projects: Project[] }>('/api/projects'),
  createProject: (root: string, displayName: string) => request<{ project: Project; created: boolean }>('/api/projects', {
    method: 'POST',
    body: JSON.stringify({ root, display_name: displayName }),
  }),
  renameProject: (projectID: string, displayName: string) => request<Project>(`/api/projects/${encodeURIComponent(projectID)}`, {
    method: 'PATCH',
    body: JSON.stringify({ display_name: displayName }),
  }),
  archiveProject: (projectID: string) => request<Project>(`/api/projects/${encodeURIComponent(projectID)}/archive`, {
    method: 'POST',
    body: '{}',
  }),
  restoreProject: (projectID: string) => request<Project>(`/api/projects/${encodeURIComponent(projectID)}/restore`, {
    method: 'POST',
    body: '{}',
  }),
  deleteProject: (projectID: string) => request<{ status: string; id: string; removed_sessions: number }>(`/api/projects/${encodeURIComponent(projectID)}`, {
    method: 'DELETE',
  }),
  sessions: (projectID: string, archived = false) => request<{ sessions: Session[] }>(`/api/projects/${encodeURIComponent(projectID)}/sessions${archived ? '?archived=true' : ''}`),
  sessionModels: (projectID: string) => request<SessionModelOptions>(`/api/projects/${encodeURIComponent(projectID)}/models`),
  createSession: (projectID: string, provider: string, modelProfile: string, reasoningLevel = '', fullAccess = false) => request<Session>(`/api/projects/${encodeURIComponent(projectID)}/sessions`, {
    method: 'POST',
    body: JSON.stringify({ provider, model_profile: modelProfile, reasoning_level: reasoningLevel, full_access: fullAccess }),
  }),
  providerSettings: () => request<ProviderSettingsDocument>('/api/provider-settings'),
  createProvider: (input: ProviderSettingsInput) => request<ProviderSettingsDocument>('/api/providers', {
    method: 'POST',
    body: JSON.stringify(input),
  }),
  updateProvider: (providerName: string, input: ProviderSettingsInput) => request<ProviderSettingsDocument>(`/api/providers/${encodeURIComponent(providerName)}`, {
    method: 'PUT',
    body: JSON.stringify(input),
  }),
  updateProviderDefault: (provider: string, model: string) => request<ProviderSettingsDocument>('/api/provider-default', {
    method: 'PATCH',
    body: JSON.stringify({ provider, model }),
  }),
  discoverProviderModels: (providerName: string) => request<{ models: string[] }>(`/api/providers/${encodeURIComponent(providerName)}/models`),
  startCodexLogin: (providerName: string) => request<CodexAuthStatus>(`/api/providers/${encodeURIComponent(providerName)}/codex-login`, {
    method: 'POST',
    body: '{}',
  }),
  codexLoginStatus: (providerName: string) => request<CodexAuthStatus>(`/api/providers/${encodeURIComponent(providerName)}/codex-login`),
  clearCodexLogin: (providerName: string) => request<void>(`/api/providers/${encodeURIComponent(providerName)}/codex-login`, { method: 'DELETE' }),
  session: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}`),
  snapshot: (sessionID: string) => request<SessionSnapshot>(`/api/sessions/${encodeURIComponent(sessionID)}/snapshot`),
  renameSession: (sessionID: string, displayName: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}`, {
    method: 'PATCH',
    body: JSON.stringify({ display_name: displayName }),
  }),
  setSessionFullAccess: (sessionID: string, fullAccess: boolean) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/full-access`, {
    method: 'POST',
    body: JSON.stringify({ full_access: fullAccess }),
  }),
  setSessionDebug: (sessionID: string, debug: SessionDebugSettings) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/debug`, {
    method: 'POST',
    body: JSON.stringify({ debug }),
  }),
  archiveSession: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/archive`, {
    method: 'POST',
    body: '{}',
  }),
  restoreSession: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/restore`, {
    method: 'POST',
    body: '{}',
  }),
  deleteSession: (sessionID: string) => request<{ status: string; id: string }>(`/api/sessions/${encodeURIComponent(sessionID)}`, {
    method: 'DELETE',
  }),
  items: (sessionID: string, beforeSeq = 0) => {
    // align_turn keeps every page whole-turn aligned: the conversation never
    // renders half a turn at the window's oldest edge, on initial load,
    // refresh, or when paging older.
    const query = new URLSearchParams({ limit: '50', align_turn: 'true' })
    if (beforeSeq > 0) query.set('before_seq', String(beforeSeq))
    return request<ItemsPage>(`/api/sessions/${encodeURIComponent(sessionID)}/items?${query}`)
  },
  compact: (sessionID: string) => request(`/api/sessions/${encodeURIComponent(sessionID)}/compact`, {
    method: 'POST',
    body: '{}',
  }),
  startRun: (sessionID: string, content: string, images: ImageAttachmentInput[] = []) => request<{ run_id: string; session_id: string; status: string }>(`/api/sessions/${encodeURIComponent(sessionID)}/runs`, {
    method: 'POST',
    body: JSON.stringify({ content, images }),
  }),
  continueRun: (sessionID: string) => request<{ run_id: string; session_id: string; status: string }>(`/api/sessions/${encodeURIComponent(sessionID)}/continue`, {
    method: 'POST',
    body: '{}',
  }),
  sessionImage: async (sessionID: string, hash: string): Promise<Blob> => {
    const response = await fetch(`/api/sessions/${encodeURIComponent(sessionID)}/images/${encodeURIComponent(hash)}`, {
      headers: { Authorization: `Bearer ${token}` },
    })
    if (!response.ok) {
      let code = 'request_failed'
      let message = `Request failed (${response.status})`
      try {
        const payload = await response.json() as { error?: { code?: string; message?: string } }
        code = payload.error?.code ?? code
        message = payload.error?.message ?? message
      } catch {
        // Preserve the safe fallback above.
      }
      throw new APIError(response.status, code, message)
    }
    return await response.blob()
  },
  cancelRun: (runID: string) => request(`/api/runs/${encodeURIComponent(runID)}`, { method: 'DELETE' }),
  cancelToolCall: (runID: string, toolCallID: string) => request(`/api/runs/${encodeURIComponent(runID)}/tools/${encodeURIComponent(toolCallID)}`, { method: 'DELETE' }),
  appendRunMessage: (runID: string, content: string) => request(`/api/runs/${encodeURIComponent(runID)}/prompts`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  }),
  removeRunMessage: (runID: string, promptID: string) => request(`/api/runs/${encodeURIComponent(runID)}/prompts/${encodeURIComponent(promptID)}`, {
    method: 'DELETE',
  }),
  steerRunMessage: (runID: string, promptID: string, steer: boolean) => request(`/api/runs/${encodeURIComponent(runID)}/prompts/${encodeURIComponent(promptID)}/steer`, {
    method: 'POST',
    body: JSON.stringify({ steer }),
  }),
  moveRunMessage: (runID: string, promptID: string, direction: 'up' | 'down') => request(`/api/runs/${encodeURIComponent(runID)}/prompts/${encodeURIComponent(promptID)}/move`, {
    method: 'POST',
    body: JSON.stringify({ direction }),
  }),
  activeRuns: () => request<{ runs: ActiveRunDescriptor[] }>('/api/runs/active'),
}

const streamReconnectLimit = 5

export interface LifecycleStreamOptions {
  signal?: AbortSignal
  onReconnect?: () => void | Promise<void>
}

function waitForStreamReconnect(attempt: number): Promise<void> {
  const delay = Math.min(200 * (2 ** attempt), 2000)
  return new Promise((resolve) => window.setTimeout(resolve, delay))
}

export async function streamRun(runID: string, onEvent: (event: RunEvent) => void | Promise<void>): Promise<void> {
  let after = 0
  let reconnects = 0

  while (true) {
    try {
      const query = after > 0 ? `?after=${encodeURIComponent(String(after))}` : ''
      const response = await fetch(`/api/runs/${encodeURIComponent(runID)}/events${query}`, {
        headers: { Authorization: `Bearer ${token}` },
      })
      if (!response.ok || !response.body) {
        throw new APIError(response.status, 'stream_failed', `Unable to connect to run events (${response.status})`)
      }

      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      let settled = false
      while (true) {
        const { done, value } = await reader.read()
        buffer += decoder.decode(value, { stream: !done })
        let boundary = buffer.indexOf('\n\n')
        while (boundary >= 0) {
          const frame = buffer.slice(0, boundary)
          buffer = buffer.slice(boundary + 2)
          const lines = frame.split('\n')
          const sequence = Number(lines.find((line) => line.startsWith('id:'))?.slice(3).trim() ?? '')
          const data = lines
            .filter((line) => line.startsWith('data:'))
            .map((line) => line.slice(5).trimStart())
            .join('\n')
          if (data) {
            const event = JSON.parse(data) as RunEvent
            await onEvent(event)
            if (Number.isSafeInteger(sequence) && sequence > after) after = sequence
            reconnects = 0
            if (event.type === 'run.settled') settled = true
          }
          boundary = buffer.indexOf('\n\n')
        }
        if (done) break
      }
      if (settled) return
    } catch (reason) {
      if (reason instanceof APIError) throw reason
    }

    if (reconnects >= streamReconnectLimit) {
      throw new APIError(0, 'stream_interrupted', 'The run event stream was interrupted. Refresh the session to view saved results.')
    }
    await waitForStreamReconnect(reconnects)
    reconnects++
  }
}

/**
 * Streams process-wide durable lifecycle events. Unlike a run stream this is
 * intentionally long-lived: a closed best-effort hub subscription is healed
 * by reconnecting and letting the caller bootstrap its durable snapshot.
 */
export async function streamLifecycle(
  onEvent: (event: LifecycleEvent) => void | Promise<void>,
  options: LifecycleStreamOptions = {},
): Promise<void> {
  let reconnectAttempt = 0

  while (!options.signal?.aborted) {
    try {
      const response = await fetch('/api/events', {
        headers: { Authorization: `Bearer ${token}` },
        signal: options.signal,
      })
      if (!response.ok || !response.body) {
        throw new APIError(response.status, 'stream_failed', `Unable to connect to lifecycle events (${response.status})`)
      }

      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      let eventName = ''
      let dataLines: string[] = []
      let sawEvent = false

      const dispatchFrame = async (frame: string): Promise<void> => {
        const lines = frame.split('\n')
        for (const line of lines) {
          if (line.startsWith(':')) continue
          if (line.startsWith('event:')) {
            eventName = line.slice(6).trim()
          } else if (line.startsWith('data:')) {
            dataLines.push(line.slice(5).trimStart())
          }
        }
        if (dataLines.length > 0) {
          const data = dataLines.join('\n')
          dataLines = []
          const event = JSON.parse(data) as LifecycleEvent
          if (!event.type && eventName) event.type = eventName as LifecycleEvent['type']
          eventName = ''
          await onEvent(event)
          sawEvent = true
        } else {
          eventName = ''
        }
      }

      while (true) {
        const { done, value } = await reader.read()
        buffer += decoder.decode(value, { stream: !done })
        // The backend currently emits LF frames. Normalize CRLF as well so
        // proxies that rewrite line endings do not corrupt the parser.
        buffer = buffer.replaceAll('\r\n', '\n').replaceAll('\r', '\n')
        let boundary = buffer.indexOf('\n\n')
        while (boundary >= 0) {
          const frame = buffer.slice(0, boundary)
          buffer = buffer.slice(boundary + 2)
          await dispatchFrame(frame)
          boundary = buffer.indexOf('\n\n')
        }
        if (done) {
          if (buffer.trim()) await dispatchFrame(buffer)
          break
        }
      }

      // Any successfully delivered event means the connection itself worked;
      // do not keep an old failure backoff after the next disconnect.
      if (sawEvent) reconnectAttempt = 0
    } catch (reason) {
      if (options.signal?.aborted) return
      // Lifecycle delivery is best effort. A failed connection is handled by
      // the same reconnect path as an EOF, including a fresh bootstrap.
      void reason
    }

    if (options.signal?.aborted) return
    // Awaiting this callback makes reconnect reconciliation single-flight with
    // the next connection, rather than allowing a reconnect storm to launch
    // overlapping bootstrap requests.
    try {
      await options.onReconnect?.()
    } catch {
      // The caller owns error presentation; continue reconnecting even when a
      // transient bootstrap request failed.
    }
    if (options.signal?.aborted) return
    const delay = Math.min(200 * (2 ** reconnectAttempt), 2000)
    reconnectAttempt = Math.min(reconnectAttempt + 1, 4)
    const shouldContinue = await new Promise<boolean>((resolve) => {
      let timer = 0
      const abort = () => {
        window.clearTimeout(timer)
        options.signal?.removeEventListener('abort', abort)
        resolve(false)
      }
      timer = window.setTimeout(() => {
        options.signal?.removeEventListener('abort', abort)
        resolve(true)
      }, delay)
      options.signal?.addEventListener('abort', abort, { once: true })
      if (options.signal?.aborted) abort()
    })
    if (!shouldContinue) return
  }
}
