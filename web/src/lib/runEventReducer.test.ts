import { describe, expect, it } from 'vitest'
import type { ActiveRun, RunEvent } from '../types'
import { reduceRunEvent } from './runEventReducer'

const newRun = (): ActiveRun => ({
  id: 'run-1',
  sessionID: 'session-1',
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

  it('hands a durable assistant item off from the transient tail by identity', () => {
    const run = apply(
      newRun(),
      { type: 'turn.started', turn_id: 'turn-1' },
      { type: 'agent.iteration.started', turn_id: 'turn-1', agent_iteration: 1 },
      {
        type: 'item.appended', session_id: 'session-1', run_id: 'run-1', turn_id: 'turn-1',
        seq: 10, revision: '10', item_id: 'assistant-1', assistant_text_length: 3,
        item: { seq: 9, id: 'assistant-1', turn_id: 'turn-1', agent_iteration: 1, created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'abc' } } },
      },
      { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'abc', item_id: 'assistant-1', durable_text_length: 3, durable_checkpointed: true },
    )
    expect(run.assistantText).toBe('')
    expect(run.assistantItems?.['turn-1:1']).toEqual({ itemID: 'assistant-1', durableTextLength: 3 })

    const tailed = reduceRunEvent(run, {
      type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: ' tail',
      item_id: 'assistant-1', durable_text_length: 3, durable_checkpointed: false,
    })
    expect(tailed.assistantText).toBe(' tail')
    const updated = reduceRunEvent(tailed, {
      type: 'item.updated', session_id: 'session-1', turn_id: 'turn-1',
      seq: 11, revision: '11', item_id: 'assistant-1', assistant_text_length: 8,
      item: { seq: 9, id: 'assistant-1', turn_id: 'turn-1', agent_iteration: 1, created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'abc tail' } } },
    })
    expect(updated.assistantText).toBe('')
    expect(updated.assistantItems?.['turn-1:1']).toEqual({ itemID: 'assistant-1', durableTextLength: 8 })
  })

  it('does not deduplicate two durable assistant ids with identical text', () => {
    const base = newRun()
    const event = (id: string, seq: number): RunEvent => ({
      type: 'item.appended', session_id: 'session-1', seq, revision: String(seq), item_id: id,
      item: { seq, id, turn_id: 'turn-1', agent_iteration: seq, created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'same' } } },
    })
    const run = apply(base, event('assistant-a', 1), event('assistant-b', 2))
    expect(Object.values(run.assistantItems ?? {}).map((item) => item.itemID)).toEqual(['assistant-a', 'assistant-b'])
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

  it('replaces queued prompts without rendering drained prompts', () => {
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
    expect(drained.steps).toEqual([])
    expect(drained.processBoundaries).toEqual([{ id: 'prompt-boundary-run-1-1', stepIndex: 0 }])
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

  it('keeps reasoning identity when iterations restart across turns', () => {
    const run = apply(
      newRun(),
      { type: 'reasoning.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'assistant-1', text: 'first ' },
      { type: 'reasoning.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'assistant-1', text: 'turn' },
      { type: 'reasoning.delta', turn_id: 'turn-2', agent_iteration: 1, item_id: 'assistant-2', text: 'second' },
    )

    expect(run.steps).toEqual([
      { kind: 'reasoning', id: 'reasoning-turn-1-assistant-1-1-0', text: 'first turn', iteration: 1, turnID: 'turn-1', itemID: 'assistant-1' },
      { kind: 'reasoning', id: 'reasoning-turn-2-assistant-2-1-1', text: 'second', iteration: 1, turnID: 'turn-2', itemID: 'assistant-2' },
    ])
  })

  it('accumulates usage across agent iterations while keeping the latest context meter values', () => {
    const run = apply(
      newRun(),
      { type: 'usage.updated', turn_id: 'turn-1', agent_iteration: 1, input_tokens: 10, output_tokens: 4, total_tokens: 14, cached_tokens: 2, cache_write_tokens: 1, reasoning_tokens: 3 },
      { type: 'usage.updated', turn_id: 'turn-1', agent_iteration: 2, input_tokens: 6, output_tokens: 5, total_tokens: 11, cached_tokens: 4, cache_write_tokens: 0, reasoning_tokens: 2 },
    )

    expect(run).toMatchObject({ inputTokens: 6, outputTokens: 5, totalTokens: 11, cachedTokens: 4, cacheWriteTokens: 0, reasoningTokens: 2 })
    expect(run.usage).toEqual({ inputTokens: 16, outputTokens: 9, totalTokens: 25, cachedTokens: 6, cacheWriteTokens: 1, reasoningTokens: 5 })
  })

  it('replaces usage updates from the same provider request', () => {
    const run = apply(
      newRun(),
      { type: 'usage.updated', turn_id: 'turn-1', agent_iteration: 1, input_tokens: 10, output_tokens: 4, total_tokens: 14, cached_tokens: 2, cache_write_tokens: 1, reasoning_tokens: 3 },
      { type: 'usage.updated', turn_id: 'turn-1', agent_iteration: 1, input_tokens: 12, output_tokens: 5, total_tokens: 17, cached_tokens: 4, cache_write_tokens: 0, reasoning_tokens: 2 },
    )

    expect(run.usageEvents).toHaveLength(1)
    expect(run.usage).toEqual({ inputTokens: 12, outputTokens: 5, totalTokens: 17, cachedTokens: 4, cacheWriteTokens: 0, reasoningTokens: 2 })
  })

  it('drops a failed provider request before its retry', () => {
    const run = apply(
      newRun(),
      { type: 'usage.updated', turn_id: 'turn-1', agent_iteration: 1, input_tokens: 10, output_tokens: 4, total_tokens: 14, cached_tokens: 2, cache_write_tokens: 1, reasoning_tokens: 3 },
      { type: 'provider.retrying', turn_id: 'turn-1', agent_iteration: 1, attempt: 2, max_attempts: 3, delay_ms: 1000, reason: 'server_error' },
    )

    expect(run.usageEvents).toEqual([])
    expect(run.usage).toBeUndefined()
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
