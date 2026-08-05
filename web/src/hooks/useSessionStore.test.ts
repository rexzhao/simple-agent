// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useSessionStore } from './useSessionStore'
import { frontendProtocolLogger } from '../lib/frontendProtocolLogger'
import { buildConversationRows } from '../lib/conversationRows'
import { reduceRunEvent } from '../lib/runEventReducer'
import type { ActiveRun, SessionItemProjectionEvent } from '../types'

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
    let firstAccepted = false
    let replayAccepted = true
    act(() => { firstAccepted = result.current.applyProjectionEvent(event) })
    act(() => { replayAccepted = result.current.applyProjectionEvent(event) })
    expect(firstAccepted).toBe(true)
    expect(replayAccepted).toBe(false)

    const applications = frontendProtocolLogger.getSnapshot('s1').records.filter((record) => record.kind === 'projection.apply')
    expect(applications).toHaveLength(2)
    expect(applications[0]).toMatchObject({ accepted: true, after: { revision: '1', item_ids: ['item-1'] } })
    expect(applications[1]).toMatchObject({ accepted: false, before: { item_ids: ['item-1'] }, after: { item_ids: ['item-1'] } })
  })

  it('keeps one latest durable row across create, updates, replay, and tail reconciliation', async () => {
    apiMocks.snapshot.mockResolvedValue({
      session_id: 's1', revision: '0',
      session: { id: 's1', last_seq: 0 } as never,
      history: { items: [], oldest_seq: 0, newest_seq: 0, has_more_before: false, has_more_after: false },
    })
    const { result } = renderHook(() => useSessionStore())
    await act(async () => { await result.current.refreshSession('s1') })

    const item = (content: string) => ({
      seq: 1, id: 'assistant-1', turn_id: 'turn-1', agent_iteration: 1,
      created_at: '', kind: 'message', visibility: 'visible', audience: 'model',
      message: { role: 'assistant', content: { inline: content } },
    } as never)
    const projection = (type: SessionItemProjectionEvent['type'], seq: number, revision: string, content: string): SessionItemProjectionEvent => ({
      type, session_id: 's1', run_id: 'run-1', turn_id: 'turn-1', agent_iteration: 1,
      seq, revision, item_id: 'assistant-1', item: item(content), assistant_text_length: content.length,
    })
    const created = projection('item.created', 1, '1', 'a')
    const firstUpdate = projection('item.updated', 2, '2', 'ab')
    const staleReplay = projection('item.updated', 1, '1', 'stale')
    const latestUpdate = projection('item.updated', 3, '3', 'abc')
    let run: ActiveRun = { id: 'run-1', sessionID: 's1', turnID: 'turn-1', assistantText: '', steps: [], agentIteration: 1, status: 'running' }
    run = reduceRunEvent(run, created)
    run = reduceRunEvent(run, { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'assistant-1', text: ' tail', durable_checkpointed: false })

    let accepted = false
    act(() => { accepted = result.current.applyProjectionEvent(created) })
    expect(accepted).toBe(true)
    act(() => { accepted = result.current.applyProjectionEvent(firstUpdate) })
    expect(accepted).toBe(true)
    act(() => { accepted = result.current.applyProjectionEvent(staleReplay) })
    expect(accepted).toBe(false)
    // App's accepted-projection gate deliberately does not feed staleReplay
    // to reduceRunEvent, so it cannot clear the current transient tail.
    expect(run.assistantText).toBe(' tail')
    act(() => { accepted = result.current.applyProjectionEvent(latestUpdate) })
    expect(accepted).toBe(true)
    run = reduceRunEvent(run, latestUpdate)

    const history = result.current.state.historyBySession.s1.page.items
    const rows = buildConversationRows({ sessionID: 's1', items: history, activeRun: run })
    expect(history).toHaveLength(1)
    expect(history[0].message?.content?.inline).toBe('abc')
    expect(rows.filter((row) => row.kind === 'message')).toHaveLength(1)
    expect(rows.filter((row) => row.kind === 'message')[0]).toMatchObject({ item: { id: 'assistant-1' }, assistantTail: undefined })
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)
  })
})
