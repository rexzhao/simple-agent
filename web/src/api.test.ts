// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './api'

describe('HTTP compatibility reads', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
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

