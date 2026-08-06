import { isRFC3339Timestamp } from '../protocol/datetime'
import type { BlobDescriptor } from '../protocol/types'
import { SyncReadError } from './errors'

export interface BlobClientOptions {
  capabilityToken?: () => string
  fetcher?: typeof globalThis.fetch
  maxBytes?: number
  now?: () => Date
  baseURL?: string
}

export interface BlobPayload {
  readonly bytes: ArrayBuffer
  readonly contentType: string
  readonly etag: string
}

function defaultCapabilityToken(): string {
  if (typeof window === 'undefined') return ''
  const hash = new URLSearchParams(window.location.hash.slice(1))
  const hashToken = hash.get('token')
  if (hashToken) {
    window.sessionStorage.setItem('sai-capability-token', hashToken)
    window.history.replaceState(null, '', window.location.pathname + window.location.search)
    return hashToken
  }
  return window.sessionStorage.getItem('sai-capability-token') ?? ''
}

function responseType(value: string | null): string {
  return value?.split(';', 1)[0]?.trim().toLowerCase() ?? ''
}

function descriptorContentType(value: string): string {
  return value.split(';', 1)[0].trim().toLowerCase()
}

function normalizeURL(value: string, baseURL?: string): URL {
  try {
    return new URL(value, baseURL ?? (typeof window !== 'undefined' ? window.location.href : undefined))
  } catch {
    throw new SyncReadError('blob_invalid', 'blob URL is invalid')
  }
}

function sameOrigin(url: URL, baseURL?: string): boolean {
  if (typeof window !== 'undefined') return url.origin === window.location.origin
  // Node/Vitest has no ambient browser origin.  When a base URL is supplied,
  // use that final URL as the origin authority rather than accidentally
  // treating every absolute descriptor URL as same-origin.
  if (baseURL) {
    try { return url.origin === new URL(baseURL).origin } catch { return false }
  }
  // Without a browser location or an explicit base, there is no trustworthy
  // origin against which to authorize an absolute data URL.
  return false
}

async function cancelResponseBody(response: Response): Promise<void> {
  try { await response.body?.cancel() } catch { /* best effort */ }
}

function validateDescriptor(descriptor: BlobDescriptor, maxBytes: number, now: Date): void {
  if (!descriptor || typeof descriptor !== 'object') throw new SyncReadError('blob_invalid', 'blob descriptor is invalid')
  if (typeof descriptor.id !== 'string' || descriptor.id.trim() === '') throw new SyncReadError('blob_invalid', 'blob id is missing')
  if (typeof descriptor.url !== 'string' || descriptor.url.trim() === '') throw new SyncReadError('blob_invalid', 'blob URL is missing')
  if (!Number.isSafeInteger(descriptor.size) || descriptor.size < 0 || descriptor.size > maxBytes) {
    throw new SyncReadError('blob_size', 'blob size is outside the allowed limit')
  }
  if (!isRFC3339Timestamp(descriptor.expires_at)) throw new SyncReadError('blob_expired', 'blob expiry is invalid')
  const expiresAt = Date.parse(descriptor.expires_at)
  if (!Number.isFinite(expiresAt) || now.getTime() >= expiresAt) throw new SyncReadError('blob_expired', 'blob has expired')
  // Keep descriptor validation aligned with protocol.ValidateBlobDescriptor:
  // these are required opaque strings, not a frontend-imposed hash/ETag or
  // MIME grammar. Integrity and response-header comparisons below still
  // verify the fetched bytes against the supplied values.
  if (typeof descriptor.sha256 !== 'string' || descriptor.sha256.trim() === '') throw new SyncReadError('blob_invalid', 'blob hash is missing')
  if (typeof descriptor.etag !== 'string' || descriptor.etag.trim() === '') throw new SyncReadError('blob_invalid', 'blob etag is missing')
  if (typeof descriptor.content_type !== 'string' || descriptor.content_type.trim() === '') throw new SyncReadError('blob_invalid', 'blob content type is missing')
  if (/[\u0000-\u001f\u007f]/.test(descriptor.url)) throw new SyncReadError('blob_invalid', 'blob URL is invalid')
}

