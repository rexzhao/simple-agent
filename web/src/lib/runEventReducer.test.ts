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
    expect(queued.queuedPrompts).toEqual([{ id: 'prompt-1', content: 'follow up', steer: false }])

    const steered = reduceRunEvent(newRun(), {
      type: 'run.prompt_queue',
      prompts: [
        { id: 'prompt-2', content: 'steer me', steer: true },
        { id: 'prompt-3', content: 'later' },
      ],
    })
    expect(steered.queuedPrompts).toEqual([
      { id: 'prompt-2', content: 'steer me', steer: true },
      { id: 'prompt-3', content: 'later', steer: false },
    ])

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

  it('tracks automatic compaction lifecycle and replacement context size', () => {
    const compacting = reduceRunEvent(newRun(), {
      type: 'compaction.started',
      turn_id: 'turn-1',
      trigger: 'auto',
    })
    expect(compacting.compaction).toEqual({ trigger: 'auto', status: 'running' })

    const completed = reduceRunEvent(compacting, {
      type: 'compaction.completed',
      turn_id: 'turn-1',
      trigger: 'auto',
      compaction_id: 'compact-1',
      active_context_tokens: 12000,
      context_window: 400000,
    })
    expect(completed.compaction).toEqual({
      trigger: 'auto',
      status: 'completed',
      activeContextTokens: 12000,
      contextWindow: 400000,
    })
  })

  it('shows provider retry state until the retried request makes progress', () => {
    const retrying = reduceRunEvent(newRun(), {
      type: 'provider.retrying',
      turn_id: 'turn-1',
      agent_iteration: 2,
      attempt: 2,
      max_attempts: 3,
      delay_ms: 1000,
      reason: 'server_error',
    })
    expect(retrying.providerRetry).toEqual({ attempt: 2, maxAttempts: 3, delayMS: 1000 })

    const recovered = reduceRunEvent(retrying, {
      type: 'text.delta',
      turn_id: 'turn-1',
      agent_iteration: 2,
      text: 'ok',
    })
    expect(recovered.providerRetry).toBeUndefined()
  })

  it('clears the provider retry notice when the turn fails', () => {
    const retrying = reduceRunEvent(newRun(), {
      type: 'provider.retrying',
      turn_id: 'turn-1',
      agent_iteration: 1,
      attempt: 5,
      max_attempts: 5,
      delay_ms: 40000,
      reason: 'server_error',
    })
    expect(retrying.providerRetry).toBeDefined()
    const failed = reduceRunEvent(retrying, {
      type: 'turn.failed',
      turn_id: 'turn-1',
      code: 'model_server_error',
      message: 'server unavailable',
    })
    expect(failed.status).toBe('failed')
    expect(failed.providerRetry).toBeUndefined()
  })
})
