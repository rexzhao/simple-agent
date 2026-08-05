// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useSessionStore } from './useSessionStore'
import { frontendProtocolLogger } from '../lib/frontendProtocolLogger'

const apiMocks = vi.hoisted(() => ({ snapshot: vi.fn() }))
vi.mock('../api', () => ({ api: apiMocks }))

describe('useSessionStore revision reader', () => {
  afterEach(() => {
    frontendProtocolLogger.resetForTesting()
    apiMocks.snapshot.mockReset()
    vi.restoreAllMocks()
  })

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

  it('records accepted and ignored projection applications with before and after identities', async () => {
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.setEnabled('s1', true)
    apiMocks.snapshot.mockResolvedValue({
      session_id: 's1', revision: '0',
      session: { id: 's1', last_seq: 0 } as never,
      history: { items: [], oldest_seq: 0, newest_seq: 0, has_more_before: false, has_more_after: false },
    })
    const { result } = renderHook(() => useSessionStore())
    await act(async () => { await result.current.refreshSession('s1') })
    const event = {
      type: 'item.appended' as const,
      session_id: 's1', seq: 1, revision: '1', item_id: 'item-1',
      item: { id: 'item-1', seq: 1 } as never,
    }
    act(() => result.current.applyProjectionEvent(event))
    act(() => result.current.applyProjectionEvent(event))

    const applications = frontendProtocolLogger.getSnapshot('s1').records.filter((record) => record.kind === 'projection.apply')
    expect(applications).toHaveLength(2)
    expect(applications[0]).toMatchObject({ accepted: true, after: { revision: '1', item_ids: ['item-1'] } })
    expect(applications[1]).toMatchObject({ accepted: false, before: { item_ids: ['item-1'] }, after: { item_ids: ['item-1'] } })
  })
})