async function sha256Hex(bytes: ArrayBuffer, signal?: AbortSignal): Promise<string> {
  if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
  const subtle = globalThis.crypto?.subtle
  if (!subtle) throw new SyncReadError('blob_hash', 'Web Crypto is unavailable')
  let digest: ArrayBuffer
  try {
    digest = await subtle.digest('SHA-256', bytes)
  } catch {
    throw new SyncReadError('blob_hash', 'blob hash could not be calculated')
  }
  if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, '0')).join('')
}

async function readBody(response: Response, expectedSize: number, maxBytes: number, signal?: AbortSignal, expired?: () => boolean): Promise<ArrayBuffer> {
  if (response.body && typeof response.body.getReader === 'function') {
    const reader = response.body.getReader()
    const chunks: Uint8Array[] = []
    let total = 0
    let cancelled = false
    const cancel = async (): Promise<void> => {
      if (cancelled) return
      cancelled = true
      try { await reader.cancel() } catch { /* best effort */ }
    }
    const abortListener = () => { void cancel() }
    signal?.addEventListener('abort', abortListener, { once: true })
    try {
      while (true) {
        if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
        if (expired?.()) throw new SyncReadError('blob_expired', 'blob expired while downloading')
        const part = await reader.read()
        if (part.done) break
        const chunk = part.value
        total += chunk.byteLength
        if (total > expectedSize || total > maxBytes) {
          await cancel()
          throw new SyncReadError('blob_size', 'blob body exceeds its descriptor')
        }
        if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
        if (expired?.()) throw new SyncReadError('blob_expired', 'blob expired while downloading')
        chunks.push(chunk)
      }
      if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
      if (expired?.()) throw new SyncReadError('blob_expired', 'blob expired while downloading')
      if (total !== expectedSize) {
        await cancel()
        throw new SyncReadError('blob_size', 'blob body does not match its descriptor')
      }
    } catch (reason) {
      await cancel()
      if (reason instanceof SyncReadError) throw reason
      if (signal?.aborted || (typeof DOMException !== 'undefined' && reason instanceof DOMException && reason.name === 'AbortError')) {
        throw new SyncReadError('aborted', 'blob request was aborted')
      }
      throw new SyncReadError('transport', 'blob response could not be read')
    } finally {
      signal?.removeEventListener('abort', abortListener)
      try { reader.releaseLock() } catch { /* best effort */ }
    }
    const bytes = new Uint8Array(total)
    let offset = 0
    for (const chunk of chunks) {
      bytes.set(chunk, offset)
      offset += chunk.byteLength
    }
    return bytes.buffer
  }

  let bytes: ArrayBuffer
  try {
    bytes = await response.arrayBuffer()
  } catch {
    await cancelResponseBody(response)
    if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
    throw new SyncReadError('transport', 'blob response could not be read')
  }
  if (signal?.aborted) {
    await cancelResponseBody(response)
    throw new SyncReadError('aborted', 'blob request was aborted')
  }
  if (bytes.byteLength !== expectedSize || bytes.byteLength > maxBytes) {
    await cancelResponseBody(response)
    throw new SyncReadError('blob_size', 'blob body does not match its descriptor')
  }
  return bytes
}

/**
 * HTTP data-plane client for immutable protocol blobs. It intentionally
 * returns bytes only to the sync runtime; no page-level caller needs to know
 * whether a snapshot was inline or fetched from a blob.
 */
export class BlobClient {
  private readonly capabilityToken: () => string
  private readonly fetcher: typeof globalThis.fetch
  private readonly maxBytes: number
  private readonly now: () => Date
  private readonly baseURL?: string

  constructor(options: BlobClientOptions = {}) {
    this.capabilityToken = options.capabilityToken ?? defaultCapabilityToken
    this.fetcher = options.fetcher ?? globalThis.fetch.bind(globalThis)
    this.maxBytes = options.maxBytes ?? 16 * 1024 * 1024
    this.now = options.now ?? (() => new Date())
    this.baseURL = options.baseURL
    if (!Number.isSafeInteger(this.maxBytes) || this.maxBytes <= 0) throw new Error('blob maxBytes must be positive')
  }

