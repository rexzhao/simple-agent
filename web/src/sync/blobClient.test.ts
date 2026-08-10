import { afterEach, describe, expect, it, vi } from 'vitest'
import { BlobClient } from './blobClient'
import { sha256Hex } from '../lib/sha256'
import type { BlobDescriptor } from '../protocol/types'

afterEach(() => {
  vi.unstubAllGlobals()
})

const bytes = new TextEncoder().encode('{"sessions":[]}')

async function digest(): Promise<string> {
  const result = await globalThis.crypto.subtle.digest('SHA-256', bytes)
  return [...new Uint8Array(result)].map((byte) => byte.toString(16).padStart(2, '0')).join('')
}

async function descriptor(overrides: Partial<BlobDescriptor> = {}): Promise<BlobDescriptor> {
  return {
    id: 'blob_1',
    url: '/api/blobs/blob_1',
    content_type: 'application/json',
    size: bytes.byteLength,
    sha256: await digest(),
    etag: '"etag-1"',
    expires_at: '2099-01-01T00:00:00Z',
    ...overrides,
  }
}

function response(body: Uint8Array, headers: Record<string, string> = {}): Response {
  return new Response(body as unknown as BodyInit, { status: 200, headers: { 'Content-Length': String(body.byteLength), 'Content-Type': 'application/json', ETag: '"etag-1"', 'X-Content-SHA256': 'a1179f88a4a67a27ff6c7922fd583ba4fc86888f4882350e58bb15d0158adb76', ...headers } })
}

function streamingResponse(stream: ReadableStream<Uint8Array>, size: number, headers: Record<string, string> = {}): Response {
  return new Response(stream, { status: 200, headers: { 'Content-Length': String(size), 'Content-Type': 'application/json', ETag: '"etag-1"', 'X-Content-SHA256': 'a1179f88a4a67a27ff6c7922fd583ba4fc86888f4882350e58bb15d0158adb76', ...headers } })
}

