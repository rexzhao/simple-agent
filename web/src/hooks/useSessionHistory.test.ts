// @vitest-environment jsdom
import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api'
import type { ItemsPage, Session, SessionSnapshot } from '../types'
import { pendingProjectionEventCap } from '../lib/sessionStore'
import { useSessionStore } from './useSessionStore'
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
function useTestSessionHistory(selectedSessionID: string, loadSessions: (projectID: string, preferredSessionID?: string) => Promise<Session[]>, onError: (reason: unknown) => void) {
  const store = useSessionStore()
  return useSessionHistory(selectedSessionID, loadSessions, onError, store)
}
function useOwnedSessionHistory(selectedSessionID: string, loadSessions: (projectID: string, preferredSessionID?: string) => Promise<Session[]>, onError: (reason: unknown) => void) {
  const store = useSessionStore()
  return { history: useSessionHistory(selectedSessionID, loadSessions, onError, store), store }
}

describe('useSessionHistory', () => {
  beforeEach(() => vi.clearAllMocks())

  it('loads session detail and items page via snapshot', async () => {
    mocked.snapshot.mockResolvedValue(snapshotFor('a', '10', 10))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn((r) => console.error('onError:', r))
    const { result } = renderHook(() => useTestSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('a'), { timeout: 3000 })
    expect(result.current.itemsPage?.oldest_seq).toBe(10)
  })

  it('ignores a stale response after session selection changes', async () => {
    const aSnapshot = deferred<SessionSnapshot>()
    mocked.snapshot.mockImplementation((id) => id === 'a' ? aSnapshot.promise : Promise.resolve(snapshotFor('b', '20', 20)))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result, rerender } = renderHook(({ id }) => useTestSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
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
    const { result } = renderHook(() => useTestSessionHistory('a', loadSessions, onError))
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
    const { rerender } = renderHook(({ id }) => useTestSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
    rerender({ id: 'b' })
    await waitFor(() => expect(mocked.snapshot).toHaveBeenCalledWith('b'))
    await act(async () => { staleSnapshot.reject(new Error('stale')); await Promise.resolve() })
    expect(onError).not.toHaveBeenCalled()
  })

  it('returns the refreshed session for background sessions while skipping the sidebar reload', async () => {
    mocked.snapshot.mockImplementation((id) => Promise.resolve(snapshotFor(id, '10', 10)))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useTestSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('a'))
    loadSessions.mockClear()
    const refreshed = await act(async () => result.current.refreshSession('b'))
    // Run settlement reconciliation depends on this value for
    // non-selected sessions; only the sidebar reload stays selection-scoped.
    expect(refreshed?.id).toBe('b')
    expect(loadSessions).not.toHaveBeenCalled()
  })

  it('suppresses refresh errors for background sessions instead of throwing', async () => {
    mocked.snapshot.mockImplementation((id) => id === 'a' ? Promise.resolve(snapshotFor('a', '10', 10)) : Promise.reject(new Error('boom')))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useTestSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('a'))
    const refreshed = await act(async () => result.current.refreshSession('b'))
    expect(refreshed).toBeNull()
    expect(onError).not.toHaveBeenCalled()
  })

  it('loadOlder prepends older items', async () => {
    mocked.snapshot.mockResolvedValue(snapshotFor('a', '10', 10))
    mocked.items.mockResolvedValueOnce({ items: [{ id: 'item-5', seq: 5 } as never], oldest_seq: 5, newest_seq: 5, has_more_before: false, has_more_after: false } as ItemsPage)
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useTestSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.itemsPage?.oldest_seq).toBe(10))
    await act(async () => { expect(await result.current.loadOlder()).toBe(true) })
    expect(result.current.itemsPage?.items.map((item) => item.seq)).toEqual([5, 10])
  })

  it('reads projection events from the store instance supplied by its owner', async () => {
    mocked.snapshot.mockResolvedValue(snapshotFor('a', '10', 10))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useOwnedSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.history.itemsPage?.oldest_seq).toBe(10))

    act(() => result.current.store.applyProjectionEvent({
      type: 'item.appended',
      session_id: 'a',
      seq: 11,
      revision: '11',
      item_id: 'item-11',
      item: { id: 'item-11', seq: 11, created_at: '', kind: 'message', visibility: 'visible', audience: 'user' },
    }))
    await waitFor(() => expect(result.current.history.itemsPage?.items.map((item) => item.seq)).toEqual([10, 11]))
  })

  it('replays an event received while the initial snapshot request is still pending', async () => {
    const pendingSnapshot = deferred<SessionSnapshot>()
    mocked.snapshot.mockReturnValue(pendingSnapshot.promise)
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useOwnedSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(mocked.snapshot).toHaveBeenCalledWith('a'))

    act(() => result.current.store.applyProjectionEvent({
      type: 'item.created',
      session_id: 'a',
      seq: 11,
      revision: '11',
      item_id: 'item-11',
      item: { id: 'item-11', seq: 11, created_at: '', kind: 'message', visibility: 'visible', audience: 'user' },
    }))
    await act(async () => {
      pendingSnapshot.resolve(snapshotFor('a', '10', 10))
      await pendingSnapshot.promise
    })
    await waitFor(() => expect(result.current.history.itemsPage?.items.map((item) => item.seq)).toEqual([10, 11]))
  })

  it('resyncs after an overflowing pending queue and repairs the old initial snapshot', async () => {
    const firstSnapshot = deferred<SessionSnapshot>()
    const secondSnapshot = deferred<SessionSnapshot>()
    mocked.snapshot
      .mockImplementationOnce(() => firstSnapshot.promise)
      .mockImplementationOnce(() => secondSnapshot.promise)
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result } = renderHook(() => useOwnedSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(mocked.snapshot).toHaveBeenCalledTimes(1))

    const eventCount = pendingProjectionEventCap + 8
    act(() => {
      for (let i = 0; i < eventCount; i++) {
        result.current.store.applyProjectionEvent({
          type: 'item.created',
          session_id: 'a',
          seq: i + 2,
          revision: String(i + 2),
          item_id: `event-item-${i}`,
          item: { id: `event-item-${i}`, seq: i + 2, created_at: '', kind: 'message', visibility: 'visible', audience: 'user' },
        })
      }
    })
    expect(result.current.store.state.pendingProjectionBySession.a.overflowed).toBe(true)
    expect(result.current.store.state.pendingProjectionBySession.a.events).toHaveLength(pendingProjectionEventCap)

    firstSnapshot.resolve(snapshotFor('a', '1', 1))
    await waitFor(() => expect(mocked.snapshot).toHaveBeenCalledTimes(2))
    expect(result.current.history.itemsPage).toBeNull()

    const repairedItems = [
      { id: 'item-1', seq: 1 } as never,
      ...Array.from({ length: eventCount }, (_, i) => ({ id: `event-item-${i}`, seq: i + 2 } as never)),
    ]
    secondSnapshot.resolve({
      session_id: 'a',
      revision: String(eventCount + 1),
      session: session('a'),
      history: {
        items: repairedItems,
        oldest_seq: 1,
        newest_seq: eventCount + 1,
        has_more_before: false,
        has_more_after: false,
      },
    })
    await waitFor(() => expect(result.current.history.itemsPage?.items).toHaveLength(eventCount + 1))
    expect(result.current.history.itemsPage?.items.some((item) => item.id === 'event-item-0')).toBe(true)
    expect(mocked.snapshot).toHaveBeenCalledTimes(2)
  })
})
