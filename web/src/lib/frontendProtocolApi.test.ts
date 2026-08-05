// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, streamRun } from '../api'
import { frontendProtocolLogger } from './frontendProtocolLogger'

function records(sessionID = 'session-1') {
  return frontendProtocolLogger.getSnapshot(sessionID).records
}

describe('frontend protocol HTTP and stream instrumentation', () => {
  afterEach(() => {
    frontendProtocolLogger.resetForTesting()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('records HTTP request, response, and error bodies for a session', async () => {
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.setEnabled('session-1', true)
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ id: 'session-1', revision: '3' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ error: { code: 'bad_request', message: 'nope' } }), { status: 400 }))
    vi.stubGlobal('fetch', fetchMock)

    await api.session('session-1')
    await expect(api.snapshot('session-1')).rejects.toThrow('nope')

    const sessionRecords = records()
    expect(sessionRecords.filter((record) => record.kind === 'request.start')).toHaveLength(2)
    expect(sessionRecords.find((record) => record.kind === 'request.start')?.url).toContain('/api/sessions/session-1')
    expect(sessionRecords.find((record) => record.kind === 'response' && record.status === 200)?.body).toEqual({ id: 'session-1', revision: '3' })
    expect(sessionRecords.find((record) => record.kind === 'error' && record.status === 400)?.body).toEqual({ error: { code: 'bad_request', message: 'nope' } })
  })

  it('records run stream connection, cursor, raw event, and close lifecycle', async () => {
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.setEnabled('session-1', true)
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode('id: 7\ndata: {"type":"run.settled","session_id":"session-1","run_id":"run-1","turn_id":"turn-1","seq":7,"revision":"9"}\n\n'))
        controller.close()
      },
    })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(stream, { status: 200 })))

    await streamRun('run-1', vi.fn(), { sessionID: 'session-1' })

    const event = records().find((record) => record.source === 'stream.run' && record.kind === 'event')
    expect(event).toMatchObject({
      run_id: 'run-1',
      turn_id: 'turn-1',
      event_type: 'run.settled',
      revision: '9',
      stream_sequence: 7,
      sse_id: '7',
      after_cursor: 0,
    })
    expect(event?.raw_frame).toContain('id: 7')
    expect(records().some((record) => record.kind === 'connect')).toBe(true)
    expect(records().some((record) => record.kind === 'connected')).toBe(true)
    expect(records().some((record) => record.kind === 'closed')).toBe(true)
  })

  it('records an aborted stream as a closed stream', async () => {
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.setEnabled('session-1', true)
    const controller = new AbortController()
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true })
    })))

    const stream = streamRun('run-1', vi.fn(), { sessionID: 'session-1', signal: controller.signal })
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    controller.abort()
    await stream

    expect(records().find((record) => record.kind === 'closed')).toMatchObject({ reason: 'aborted', run_id: 'run-1' })
  })
})
