// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useSessionStore } from './useSessionStore'

const apiMocks = vi.hoisted(() => ({ snapshot: vi.fn() }))
vi.mock('../api', () => ({ api: apiMocks }))

describe('useSessionStore revision reader', () => {
  it('does not treat a pre-snapshot queue as projection coverage', async () => {
    const { result } = renderHook(() => useSessionStore())
    expect(result.current.getSessionRevision('s1')).toBeUndefined()
    expect(result.current.isRevisionCovered('s1', 'bad')).toBe(false)

    act(() => {
      result.current.applyProjectionEvent({
        type: 'item.appended',
        session_id: 's1',
        seq: 1,
        revision: '90071992547409930',
        item_id: 'item-1',
        item: { id: 'item-1', seq: 1 } as never,
      })
    })
    // The event is retained for a later snapshot merge, but it is not a
    // complete history base and cannot clear a settlement reconciliation.
    expect(result.current.getSessionRevision('s1')).toBeUndefined()
    expect(result.current.isRevisionCovered('s1', '90071992547409930')).toBe(false)

    apiMocks.snapshot.mockResolvedValue({
      session_id: 's1', revision: '90071992547409929',
      session: { id: 's1', last_seq: 0 } as never,
      history: { items: [], oldest_seq: 0, newest_seq: 0, has_more_before: false, has_more_after: false },
    })
    await act(async () => { await result.current.refreshSession('s1') })
    expect(result.current.getSessionRevision('s1')).toBe('90071992547409930')
    expect(result.current.isRevisionCovered('s1', '90071992547409929')).toBe(true)
    expect(result.current.isRevisionCovered('s1', '90071992547409931')).toBe(false)

    expect(() => act(() => {
      result.current.applyProjectionEvent({
        type: 'item.updated', session_id: 's1', seq: 2, revision: 'invalid', item_id: 'item-1', item: { id: 'item-1', seq: 1 } as never,
      })
    })).not.toThrow()
    expect(result.current.getSessionRevision('s1')).toBe('90071992547409930')
  })
})
