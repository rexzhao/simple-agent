import { describe, expect, it } from 'vitest'
import type { ActiveRun, RunEvent } from '../types'
import { reduceRunEvent } from './runEventReducer'

const newRun = (): ActiveRun => ({
  id: 'run-1',
  sessionID: 'session-1',
  userText: 'hello',
  assistantText: '',
  steps: [],
  agentIteration: 0,
  status: 'running',
})

function apply(run: ActiveRun, ...events: RunEvent[]): ActiveRun {
  return events.reduce(reduceRunEvent, run)
}

describe('reduceRunEvent', () => {
  it('accumulates streamed output and commits it when a tool starts', () => {
    const run = apply(
      newRun(),
      { type: 'agent.iteration.started', turn_id: 'turn-1', agent_iteration: 1 },
      { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'hel' },
      { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'lo' },
      { type: 'tool.requested', turn_id: 'turn-1', agent_iteration: 1, tool_call_id: 'tool-1', name: 'read' },
    )

    expect(run.assistantText).toBe('')
    expect(run.steps).toEqual([
      expect.objectContaining({ kind: 'output', text: 'hello', iteration: 1 }),
      expect.objectContaining({ kind: 'tool', id: 'tool-1', status: 'requested' }),
    ])
  })

  it('updates one tool across iterations without duplicating it', () => {
    const run = apply(
      newRun(),
      { type: 'tool.requested', turn_id: 'turn-1', agent_iteration: 1, tool_call_id: 'tool-1', name: 'shell' },
      { type: 'tool.started', turn_id: 'turn-1', agent_iteration: 2, tool_call_id: 'tool-1', name: 'shell' },
      { type: 'tool.finished', turn_id: 'turn-1', agent_iteration: 2, tool_call_id: 'tool-1', name: 'shell', is_error: false, content: 'done' },
    )

    expect(run.steps).toHaveLength(1)
    expect(run.steps[0]).toMatchObject({ kind: 'tool', id: 'tool-1', iteration: 1, status: 'finished', result: 'done' })
  })

  it('replaces queued prompts and records drained prompts', () => {
    const queued = reduceRunEvent(newRun(), {
      type: 'run.prompt_queue',
      prompts: [{ id: 'prompt-1', content: 'follow up' }],
    })
    expect(queued.queuedPrompts).toEqual([{ id: 'prompt-1', content: 'follow up' }])

    const drained = apply(
      queued,
      { type: 'run.prompt_appended', turn_id: 'turn-1', prompts: ['follow up'] },
      { type: 'run.prompt_queue', prompts: [] },
    )
    expect(drained.queuedPrompts).toEqual([])
    expect(drained.turnID).toBe('turn-1')
    expect(drained.steps).toContainEqual(expect.objectContaining({ kind: 'user', text: 'follow up' }))
  })

  it('tracks reasoning and usage and marks failures', () => {
    const run = apply(
      newRun(),
      { type: 'reasoning.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'think ' },
      { type: 'reasoning.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'more' },
      { type: 'usage.updated', turn_id: 'turn-1', agent_iteration: 1, input_tokens: 10, output_tokens: 4, total_tokens: 14, cached_tokens: 2, cache_write_tokens: 1, reasoning_tokens: 3 },
      { type: 'turn.failed', turn_id: 'turn-1', code: 'failed', message: 'boom' },
    )

    expect(run.steps).toEqual([expect.objectContaining({ kind: 'reasoning', text: 'think more' })])
    expect(run).toMatchObject({ inputTokens: 10, totalTokens: 14, cachedTokens: 2, reasoningTokens: 3, status: 'failed', error: 'boom' })
  })
})
