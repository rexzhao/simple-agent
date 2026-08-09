// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api'
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
      .mockResolvedValueOnce(new Response('image-data', { status: 200, headers: { 'Content-Type': 'image/png' } }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ error: { code: 'bad_request', message: 'nope' } }), { status: 400 }))
    vi.stubGlobal('fetch', fetchMock)

    await api.sessionImage('session-1', 'hash')
    await expect(api.sessionImage('session-1', 'hash')).rejects.toThrow('nope')

    const sessionRecords = records()
    expect(sessionRecords.filter((record) => record.kind === 'request.start')).toHaveLength(2)
    expect(sessionRecords.find((record) => record.kind === 'request.start')?.url).toContain('/api/sessions/session-1/images/hash')
    expect(sessionRecords.find((record) => record.kind === 'response' && record.status === 200)?.body).toEqual({ blob_type: 'image/png', size: 10 })
    expect(sessionRecords.find((record) => record.kind === 'response' && record.status === 400)?.body).toBe(JSON.stringify({ error: { code: 'bad_request', message: 'nope' } }))
    expect(sessionRecords.find((record) => record.kind === 'error' && record.status === 400)?.error).toMatchObject({ code: 'bad_request', message: 'nope' })
  })

})
