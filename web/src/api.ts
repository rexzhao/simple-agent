import type { ActiveRunDescriptor, Bootstrap, CodexUsage, ImageAttachmentInput, ItemsPage, LifecycleEvent, Project, RunAdmission, RunEvent, Session, SessionDebugSettings, SessionModelOptions, SessionSnapshot } from './types'
import { frontendProtocolLogger, protocolLogIdentity } from './lib/frontendProtocolLogger'

export interface CreateSessionOptions {
  projectID: string
  provider?: string
  modelProfile?: string
  reasoningLevel?: string
  fullAccess?: boolean
  cwd?: string
  configPath?: string
}

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

interface ProtocolRequestContext {
  sessionID?: string
  runID?: string
  turnID?: string
}

let requestSequence = 0

async function request<T>(path: string, init: RequestInit = {}, context: ProtocolRequestContext = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')
  const requestID = `http-${++requestSequence}`
  const logRequest = (kind: string, fields: Record<string, unknown>) => {
    if (!context.sessionID || !frontendProtocolLogger.isEnabled(context.sessionID)) return
    frontendProtocolLogger.log({
      sessionID: context.sessionID,
      source: 'http',
      kind,
      runID: context.runID,
      turnID: context.turnID,
      request_id: requestID,
      method: init.method ?? 'GET',
      url: path,
      ...fields,
    })
  }

  logRequest('request.start', { body: init.body ?? null })
  try {
    const response = await fetch(path, { ...init, headers })
    const bodyText = response.status === 204 ? '' : await response.text()
    let body: unknown = bodyText
    if (bodyText) {
      try {
        body = JSON.parse(bodyText)
      } catch {
        // Preserve the raw response text in the diagnostic record.
      }
    }
    if (context.sessionID && frontendProtocolLogger.isEnabled(context.sessionID)) {
      const responseIdentity = body && typeof body === 'object'
        ? protocolLogIdentity(body as Record<string, unknown>, context.runID)
        : { runID: context.runID, turnID: context.turnID }
      logRequest('response', { status: response.status, body, body_text: bodyText, ...responseIdentity })
    }
    if (!response.ok) {
      let code = 'request_failed'
      let message = `Request failed (${response.status})`
      if (body && typeof body === 'object') {
        const payload = body as { error?: { code?: string; message?: string } }
        code = payload.error?.code ?? code
        message = payload.error?.message ?? message
      }
      const error = new APIError(response.status, code, message)
      if (context.sessionID && frontendProtocolLogger.isEnabled(context.sessionID)) {
        const responseIdentity = body && typeof body === 'object'
          ? protocolLogIdentity(body as Record<string, unknown>, context.runID)
          : { runID: context.runID, turnID: context.turnID }
        logRequest('error', { status: response.status, body, error: { name: error.name, message: error.message, code: error.code }, ...responseIdentity })
      }
      throw error
    }
    if (response.status === 204) return undefined as T
    if (!bodyText) return JSON.parse(bodyText) as T
    return body as T
  } catch (reason) {
    if (reason instanceof APIError) throw reason
    if (context.sessionID && frontendProtocolLogger.isEnabled(context.sessionID)) logRequest('error', { error: reason })
    throw reason
  }
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
  createSession: (options: CreateSessionOptions) => request<Session>(`/api/projects/${encodeURIComponent(options.projectID)}/sessions`, {
    method: 'POST',
    body: JSON.stringify({
      cwd: options.cwd,
      config_path: options.configPath,
      provider: options.provider,
      model_profile: options.modelProfile,
      reasoning_level: options.reasoningLevel,
      full_access: options.fullAccess,
    }),
  }),
  // Temporary, bounded read-only compatibility path. Provider mutations and
  // login state are owned by typed WebSocket commands/resources.
  codexUsage: (providerName: string) => request<CodexUsage>(`/api/providers/${encodeURIComponent(providerName)}/codex-usage`),
  session: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}`, {}, { sessionID }),
  snapshot: (sessionID: string) => request<SessionSnapshot>(`/api/sessions/${encodeURIComponent(sessionID)}/snapshot`, {}, { sessionID }),
  renameSession: (sessionID: string, displayName: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}`, {
    method: 'PATCH',
    body: JSON.stringify({ display_name: displayName }),
  }, { sessionID }),
  setSessionFullAccess: (sessionID: string, fullAccess: boolean) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/full-access`, {
    method: 'POST',
    body: JSON.stringify({ full_access: fullAccess }),
  }, { sessionID }),
  setSessionDebug: (sessionID: string, debug: SessionDebugSettings) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/debug`, {
    method: 'POST',
    body: JSON.stringify({ debug }),
  }, { sessionID }),
  archiveSession: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/archive`, {
    method: 'POST',
    body: '{}',
  }, { sessionID }),
  restoreSession: (sessionID: string) => request<Session>(`/api/sessions/${encodeURIComponent(sessionID)}/restore`, {
    method: 'POST',
    body: '{}',
  }, { sessionID }),
  deleteSession: (sessionID: string) => request<{ status: string; id: string }>(`/api/sessions/${encodeURIComponent(sessionID)}`, {
    method: 'DELETE',
  }, { sessionID }),
  items: (sessionID: string, beforeSeq = 0) => {
    // align_turn keeps every page whole-turn aligned: the conversation never
    // renders half a turn at the window's oldest edge, on initial load,
    // refresh, or when paging older.
    const query = new URLSearchParams({ limit: '50', align_turn: 'true' })
    if (beforeSeq > 0) query.set('before_seq', String(beforeSeq))
    return request<ItemsPage>(`/api/sessions/${encodeURIComponent(sessionID)}/items?${query}`, {}, { sessionID })
  },
  compact: (sessionID: string) => request(`/api/sessions/${encodeURIComponent(sessionID)}/compact`, {
    method: 'POST',
    body: '{}',
  }, { sessionID }),
  startRun: (sessionID: string, content: string, images: ImageAttachmentInput[] = []) => request<RunAdmission>(`/api/sessions/${encodeURIComponent(sessionID)}/runs`, {
    method: 'POST',
    body: JSON.stringify({ content, images }),
  }, { sessionID }),
  continueRun: (sessionID: string) => request<RunAdmission>(`/api/sessions/${encodeURIComponent(sessionID)}/continue`, {
    method: 'POST',
    body: '{}',
  }, { sessionID }),
  sessionImage: async (sessionID: string, hash: string): Promise<Blob> => {
    const requestID = `http-${++requestSequence}`
    const imageURL = `/api/sessions/${encodeURIComponent(sessionID)}/images/${encodeURIComponent(hash)}`
    const logImage = (kind: string, fields: Record<string, unknown> = {}) => {
      if (!frontendProtocolLogger.isEnabled(sessionID)) return
      frontendProtocolLogger.log({ sessionID, source: 'http', kind, request_id: requestID, method: 'GET', url: imageURL, ...fields })
    }
    logImage('request.start', { body: null })
    let response: Response
    try {
      response = await fetch(imageURL, {
        headers: { Authorization: `Bearer ${token}` },
      })
    } catch (reason) {
      logImage('error', { error: reason })
      throw reason
    }
    if (!response.ok) {
      let code = 'request_failed'
      let message = `Request failed (${response.status})`
      let body: unknown = ''
      try {
        const bodyText = await response.text()
        body = bodyText
        const payload = JSON.parse(bodyText) as { error?: { code?: string; message?: string } }
        code = payload.error?.code ?? code
        message = payload.error?.message ?? message
      } catch {
        // Preserve the safe fallback above.
      }
      logImage('response', { status: response.status, body })
      const error = new APIError(response.status, code, message)
      logImage('error', { status: response.status, error: { name: error.name, message: error.message, code: error.code } })
      throw error
    }
    const blob = await response.blob()
    logImage('response', { status: response.status, body: { blob_type: blob.type, size: blob.size } })
    return blob
  },
  cancelRun: (runID: string, sessionID?: string) => request(`/api/runs/${encodeURIComponent(runID)}`, { method: 'DELETE' }, { sessionID, runID }),
  cancelToolCall: (runID: string, toolCallID: string, sessionID?: string) => request(`/api/runs/${encodeURIComponent(runID)}/tools/${encodeURIComponent(toolCallID)}`, { method: 'DELETE' }, { sessionID, runID }),
  appendRunMessage: (runID: string, content: string, sessionID?: string) => request(`/api/runs/${encodeURIComponent(runID)}/prompts`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  }, { sessionID, runID }),
  removeRunMessage: (runID: string, promptID: string, sessionID?: string) => request(`/api/runs/${encodeURIComponent(runID)}/prompts/${encodeURIComponent(promptID)}`, {
    method: 'DELETE',
  }, { sessionID, runID }),
  steerRunMessage: (runID: string, promptID: string, steer: boolean, sessionID?: string) => request(`/api/runs/${encodeURIComponent(runID)}/prompts/${encodeURIComponent(promptID)}/steer`, {
    method: 'POST',
    body: JSON.stringify({ steer }),
  }, { sessionID, runID }),
  moveRunMessage: (runID: string, promptID: string, direction: 'up' | 'down', sessionID?: string) => request(`/api/runs/${encodeURIComponent(runID)}/prompts/${encodeURIComponent(promptID)}/move`, {
    method: 'POST',
    body: JSON.stringify({ direction }),
  }, { sessionID, runID }),
  activeRuns: () => request<{ runs: ActiveRunDescriptor[] }>('/api/runs/active'),
}

const streamReconnectLimit = 5

export interface LifecycleStreamOptions {
  signal?: AbortSignal
  onReconnect?: () => void | Promise<void>
}

function waitForStreamReconnect(attempt: number, signal?: AbortSignal): Promise<boolean> {
  const delay = Math.min(200 * (2 ** attempt), 2000)
  if (signal?.aborted) return Promise.resolve(false)
  return new Promise((resolve) => {
    let timer = 0
    const finish = (shouldReconnect: boolean) => {
      window.clearTimeout(timer)
      signal?.removeEventListener('abort', onAbort)
      resolve(shouldReconnect)
    }
    const onAbort = () => finish(false)
    timer = window.setTimeout(() => finish(true), delay)
    signal?.addEventListener('abort', onAbort, { once: true })
    if (signal?.aborted) onAbort()
  })
}

export interface RunStreamOptions {
  signal?: AbortSignal
  sessionID?: string
}

export async function streamRun(runID: string, onEvent: (event: RunEvent) => void | Promise<void>, options: RunStreamOptions = {}): Promise<void> {
  let after = 0
  let reconnects = 0

  if (options.signal?.aborted) {
    if (options.sessionID && frontendProtocolLogger.isEnabled(options.sessionID)) {
      frontendProtocolLogger.log({ sessionID: options.sessionID, source: 'stream.run', kind: 'closed', runID, reason: 'aborted', after_cursor: after })
    }
    return
  }

  while (!options.signal?.aborted) {
    const logStream = (kind: string, fields: Record<string, unknown> = {}, payload?: Record<string, unknown>) => {
      if (!options.sessionID || !frontendProtocolLogger.isEnabled(options.sessionID)) return
      frontendProtocolLogger.log({
        sessionID: options.sessionID,
        source: 'stream.run',
        kind,
        runID,
        ...(payload ? protocolLogIdentity(payload, runID) : {}),
        after_cursor: after,
        ...fields,
      })
    }
    logStream('connect', { reconnect_attempt: reconnects })
    let lastFrame = ''
    const query = after > 0 ? `?after=${encodeURIComponent(String(after))}` : ''
    const streamURL = `/api/runs/${encodeURIComponent(runID)}/events${query}`
    const streamRequestID = `http-${++requestSequence}`
    const logHTTP = (kind: string, fields: Record<string, unknown> = {}) => {
      if (!options.sessionID || !frontendProtocolLogger.isEnabled(options.sessionID)) return
      frontendProtocolLogger.log({
        sessionID: options.sessionID,
        source: 'http',
        kind,
        runID,
        request_id: streamRequestID,
        method: 'GET',
        url: streamURL,
        ...fields,
      })
    }
    logHTTP('request.start', { body: null, after_cursor: after })
    try {
      const response = await fetch(streamURL, {
        headers: { Authorization: `Bearer ${token}` },
        signal: options.signal,
      })
      if (!response.ok || !response.body) {
        const body = response.body ? await response.text() : ''
        logHTTP('response', { status: response.status, body })
        logHTTP('error', { status: response.status, body, code: 'stream_failed' })
        logStream('error', { status: response.status, body, message: `Unable to connect to run events (${response.status})` })
        throw new APIError(response.status, 'stream_failed', `Unable to connect to run events (${response.status})`)
      }

      logHTTP('response', { status: response.status, body: null, stream: true })
      logStream('connected', { status: response.status, reconnect_attempt: reconnects })
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
          lastFrame = frame
          buffer = buffer.slice(boundary + 2)
          const lines = frame.split('\n')
          const sseID = lines.find((line) => line.startsWith('id:'))?.slice(3).trim() ?? ''
          const sequence = Number(sseID)
          const data = lines
            .filter((line) => line.startsWith('data:'))
            .map((line) => line.slice(5).trimStart())
            .join('\n')
          if (data) {
            const event = JSON.parse(data) as RunEvent
            if (options.sessionID && frontendProtocolLogger.isEnabled(options.sessionID)) {
              const payload = event as unknown as Record<string, unknown>
              logStream('event', {
                event: event,
                raw_frame: frame,
                sse_id: sseID || undefined,
                stream_sequence: Number.isSafeInteger(sequence) ? sequence : undefined,
              }, payload)
            }
            await onEvent(event)
            if (Number.isSafeInteger(sequence) && sequence > after) after = sequence
            reconnects = 0
            if (event.type === 'run.settled') settled = true
          }
          boundary = buffer.indexOf('\n\n')
        }
        if (done) break
      }
      logStream('closed', { settled, last_frame: lastFrame || undefined })
      if (settled) return
    } catch (reason) {
      if (options.signal?.aborted) {
        logStream('closed', { reason: 'aborted', last_frame: lastFrame || undefined })
        return
      }
      if (!(reason instanceof APIError)) logHTTP('error', { error: reason })
      logStream('error', { error: reason, raw_frame: lastFrame || undefined })
      if (reason instanceof APIError) throw reason
    }

    if (reconnects >= streamReconnectLimit) {
      logStream('error', { code: 'stream_interrupted', reconnect_attempt: reconnects })
      throw new APIError(0, 'stream_interrupted', 'The run event stream was interrupted. Refresh the session to view saved results.')
    }
    logStream('reconnect', { reconnect_attempt: reconnects + 1 })
    if (!await waitForStreamReconnect(reconnects, options.signal)) {
      logStream('closed', { reason: options.signal?.aborted ? 'aborted' : 'closed' })
      return
    }
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
          const sessionID = event.session_id
            ?? (typeof event.session === 'string' ? event.session : event.session?.id)
            ?? event.metadata?.id
            ?? event.session_metadata?.id
            ?? ''
          if (sessionID && frontendProtocolLogger.isEnabled(sessionID)) {
            const payload = event as unknown as Record<string, unknown>
            frontendProtocolLogger.log({
              sessionID,
              source: 'stream.lifecycle',
              kind: 'event',
              ...protocolLogIdentity(payload),
              event: event,
              raw_frame: frame,
            })
          }
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
