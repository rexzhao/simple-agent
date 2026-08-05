// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, streamLifecycle } from './api'

describe('streamLifecycle', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('reconciles once when a lifecycle connection disconnects', async () => {
    vi.useFakeTimers()
    const controller = new AbortController()
    const reconnectingFetch = vi.fn()
    const firstPayload = new TextEncoder().encode(
      ': connected\nretry: 3000\n\nevent: session.created\ndata: {"type":"session.created","session_id":"child-1"}\n\n',
    )
    reconnectingFetch
      .mockResolvedValueOnce(new Response(new ReadableStream({
        start(stream) {
          stream.enqueue(firstPayload.slice(0, 24))
          stream.enqueue(firstPayload.slice(24))
          stream.close()
        },
      }), { status: 200 }))
      .mockImplementation((_input: RequestInfo | URL, init?: RequestInit) => new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true })
      }))
    vi.stubGlobal('fetch', reconnectingFetch)
    const onReconnect = vi.fn().mockResolvedValue(undefined)
    const onEvent = vi.fn()

    const stream = streamLifecycle(onEvent, { signal: controller.signal, onReconnect })
    await vi.waitFor(() => expect(onReconnect).toHaveBeenCalledTimes(1))
    expect(onEvent).toHaveBeenCalledWith(expect.objectContaining({ type: 'session.created', session_id: 'child-1' }))
    expect(reconnectingFetch).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(200)
    await vi.waitFor(() => expect(reconnectingFetch).toHaveBeenCalledTimes(2))
    expect(onReconnect).toHaveBeenCalledTimes(1)

    controller.abort()
    await stream
  })

  it('posts configured session creation fields as one typed options object', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: 'session-new' }), { status: 201 }))
    vi.stubGlobal('fetch', fetchMock)

    await api.createSession({
      projectID: 'project/1',
      provider: 'fake',
      modelProfile: 'precise',
      reasoningLevel: 'high',
      fullAccess: true,
      cwd: '/workspace/src',
      configPath: '/config/sai.yaml',
    })

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/projects/project%2F1/sessions')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({
      cwd: '/workspace/src',
      config_path: '/config/sai.yaml',
      provider: 'fake',
      model_profile: 'precise',
      reasoning_level: 'high',
      full_access: true,
    })
  })
})

