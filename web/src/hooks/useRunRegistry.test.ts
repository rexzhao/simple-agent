// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ActiveRun } from '../types'
import { coalesceRunEvents, streamPublishIntervalMS, useRunRegistry } from './useRunRegistry'

const run: ActiveRun = { id: 'run-1', sessionID: 'session-1', userText: '', assistantText: '', steps: [], agentIteration: 1, status: 'running' }

describe('useRunRegistry delta batching', () => {
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

  it('keeps authority current while limiting presentation updates', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useRunRegistry())

    act(() => result.current.addActiveRun(run))
    act(() => {
      for (const text of ['a', 'b', 'c']) {
        result.current.queueRunEvent('session-1', 'run-1', { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text })
      }
    })
    expect(result.current.activeRunsRef.current['session-1'].assistantText).toBe('abc')
    expect(result.current.activeRunsBySession['session-1'].assistantText).toBe('')
    act(() => vi.advanceTimersByTime(streamPublishIntervalMS))
    expect(result.current.activeRunsBySession['session-1'].assistantText).toBe('abc')
  })

  it('synchronously flushes pending deltas before lifecycle events', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    act(() => result.current.queueRunEvent('session-1', 'run-1', { type: 'reasoning.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'thinking' }))
    act(() => result.current.flushRunEvents('session-1', 'run-1'))
    expect(result.current.activeRunsRef.current['session-1'].steps[0]).toMatchObject({ kind: 'reasoning', text: 'thinking' })
  })

  it('keeps run membership during stream updates, changes on remove', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    act(() => result.current.queueRunEvent('session-1', 'run-1', { type: 'text.delta', turn_id: 'turn', agent_iteration: 1, text: 'delta' }))
    act(() => result.current.flushRunEvents('session-1', 'run-1'))
    // membership stable during delta flush (still has session-1)
    expect(result.current.runningSessionIDs.has('session-1')).toBe(true)
    act(() => result.current.updateActiveRun('session-1', 'run-1', () => null))
    expect(result.current.runningSessionIDs.has('session-1')).toBe(false)
  })

  it('does not mix consecutive runs in one session', () => {
    vi.useFakeTimers()
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    act(() => result.current.queueRunEvent('session-1', 'run-1', { type: 'text.delta', turn_id: 'one', agent_iteration: 1, text: 'old' }))
    // Mark run-1 as non-running so run-2 can supersede it
    act(() => result.current.updateActiveRun('session-1', 'run-1', (r) => ({ ...r, status: 'reconciling' })))
    act(() => {
      const added = result.current.addActiveRun({ ...run, id: 'run-2' })
      expect(added).toBe(true)
    })
    act(() => result.current.queueRunEvent('session-1', 'run-2', { type: 'text.delta', turn_id: 'two', agent_iteration: 1, text: 'new' }))
    act(() => vi.advanceTimersByTime(streamPublishIntervalMS))
    expect(result.current.activeRunsBySession['session-1']).toMatchObject({ id: 'run-2', assistantText: 'new' })
  })

  it('refuses to supersede a running run', () => {
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    let added = false
    act(() => { added = result.current.addActiveRun({ ...run, id: 'run-2' }) })
    expect(added).toBe(false)
    expect(result.current.activeRunsBySession['session-1'].id).toBe('run-1')
  })

  it('calls onSupersedeRun when superseding a non-running run', () => {
    const onSupersedeRun = vi.fn()
    const { result } = renderHook(() => useRunRegistry({ onSupersedeRun }))
    act(() => result.current.addActiveRun(run))
    act(() => result.current.updateActiveRun('session-1', 'run-1', (r) => ({ ...r, status: 'reconciling' })))
    act(() => { result.current.addActiveRun({ ...run, id: 'run-2' }) })
    expect(onSupersedeRun).toHaveBeenCalledWith('session-1', 'run-1')
    expect(result.current.activeRunsBySession['session-1'].id).toBe('run-2')
  })

  it('runningSessionIDs only includes sessions with running status', () => {
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    expect(result.current.runningSessionIDs.has('session-1')).toBe(true)
    act(() => result.current.updateActiveRun('session-1', 'run-1', (r) => ({ ...r, status: 'reconciling' })))
    expect(result.current.runningSessionIDs.has('session-1')).toBe(false)
  })
})

describe('coalesceRunEvents', () => {
  it('merges only adjacent compatible deltas', () => {
    const events = coalesceRunEvents([
      { type: 'text.delta', turn_id: 'turn', agent_iteration: 1, text: 'a' },
      { type: 'text.delta', turn_id: 'turn', agent_iteration: 1, text: 'b' },
      { type: 'reasoning.delta', turn_id: 'turn', agent_iteration: 1, text: 'r' },
      { type: 'text.delta', turn_id: 'turn', agent_iteration: 1, text: 'c' },
    ])
    expect(events.map((event) => ({ type: event.type, text: 'text' in event ? event.text : '' }))).toEqual([
      { type: 'text.delta', text: 'ab' },
      { type: 'reasoning.delta', text: 'r' },
      { type: 'text.delta', text: 'c' },
    ])
  })
})
