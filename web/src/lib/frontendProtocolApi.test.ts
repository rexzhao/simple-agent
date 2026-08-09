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
      .mockResolvedValueOnce(new Response(JSON.stringify({ id: 'session-1', revision: '3' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ error: { code: 'bad_request', message: 'nope' } }), { status: 400 }))
    vi.stubGlobal('fetch', fetchMock)

    await api.session('session-1')
    await expect(api.session('session-1')).rejects.toThrow('nope')

    const sessionRecords = records()
    expect(sessionRecords.filter((record) => record.kind === 'request.start')).toHaveLength(2)
    expect(sessionRecords.find((record) => record.kind === 'request.start')?.url).toContain('/api/sessions/session-1')
    expect(sessionRecords.find((record) => record.kind === 'response' && record.status === 200)?.body).toEqual({ id: 'session-1', revision: '3' })
    expect(sessionRecords.find((record) => record.kind === 'error' && record.status === 400)?.body).toEqual({ error: { code: 'bad_request', message: 'nope' } })
  })

})
