// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ActiveRun } from '../types'
import { coalesceRunEvents, useRunRegistry } from './useRunRegistry'

const run: ActiveRun = { id: 'run-1', sessionID: 'session-1', userText: '', assistantText: '', steps: [], agentIteration: 1, status: 'running' }

describe('useRunRegistry delta batching', () => {
  afterEach(() => vi.restoreAllMocks())

  it('publishes many deltas in one animation-frame update', () => {
    let frame: FrameRequestCallback | undefined
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((callback) => { frame = callback; return 1 })
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => undefined)
    const { result } = renderHook(() => useRunRegistry())

    act(() => result.current.addActiveRun(run))
    act(() => {
      for (const text of ['a', 'b', 'c']) {
        result.current.queueRunEvent('session-1', 'run-1', { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text })
      }
    })
    expect(result.current.activeRunsBySession['session-1'].assistantText).toBe('')

    act(() => frame?.(0))
    expect(result.current.activeRunsBySession['session-1'].assistantText).toBe('abc')
  })

  it('synchronously flushes pending deltas before lifecycle events', () => {
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation(() => 1)
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => undefined)
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    act(() => result.current.queueRunEvent('session-1', 'run-1', { type: 'reasoning.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'thinking' }))
    act(() => result.current.flushRunEvents('session-1', 'run-1'))
    expect(result.current.activeRunsRef.current['session-1'].steps[0]).toMatchObject({ kind: 'reasoning', text: 'thinking' })
  })

  it('does not mix consecutive runs in one session', () => {
    let frame: FrameRequestCallback | undefined
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation((callback) => { frame = callback; return 1 })
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => undefined)
    const { result } = renderHook(() => useRunRegistry())
    act(() => result.current.addActiveRun(run))
    act(() => result.current.queueRunEvent('session-1', 'run-1', { type: 'text.delta', turn_id: 'one', agent_iteration: 1, text: 'old' }))
    act(() => result.current.addActiveRun({ ...run, id: 'run-2' }))
    act(() => result.current.queueRunEvent('session-1', 'run-2', { type: 'text.delta', turn_id: 'two', agent_iteration: 1, text: 'new' }))
    act(() => frame?.(0))
    expect(result.current.activeRunsBySession['session-1']).toMatchObject({ id: 'run-2', assistantText: 'new' })
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
