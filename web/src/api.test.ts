// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './api'

describe('HTTP compatibility reads', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('keeps bootstrap as the only ordinary application read', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' }), { status: 200 }))
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.bootstrap()).resolves.toEqual({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' })

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/bootstrap')
    expect(init.method).toBeUndefined()
  })
})