  async get(descriptor: BlobDescriptor, options: { signal?: AbortSignal } = {}): Promise<BlobPayload> {
    const signal = options.signal
    if (signal?.aborted) throw new SyncReadError('aborted', 'blob request was aborted')
    validateDescriptor(descriptor, this.maxBytes, this.now())
    const url = normalizeURL(descriptor.url, this.baseURL)
    if (url.protocol !== 'http:' && url.protocol !== 'https:') throw new SyncReadError('blob_invalid', 'blob URL protocol is invalid')
    if (url.username || url.password) throw new SyncReadError('blob_auth', 'blob URL credentials are not allowed')
    if (!sameOrigin(url, this.baseURL)) throw new SyncReadError('blob_auth', 'blob origin is not allowed')
    const capability = this.capabilityToken()
    if (!capability) throw new SyncReadError('blob_auth', 'capability token is unavailable')

    let response: Response
    try {
      response = await this.fetcher(url.toString(), {
        method: 'GET',
        headers: { Authorization: `Bearer ${capability}` },
        signal,
      })
    } catch (reason) {
      if (signal?.aborted || (typeof DOMException !== 'undefined' && reason instanceof DOMException && reason.name === 'AbortError')) {
        throw new SyncReadError('aborted', 'blob request was aborted')
      }
      throw new SyncReadError('transport', 'blob request failed')
    }
    if (!response.ok) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_auth', 'blob request was rejected')
    }
    if (response.url) {
      let finalURL: URL
      try { finalURL = new URL(response.url, url.toString()) } catch {
        await cancelResponseBody(response)
        throw new SyncReadError('blob_auth', 'blob final URL is invalid')
      }
      if (!sameOrigin(finalURL, this.baseURL)) {
        await cancelResponseBody(response)
        throw new SyncReadError('blob_auth', 'blob final origin is not allowed')
      }
    }

    const contentLength = response.headers.get('Content-Length')
    if (contentLength === null || !/^\d+$/.test(contentLength) || Number(contentLength) !== descriptor.size) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_size', 'blob content length does not match its descriptor')
    }
    const contentType = responseType(response.headers.get('Content-Type'))
    if (contentType !== descriptorContentType(descriptor.content_type)) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_type', 'blob content type does not match its descriptor')
    }
    if (response.headers.get('ETag') !== descriptor.etag) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_etag', 'blob etag does not match its descriptor')
    }
    const responseHash = response.headers.get('X-Content-SHA256')
    if (!responseHash || !/^[0-9a-fA-F]{64}$/.test(responseHash) || responseHash.toLowerCase() !== descriptor.sha256.toLowerCase()) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_hash', 'blob hash header does not match its descriptor')
    }

    const expiresAt = Date.parse(descriptor.expires_at)
    const bytes = await readBody(response, descriptor.size, this.maxBytes, signal, () => this.now().getTime() >= expiresAt)
    if (this.now().getTime() >= Date.parse(descriptor.expires_at)) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_expired', 'blob expired while downloading')
    }
    let hash: string
    try {
      hash = await sha256Hex(bytes, signal)
    } catch (reason) {
      await cancelResponseBody(response)
      throw reason
    }
    if (this.now().getTime() >= Date.parse(descriptor.expires_at)) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_expired', 'blob expired while validating')
    }
    if (hash.toLowerCase() !== descriptor.sha256.toLowerCase()) {
      await cancelResponseBody(response)
      throw new SyncReadError('blob_hash', 'blob hash does not match its descriptor')
    }
    return { bytes, contentType, etag: descriptor.etag }
  }

  async getJSON(descriptor: BlobDescriptor, options: { signal?: AbortSignal } = {}): Promise<unknown> {
    const payload = await this.get(descriptor, options)
    if (payload.contentType !== 'application/json') throw new SyncReadError('blob_type', 'snapshot blob must be JSON')
    try {
      return JSON.parse(new TextDecoder().decode(payload.bytes)) as unknown
    } catch {
      throw new SyncReadError('blob_invalid', 'snapshot blob JSON is invalid')
    }
  }
}
