// @vitest-environment jsdom
import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api'
import type { ItemsPage, Session } from '../types'
import { useSessionHistory } from './useSessionHistory'

vi.mock('../api', () => ({ api: { session: vi.fn(), items: vi.fn() } }))
const mocked = vi.mocked(api)
const session = (id: string): Session => ({ id, project_id: 'project', display_name: id } as Session)
const page = (seq: number): ItemsPage => ({ items: [{ id: `item-${seq}`, seq } as never], oldest_seq: seq, newest_seq: seq, has_more_before: seq > 1, has_more_after: false })
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: unknown) => void; const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail }); return { promise, resolve, reject } }

describe('useSessionHistory', () => {
  beforeEach(() => vi.clearAllMocks())

  it('ignores a stale response after session selection changes', async () => {
    const aDetail = deferred<Session>(); const aPage = deferred<ItemsPage>()
    mocked.session.mockImplementation((id) => id === 'a' ? aDetail.promise : Promise.resolve(session('b')))
    mocked.items.mockImplementation((id) => id === 'a' ? aPage.promise : Promise.resolve(page(20)))
    const loadSessions = vi.fn().mockResolvedValue([])
    const onError = vi.fn()
    const { result, rerender } = renderHook(({ id }) => useSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
    rerender({ id: 'b' })
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('b'))
    await act(async () => { aDetail.resolve(session('a')); aPage.resolve(page(10)); await Promise.all([aDetail.promise, aPage.promise]) })
    expect(result.current.sessionDetail?.id).toBe('b')
    expect(result.current.itemsPage?.oldest_seq).toBe(20)
  })

  it('prepends older items in order', async () => {
    mocked.session.mockResolvedValue(session('a'))
    mocked.items.mockResolvedValueOnce(page(10)).mockResolvedValueOnce(page(5))
    const loadSessions = vi.fn().mockResolvedValue([]); const onError = vi.fn()
    const { result } = renderHook(() => useSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.itemsPage?.oldest_seq).toBe(10))
    await act(async () => { expect(await result.current.loadOlder()).toBe(true) })
    expect(result.current.itemsPage?.items.map((item) => item.seq)).toEqual([5, 10])
  })

  it('does not merge loadOlder after switching sessions', async () => {
    const older = deferred<ItemsPage>()
    mocked.session.mockImplementation((id) => Promise.resolve(session(id)))
    mocked.items.mockImplementationOnce(() => Promise.resolve(page(10))).mockImplementationOnce(() => older.promise).mockImplementation(() => Promise.resolve(page(20)))
    const loadSessions = vi.fn().mockResolvedValue([]); const onError = vi.fn()
    const { result, rerender } = renderHook(({ id }) => useSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
    await waitFor(() => expect(result.current.itemsPage?.oldest_seq).toBe(10))
    let loading!: Promise<boolean>
    act(() => { loading = result.current.loadOlder() })
    rerender({ id: 'b' })
    await waitFor(() => expect(result.current.sessionDetail?.id).toBe('b'))
    await act(async () => { older.resolve(page(5)); expect(await loading).toBe(false) })
    expect(result.current.itemsPage?.oldest_seq).toBe(20)
  })

  it('does not report a rejected request after switching sessions', async () => {
    const staleDetail = deferred<Session>(); const stalePage = deferred<ItemsPage>()
    mocked.session.mockImplementation((id) => id === 'a' ? staleDetail.promise : Promise.resolve(session('b')))
    mocked.items.mockImplementation((id) => id === 'a' ? stalePage.promise : Promise.resolve(page(20)))
    const loadSessions = vi.fn().mockResolvedValue([]); const onError = vi.fn()
    const { rerender } = renderHook(({ id }) => useSessionHistory(id, loadSessions, onError), { initialProps: { id: 'a' } })
    rerender({ id: 'b' })
    await waitFor(() => expect(mocked.session).toHaveBeenCalledWith('b'))
    await act(async () => { staleDetail.reject(new Error('stale')); stalePage.resolve(page(1)); await Promise.resolve() })
    expect(onError).not.toHaveBeenCalled()
  })

  it('keeps the newest concurrent refresh for the same session', async () => {
    mocked.session.mockResolvedValueOnce(session('a'))
    mocked.items.mockResolvedValueOnce(page(1))
    const loadSessions = vi.fn().mockResolvedValue([]); const onError = vi.fn()
    const { result } = renderHook(() => useSessionHistory('a', loadSessions, onError))
    await waitFor(() => expect(result.current.itemsPage?.oldest_seq).toBe(1))
    const oldDetail = deferred<Session>(); const oldPage = deferred<ItemsPage>()
    mocked.session.mockReturnValueOnce(oldDetail.promise).mockResolvedValueOnce(session('a'))
    mocked.items.mockReturnValueOnce(oldPage.promise).mockResolvedValueOnce(page(3))
    let oldRefresh!: Promise<Session | null>; let newRefresh!: Promise<Session | null>
    act(() => { oldRefresh = result.current.refreshSession('a'); newRefresh = result.current.refreshSession('a') })
    await act(async () => { await newRefresh })
    expect(result.current.itemsPage?.oldest_seq).toBe(3)
    await act(async () => { oldDetail.resolve(session('a')); oldPage.resolve(page(2)); await oldRefresh })
    expect(result.current.itemsPage?.oldest_seq).toBe(3)
  })
})
