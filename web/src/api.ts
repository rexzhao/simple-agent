import type { Bootstrap, ItemsPage, Project, RunEvent, Session } from './types'

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
    let message = `请求失败 (${response.status})`
    try {
      const payload = await response.json() as { error?: { code?: string; message?: string } }
      code = payload.error?.code ?? code
      message = payload.error?.message ?? message
    } catch {
      // Preserve the safe fallback above.
    }
    throw new APIError(response.status, code, message)
  }
  return await response.json() as T
}

export const api = {
  bootstrap: () => request<Bootstrap>('/api/bootstrap'),
  projects: () => request<{ projects: Project[] }>('/api/projects'),
  createProject: (root: string, displayName: string) => request<{ project: Project; created: boolean }>('/api/projects', {
    method: 'POST',
    body: JSON.stringify({ root, display_name: displayName }),
  }),
  sessions: (projectID: string) => request<{ sessions: Session[] }>(`/api/projects/${encodeURIComponent(projectID)}/sessions`),
  createSession: (projectID: string) => request<Session>(`/api/projects/${encodeURIComponent(projectID)}/sessions`, {
    method: 'POST',
    body: '{}',
  }),
  session: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}`),
  items: (sessionID: string, beforeSeq = 0) => {
    const query = new URLSearchParams({ limit: '50' })
    if (beforeSeq > 0) query.set('before_seq', String(beforeSeq))
    return request<ItemsPage>(`/api/sessions/${encodeURIComponent(sessionID)}/items?${query}`)
  },
  compact: (sessionID: string) => request(`/api/sessions/${encodeURIComponent(sessionID)}/compact`, {
    method: 'POST',
    body: '{}',
  }),
  startRun: (sessionID: string, content: string) => request<{ run_id: string; session_id: string; status: string }>(`/api/sessions/${encodeURIComponent(sessionID)}/runs`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  }),
  cancelRun: (runID: string) => request(`/api/runs/${encodeURIComponent(runID)}`, { method: 'DELETE' }),
}

export async function streamRun(runID: string, onEvent: (event: RunEvent) => void | Promise<void>): Promise<void> {
  const response = await fetch(`/api/runs/${encodeURIComponent(runID)}/events`, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!response.ok || !response.body) {
    throw new APIError(response.status, 'stream_failed', `无法连接运行事件 (${response.status})`)
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  while (true) {
    const { done, value } = await reader.read()
    buffer += decoder.decode(value, { stream: !done })
    let boundary = buffer.indexOf('\n\n')
    while (boundary >= 0) {
      const frame = buffer.slice(0, boundary)
      buffer = buffer.slice(boundary + 2)
      const data = frame.split('\n')
        .filter((line) => line.startsWith('data:'))
        .map((line) => line.slice(5).trimStart())
        .join('\n')
      if (data) await onEvent(JSON.parse(data) as RunEvent)
      boundary = buffer.indexOf('\n\n')
    }
    if (done) break
  }
}
