import type { Bootstrap } from './types'
import type { RunImageReference } from './commands/runCommands'
import { frontendProtocolLogger, protocolLogIdentity } from './lib/frontendProtocolLogger'

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
  uploadSessionImage: async (sessionID: string, image: Blob, signal?: AbortSignal): Promise<RunImageReference> => {
    const response = await fetch(`/api/sessions/${encodeURIComponent(sessionID)}/images`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': image.type },
      body: image,
      signal,
    })
    if (!response.ok) {
      let code = 'request_failed'
      let message = `Image upload failed (${response.status})`
      try {
        const payload = await response.json() as { error?: { code?: string; message?: string } }
        code = payload.error?.code ?? code
        message = payload.error?.message ?? message
      } catch { /* retain safe fallback */ }
      throw new APIError(response.status, code, message)
    }
    const value = await response.json() as Partial<RunImageReference>
    if (typeof value.hash !== 'string' || !/^[0-9a-f]{64}$/u.test(value.hash)
      || typeof value.media_type !== 'string' || !['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(value.media_type)
      || !Number.isSafeInteger(value.size_bytes) || Number(value.size_bytes) <= 0 || Number(value.size_bytes) > 4 * 1024 * 1024) {
      throw new APIError(response.status, 'invalid_response', 'Image upload returned an invalid reference')
    }
    return { hash: value.hash, media_type: value.media_type, size_bytes: Number(value.size_bytes) }
  },
  sessionImage: async (sessionID: string, hash: string, signal?: AbortSignal): Promise<Blob> => {
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
        signal,
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
}
