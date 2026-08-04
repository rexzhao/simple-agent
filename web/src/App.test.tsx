// @vitest-environment jsdom
import { act, render, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App'

const mocks = vi.hoisted(() => {
  const session = {
    id: 'session-1',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    display_name: 'session-1',
    created_by: 'user',
    root_session_id: 'session-1',
    spawn_depth: 0,
    archived: false,
    last_used_at: '2026-01-01T00:00:00Z',
    provider: 'fake',
    model_profile: 'default',
    model_id: 'fake-model',
    project_id: 'project-1',
    created_cwd: '/workspace',
    last_seq: 0,
    full_access: false,
  }
  const api = {
    bootstrap: vi.fn().mockResolvedValue({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' }),
    projects: vi.fn().mockResolvedValue({ projects: [{ id: 'project-1', root: '/workspace', display_name: 'project', archived: false, created_at: '', updated_at: '' }] }),
    activeRuns: vi.fn().mockResolvedValue({ runs: [{ run_id: 'run-1', session_id: 'session-1', turn_id: 'turn-1', started_at: '', status: 'running' }] }),
    sessions: vi.fn().mockResolvedValue({ sessions: [session] }),
    snapshot: vi.fn().mockImplementation(() => new Promise(() => {})),
  }
  const streamLifecycle = vi.fn((_onEvent: unknown, options: { signal?: AbortSignal }) => new Promise<void>((resolve) => {
    options.signal?.addEventListener('abort', () => resolve(), { once: true })
  }))
  return { api, session, streamLifecycle, streamRun: vi.fn().mockResolvedValue(undefined) }
})

vi.mock('./api', () => ({
  api: mocks.api,
  streamLifecycle: mocks.streamLifecycle,
  streamRun: mocks.streamRun,
}))

describe('App lifecycle bootstrap', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
  })

  it('does not poll sessions or active runs while a run remains active', async () => {
    const view = render(<App />)
    await waitFor(() => expect(mocks.api.sessions).toHaveBeenCalledTimes(2))

    vi.useFakeTimers()
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(mocks.api.activeRuns).toHaveBeenCalledTimes(1)
    expect(mocks.api.sessions).toHaveBeenCalledTimes(2)
    view.unmount()
  })
})

