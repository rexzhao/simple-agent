import { describe, expect, it } from 'vitest'
import { activeRunForConversation } from './sessionContentPresentation'
import { buildConversationRows } from './conversationRows'
import type { SessionRunState, SessionView } from '../domain/sessionContent'

const identity = JSON.stringify(['turn_1', 1, 'assistant_1'])

function run(overrides: Partial<SessionRunState> = {}): SessionRunState {
  return {
    runEpoch: 'epoch_1',
    runID: 'run_1',
    runCursor: '4',
    turnID: 'turn_1',
    status: 'running',
    messages: {},
    tools: {},
    stepOrder: [],
    promptQueue: [],
    appendedPrompts: [],
    stale: false,
    recoveryRequired: false,
    ...overrides,
  }
}

function view(state: SessionRunState): SessionView {
  return {
    availability: { status: 'ready' },
    dataAvailability: { status: 'ready' },
    history: { items: [], descriptor: { limit: 50, align_turn: true, visible_only: true, has_more_before: false, has_more_after: false } },
    historyState: { loading: false, version: 1 },
    activeRun: null,
    compaction: { checkpoints: [], truncated: false },
    runState: state,
  }
}

describe('activeRunForConversation', () => {
  it('projects a first-class assistant message snapshot without a tail binding', () => {
    const state = run({
      messages: {
        [identity]: {
          key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'assistant_1' },
          revision: '3',
          status: 'streaming',
          message: {
            role: 'assistant',
            content: { inline: 'complete snapshot' },
            reasoning: { inline: 'thinking' },
            tool_calls: [{ id: 'call_1', name: 'read_file', arguments: { inline: '{"path":"a"}' } }],
          },
        },
      },
      reasoningTimings: { [identity]: { startedAt: '2025-01-01T00:00:00Z', endedAt: '2025-01-01T00:00:01Z' } },
      stepOrder: [{ kind: 'reasoning', key: identity }],
    })

    const active = activeRunForConversation(view(state), 'session_1')
    expect(active?.messages?.[identity]).toMatchObject({
      itemID: 'assistant_1', revision: '3', status: 'streaming', text: 'complete snapshot', reasoning: 'thinking',
    })
    expect(active?.messages?.[identity].toolCalls?.[0]).toEqual({ id: 'call_1', name: 'read_file', arguments: '{"path":"a"}' })
    expect(active?.steps[0]).toMatchObject({ kind: 'reasoning', itemID: 'assistant_1', text: 'thinking' })
  })

  it('does not project an omitted-only lifecycle over durable assistant content', () => {
    const state = run({
      messages: {
        [identity]: {
          key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'assistant_1' },
          revision: '1',
          status: 'complete',
          snapshotAvailable: false,
          message: { role: 'assistant', content: { inline: '' } },
        },
      },
    })
    const active = activeRunForConversation(view(state), 'session_1')
    expect(active?.messages).toEqual({})

    const rows = buildConversationRows({
      sessionID: 'session_1',
      items: [{
        seq: 1, id: 'assistant_1', turn_id: 'turn_1', agent_iteration: 1,
        created_at: '2025-01-01T00:00:00Z', kind: 'message', visibility: 'visible', audience: 'user', status: 'completed',
        message: { role: 'assistant', content: { inline: 'durable large response' } },
      }],
      activeRun: active,
    })
    const message = rows.find((row) => row.kind === 'message')
    expect(message?.kind === 'message' ? message.item.message?.content?.inline : undefined).toBe('durable large response')
  })

  it('projects tools, queue state, and recovery status independently of messages', () => {
    const state = run({
      tools: { call_1: { tool_call_id: 'call_1', turn_id: 'turn_1', agent_iteration: 1, name: 'shell', status: 'finished', arguments: '{}', content: 'ok' } },
      stepOrder: [{ kind: 'tool', key: 'call_1' }],
      promptQueue: [{ id: 'prompt_1', content: 'next', steer: true }],
      recoveryRequired: true,
    })
    const active = activeRunForConversation(view(state), 'session_1')
    expect(active?.steps[0]).toMatchObject({ kind: 'tool', id: 'call_1', result: 'ok', status: 'finished' })
    expect(active?.queuedPrompts).toEqual([{ id: 'prompt_1', content: 'next', steer: true }])
    expect(active?.status).toBe('error_pending_refresh')
  })

  it('does not expose terminal overlays as active composer targets', () => {
    expect(activeRunForConversation(view(run({ status: 'committed' })), 'session_1')).toBeNull()
    expect(activeRunForConversation(view(run({ status: 'failed' })), 'session_1')).toBeNull()
  })
})
