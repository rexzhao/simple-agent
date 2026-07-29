import type { ActiveRunDescriptor, Bootstrap, CodexAuthStatus, ImageAttachmentInput, ItemsPage, Project, ProviderSettingsDocument, ProviderSettingsInput, RunEvent, Session, SessionModelOptions } from './types'

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
  createSession: (projectID: string, provider: string, modelProfile: string, reasoningLevel = '') => request<Session>(`/api/projects/${encodeURIComponent(projectID)}/sessions`, {
    method: 'POST',
    body: JSON.stringify({ provider, model_profile: modelProfile, reasoning_level: reasoningLevel }),
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
  renameSession: (sessionID: string, displayName: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}`, {
    method: 'PATCH',
    body: JSON.stringify({ display_name: displayName }),
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
    const query = new URLSearchParams({ limit: '50' })
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
  resendRun: (sessionID: string, replayItemID: string) => request<{ run_id: string; session_id: string; status: string }>(`/api/sessions/${encodeURIComponent(sessionID)}/runs`, {
    method: 'POST',
    body: JSON.stringify({ replay_item_id: replayItemID }),
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
  activeRuns: () => request<{ runs: ActiveRunDescriptor[] }>('/api/runs/active'),
}

const streamReconnectLimit = 5

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