describe('BlobClient', () => {
  it('authenticates and validates type, size, etag, expiry, and SHA-256 before returning JSON', async () => {
    const calls: { url: string; init?: RequestInit }[] = []
    const client = new BlobClient({
      capabilityToken: () => 'capability',
      fetcher: async (input, init) => {
        calls.push({ url: String(input), init })
        return response(bytes)
      },
      now: () => new Date('2025-01-01T00:00:00Z'),
      baseURL: 'http://example.test',
    })
    await expect(client.getJSON(await descriptor())).resolves.toEqual({ sessions: [] })
    expect(calls[0].init?.headers).toEqual({ Authorization: 'Bearer capability' })
    expect(calls[0].url).toContain('/api/blobs/blob_1')

    await expect(client.get(await descriptor({ etag: '"wrong"' }))).rejects.toMatchObject({ code: 'blob_etag' })
    await expect(client.get(await descriptor({ sha256: '00'.repeat(32) }))).rejects.toMatchObject({ code: 'blob_hash' })
    await expect(client.get(await descriptor({ expires_at: '2020-01-01T00:00:00Z' }))).rejects.toMatchObject({ code: 'blob_expired' })
    await expect(client.get(await descriptor({ size: bytes.byteLength + 1 }))).rejects.toMatchObject({ code: 'blob_size' })
    await expect(client.get(await descriptor({ size: Number.MAX_SAFE_INTEGER + 1 }))).rejects.toMatchObject({ code: 'blob_size' })
  })

  it('rejects a body over the hard limit while streaming and honors AbortSignal', async () => {
    const tooLarge = new Uint8Array(20)
    const client = new BlobClient({
      capabilityToken: () => 'capability',
      maxBytes: 8,
      baseURL: 'http://example.test',
      fetcher: async () => response(tooLarge, { 'Content-Length': '8' }),
    })
    await expect(client.get(await descriptor({ size: 8 }))).rejects.toMatchObject({ code: 'blob_size' })

    const controller = new AbortController()
    controller.abort()
    const abortedClient = new BlobClient({ capabilityToken: () => 'capability', baseURL: 'http://example.test', fetcher: async (_input, init) => {
      expect(init?.signal).toBe(controller.signal)
      throw new DOMException('aborted', 'AbortError')
    } })
    await expect(abortedClient.get(await descriptor(), { signal: controller.signal })).rejects.toMatchObject({ code: 'aborted' })
  })

  it('accepts opaque protocol descriptor strings and rejects forged headers, invalid origins, and redirect escapes', async () => {
    const good = await descriptor()
    const calls: string[] = []
    const client = new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async (input) => { calls.push(String(input)); return response(bytes) },
    })
    // The Go wire schema treats sha256/etag/content_type as opaque required
    // strings. An arbitrary hash is accepted at the descriptor boundary and
    // rejected only by the fetched-byte integrity check.
    await expect(client.get({ ...good, sha256: 'abc' })).rejects.toMatchObject({ code: 'blob_hash' })
    await expect(client.get({ ...good, sha256: ` ${good.sha256}` })).rejects.toMatchObject({ code: 'blob_hash' })
    await expect(new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async () => response(bytes, { 'X-Content-SHA256': '0'.repeat(64) }),
    }).get(good)).rejects.toMatchObject({ code: 'blob_hash' })
    await expect(new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async () => response(bytes, { 'X-Content-SHA256': '' }),
    }).get(good)).rejects.toMatchObject({ code: 'blob_hash' })
    calls.length = 0
    await expect(client.get({ ...good, url: 'http://evil.test/blob' })).rejects.toMatchObject({ code: 'blob_auth' })

    const redirected = response(bytes)
    Object.defineProperty(redirected, 'url', { value: 'http://evil.test/final' })
    await expect(new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async () => redirected,
    }).get(good)).rejects.toMatchObject({ code: 'blob_auth' })
    expect(calls).toHaveLength(0)
  })

  it('cancels a response body on non-success before inspecting its headers', async () => {
    const good = await descriptor()
    let cancelled = false
    const body = new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(bytes) },
      cancel() { cancelled = true },
    })
    const rejected = new Response(body, { status: 401 })
    const client = new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async () => rejected,
    })
    await expect(client.get(good)).rejects.toMatchObject({ code: 'blob_auth' })
    expect(cancelled).toBe(true)
  })

  it('cancels and releases streaming readers on expiry, abort, and chunk overrun', async () => {
    const good = await descriptor()
    let expiryCalls = 0
    let expiredCancelled = false
    const expiredStream = new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(bytes) },
      cancel() { expiredCancelled = true },
    })
    const expiringClient = new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      now: () => new Date(expiryCalls++ < 2 ? '2025-01-01T00:00:00Z' : '2100-01-01T00:00:00Z'),
      fetcher: async () => streamingResponse(expiredStream, bytes.byteLength),
    })
    await expect(expiringClient.get(good)).rejects.toMatchObject({ code: 'blob_expired' })
    expect(expiredCancelled).toBe(true)

    const controller = new AbortController()
    let abortCancelled = false
    let releasePull: (() => void) | undefined
    const abortStream = new ReadableStream<Uint8Array>({
      pull() { return new Promise<void>((resolve) => { releasePull = resolve }) },
      cancel() { abortCancelled = true; releasePull?.() },
    })
    const abortClient = new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async () => streamingResponse(abortStream, bytes.byteLength),
    })
    const aborting = abortClient.get(good, { signal: controller.signal })
    await Promise.resolve()
    controller.abort()
    await expect(aborting).rejects.toMatchObject({ code: 'aborted' })
    expect(abortCancelled).toBe(true)

    let overrunCancelled = false
    const overrunStream = new ReadableStream<Uint8Array>({
      start(streamController) { streamController.enqueue(new Uint8Array(20)) },
      cancel() { overrunCancelled = true },
    })
    const overrunClient = new BlobClient({
      capabilityToken: () => 'capability',
      maxBytes: 32,
      baseURL: 'http://example.test',
      fetcher: async () => streamingResponse(overrunStream, 8, { 'Content-Length': '8' }),
    })
    await expect(overrunClient.get({ ...good, size: 8 })).rejects.toMatchObject({ code: 'blob_size' })
    expect(overrunCancelled).toBe(true)

    let shortCancelled = 0
    let shortReleased = 0
    const shortResponse = {
      ok: true,
      url: 'http://example.test/api/blob_1',
      headers: new Headers({ 'Content-Length': '8', 'Content-Type': 'application/json', ETag: '"etag-1"', 'X-Content-SHA256': good.sha256 }),
      body: {
        getReader: () => ({
          read: async () => ({ done: true, value: undefined }),
          cancel: async () => { shortCancelled += 1 },
          releaseLock: () => { shortReleased += 1 },
        }),
      },
    } as unknown as Response
    const shortClient = new BlobClient({ capabilityToken: () => 'capability', baseURL: 'http://example.test', fetcher: async () => shortResponse })
    await expect(shortClient.get({ ...good, size: 8 })).rejects.toMatchObject({ code: 'blob_size' })
    expect(shortCancelled).toBe(1)
    expect(shortReleased).toBe(1)
  })

  it('verifies integrity without Web Crypto when the page is not a secure context', async () => {
    // http pages on non-loopback addresses get no crypto.subtle; the client
    // must still validate blob bytes instead of failing the whole sync read.
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) })
    const good: BlobDescriptor = {
      id: 'blob_1',
      url: '/api/blobs/blob_1',
      content_type: 'application/json',
      size: bytes.byteLength,
      sha256: sha256Hex(bytes),
      etag: '"etag-1"',
      expires_at: '2099-01-01T00:00:00Z',
    }
    const client = new BlobClient({
      capabilityToken: () => 'capability',
      baseURL: 'http://example.test',
      fetcher: async () => response(bytes),
    })
    await expect(client.getJSON(good)).resolves.toEqual({ sessions: [] })
    await expect(client.get({ ...good, sha256: '00'.repeat(32) })).rejects.toMatchObject({ code: 'blob_hash' })
  })
})
