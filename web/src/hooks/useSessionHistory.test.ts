// @vitest-environment jsdom
import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api'
import type { ItemsPage, Session, SessionSnapshot } from '../types'
import { useSessionHistory } from './useSessionHistory'

vi.mock('../api', () => ({
  api: {
    snapshot: vi.fn(),
    session: vi.fn(),
    items: vi.fn(),
  },
  APIError: class APIError extends Error {
    status: number
    code: string
    constructor(status: number, code: string, message: string) {
      super(message)
      this.status = status
      this.code = code
    }
  },
}))
const mocked = vi.mocked(api)
const session = (id: string): Session => ({ id, project_id: 'project', display_name: id, last_seq: 0 } as Session)
const snapshotFor = (id: string, revision: string, seq: number): SessionSnapshot => ({
  session_id: id,
  revision,
  session: session(id),
  history: {
    items: [{ id: `item-${seq}`, seq } as never],
    oldest_seq: seq,
    newest_seq: seq,
    has_more_before: seq > 1,
    has_more_after: false,
  },
})
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: unknown) => void; const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail }); return { promise, resolve, reject } }

describe('useSessionHistory', () => {
  beforeEach(() => vi.clearAllMocks())

  it('loads session detail and items page via snapshot', async () => {
    mocked.snapshot.mockResolvedValue(snapshotFor('a', '10', 10))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn((r) => console.error('onError:', r))
    const { result } = renderHook(() => useSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('a'), { timeout: 3000 })
    expect(result.current.itemsPage?.oldest_seq).toBe(10)
  })

  it('ignores a stale response after session selection changes', async () => {
    const aSnapshot = deferred<SessionSnapshot>()
    mocked.snapshot.mockImplementation((id) => id === 'a' ? aSnapshot.promise : Promise.resolve(snapshotFor('b', '20', 20)))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result, rerender } = renderHook(({ id }) => useSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
    rerender({ id: 'b' })
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('b'))
    await act(async () => { aSnapshot.resolve(snapshotFor('a', '10', 10)); await Promise.all([aSnapshot.promise]) })
    expect(result.current.sessionDetail?.id).toBe('b')
    expect(result.current.itemsPage?.oldest_seq).toBe(20)
  })

  it('falls back to dual-fetch on 404', async () => {
    // Use a plain object with status=404 since the mock APIError class may not
    // be the same reference as the one imported by useSessionStore.
    const notFoundError = Object.assign(new Error('not found'), { status: 404, code: 'not_found' })
    mocked.snapshot.mockRejectedValue(notFoundError)
    mocked.session.mockResolvedValue(session('a'))
    mocked.items.mockResolvedValue({ items: [{ id: 'item-1', seq: 1 } as never], oldest_seq: 1, newest_seq: 1, has_more_before: false, has_more_after: false } as ItemsPage)
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('a'), { timeout: 3000 })
    expect(result.current.itemsPage?.oldest_seq).toBe(1)
    expect(mocked.session).toHaveBeenCalledWith('a')
    expect(mocked.items).toHaveBeenCalledWith('a')
  })

  it('does not report a rejected request after switching sessions', async () => {
    const staleSnapshot = deferred<SessionSnapshot>()
    mocked.snapshot.mockImplementation((id) => id === 'a' ? staleSnapshot.promise : Promise.resolve(snapshotFor('b', '20', 20)))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { rerender } = renderHook(({ id }) => useSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
    rerender({ id: 'b' })
    await waitFor(() => expect(mocked.snapshot).toHaveBeenCalledWith('b'))
    await act(async () => { staleSnapshot.reject(new Error('stale')); await Promise.resolve() })
    expect(onError).not.toHaveBeenCalled()
  })

  it('loadOlder prepends older items', async () => {
    mocked.snapshot.mockResolvedValue(snapshotFor('a', '10', 10))
    mocked.items.mockResolvedValueOnce({ items: [{ id: 'item-5', seq: 5 } as never], oldest_seq: 5, newest_seq: 5, has_more_before: false, has_more_after: false } as ItemsPage)
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.itemsPage?.oldest_seq).toBe(10))
    await act(async () => { expect(await result.current.loadOlder()).toBe(true) })
    expect(result.current.itemsPage?.items.map((item) => item.seq)).toEqual([5, 10])
  })
})
