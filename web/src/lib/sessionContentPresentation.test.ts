import { describe, expect, it } from 'vitest'
import { buildConversationRows } from './conversationRows'
import { activeRunForConversation } from './sessionContentPresentation'
import type { ActiveRun, SessionItem } from '../types'
import type { SessionView, SessionRunState } from '../domain/sessionContent'

function assistant(turnID: string, iteration: number, itemID: string, text: string, seq: number): SessionItem {
  return {
    id: itemID,
    seq,
    turn_id: turnID,
    agent_iteration: iteration,
    created_at: `2025-01-01T00:00:0${seq}Z`,
    kind: 'message',
    visibility: 'visible',
    audience: 'user',
    message: { role: 'assistant', content: { inline: text } },
  }
}

function runWithTails(tails: Record<string, { turnID: string; agentIteration: number; itemID: string; text: string; durableTextLength: number }>): ActiveRun {
  return { id: 'run_1', sessionID: 'session_1', assistantText: '', assistantTails: tails, steps: [], agentIteration: 2, status: 'running' }
}

function key(turnID: string, iteration: number, itemID: string): string {
  return JSON.stringify([turnID, iteration, itemID])
}

function viewWithRun(state: SessionRunState): SessionView {
  const snapshot = {
    schema_version: 1 as const,
    session: {
      id: 'session_1', version: 1, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
      archived: false, last_used_at: '2025-01-01T00:00:00Z', has_unread_result: false, status: 'running' as const,
      show_reasoning: false, full_access: false, debug: { request_bodies: false }, context: {}, save_tool_results: false,
    },
    history: { items: [], descriptor: { limit: 20, align_turn: false, visible_only: true, has_more_before: false, has_more_after: false } },
    active_run: null,
    compaction: { checkpoints: [], truncated: false },
  }
  return {
    availability: { status: 'ready' }, dataAvailability: { status: 'ready' }, session: snapshot.session,
    history: snapshot.history, activeRun: null, compaction: snapshot.compaction,
    historyState: { loading: false, version: 0 }, runState: state,
  }
}

describe('Session Content transient assistant presentation ownership', () => {
  it('renders a durable prefix and transient tail exactly once', () => {
    const identity = key('turn_1', 1, 'item_1')
    const rows = buildConversationRows({
      sessionID: 'session_1',
      items: [assistant('turn_1', 1, 'item_1', 'prefix+tail', 1)],
      activeRun: runWithTails({ [identity]: { turnID: 'turn_1', agentIteration: 1, itemID: 'item_1', text: 'tail', durableTextLength: 6 } }),
    })
    const message = rows.find((row) => row.kind === 'message')
    expect(message?.kind).toBe('message')
    if (message?.kind === 'message') {
      expect(message.item.message?.content?.inline).toBe('prefix+tail')
      expect(message.assistantTail).toBeUndefined()
    }
    expect(rows.filter((row) => row.kind === 'active-assistant')).toHaveLength(0)
  })

  it('keeps two assistant iterations on their complete identities', () => {
    const first = key('turn_1', 1, 'item_1')
    const second = key('turn_1', 2, 'item_2')
    const rows = buildConversationRows({
      sessionID: 'session_1',
      items: [assistant('turn_1', 1, 'item_1', 'one+tail-one', 1), assistant('turn_1', 2, 'item_2', 'two+tail-two', 2)],
      activeRun: runWithTails({
        [first]: { turnID: 'turn_1', agentIteration: 1, itemID: 'item_1', text: 'tail-one', durableTextLength: 3 },
        [second]: { turnID: 'turn_1', agentIteration: 2, itemID: 'item_2', text: 'tail-two', durableTextLength: 3 },
      }),
    })
    const messages = rows.filter((row): row is Extract<typeof rows[number], { kind: 'message' }> => row.kind === 'message')
    expect(messages.map((row) => row.item.message?.content?.inline)).toEqual(['one+tail-one', 'two+tail-two'])
    expect(messages.map((row) => row.assistantTail)).toEqual([undefined, undefined])
  })

  it('shows each transient tail before its durable item arrives', () => {
    const first = key('turn_1', 1, 'item_1')
    const second = key('turn_1', 2, 'item_2')
    const rows = buildConversationRows({
      sessionID: 'session_1', items: [], activeRun: runWithTails({
        [first]: { turnID: 'turn_1', agentIteration: 1, itemID: 'item_1', text: 'first tail', durableTextLength: 0 },
        [second]: { turnID: 'turn_1', agentIteration: 2, itemID: 'item_2', text: 'second tail', durableTextLength: 0 },
      }),
    })
    expect(rows.filter((row) => row.kind === 'active-assistant').map((row) => row.text)).toEqual(['first tail', 'second tail'])
  })

  it('keeps keyed tails distinct and converges after a durable checkpoint', () => {
    const state = {
      runEpoch: 'epoch_1', runID: 'run_1', runCursor: '3', turnID: 'turn_1', status: 'running' as const,
      text: {
        [key('turn_1', 1, 'item_1')]: { key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'item_1' }, text: 'remaining', baseLength: 13, checkpointed: true },
        [key('turn_1', 2, 'item_2')]: { key: { turn_id: 'turn_1', agent_iteration: 2, item_id: 'item_2' }, text: 'other', baseLength: 3, checkpointed: false },
      },
      reasoning: {}, tools: {}, stepOrder: [], promptQueue: [], appendedPrompts: [], stale: false, recoveryRequired: false,
    } satisfies SessionRunState
    const active = activeRunForConversation(viewWithRun(state), 'session_1')
    expect(active?.assistantText).toBe('')
    expect(Object.keys(active?.assistantTails ?? {})).toEqual([key('turn_1', 1, 'item_1'), key('turn_1', 2, 'item_2')])
    const rows = buildConversationRows({
      sessionID: 'session_1',
      items: [assistant('turn_1', 1, 'item_1', 'prefix+consumed+remaining', 1)],
      activeRun: active,
    })
    const message = rows.find((row) => row.kind === 'message')
    expect(message?.kind === 'message' ? message.item.message?.content?.inline : '').toBe('prefix+consumed+remaining')
    expect(rows.filter((row) => row.kind === 'active-assistant').map((row) => row.text)).toEqual(['other'])
  })

  it('does not append a keyed tail a second time after a partial durable merge', () => {
    const identity = key('turn_1', 1, 'item_1')
    const rows = buildConversationRows({
      sessionID: 'session_1',
      // The repository has already merged the checkpointed portion into the
      // inline value.  The presentation layer must not append the full source
      // tail again merely because the durable frame is not at its final size.
      items: [assistant('turn_1', 1, 'item_1', 'prefix+tail-', 1)],
      activeRun: runWithTails({ [identity]: { turnID: 'turn_1', agentIteration: 1, itemID: 'item_1', text: 'tail-rest', durableTextLength: 7 } }),
    })
    const message = rows.find((row) => row.kind === 'message')
    expect(message?.kind === 'message' ? message.item.message?.content?.inline : '').toBe('prefix+tail-')
    expect(message?.kind === 'message' ? message.assistantTail : undefined).toBeUndefined()
  })

  it('redacts web.eval source from active tool presentation arguments', () => {
    const state = {
      runEpoch: 'epoch_1', runID: 'run_1', runCursor: '3', turnID: 'turn_1', status: 'running' as const,
      text: {}, reasoning: {},
      tools: {
        call_1: {
          tool_call_id: 'call_1', turn_id: 'turn_1', agent_iteration: 1, name: 'web.eval', status: 'running' as const,
          arguments: JSON.stringify({ code: 'document.body.innerHTML', timeout_ms: 5000 }),
        },
      },
      stepOrder: [{ kind: 'tool', key: 'call_1' }],
      promptQueue: [], appendedPrompts: [], stale: false, recoveryRequired: false,
    } satisfies SessionRunState
    const active = activeRunForConversation(viewWithRun(state), 'session_1')
    const tool = active?.steps.find((step) => step.kind === 'tool')
    expect(tool?.kind).toBe('tool')
    if (tool?.kind === 'tool') {
      expect(tool.arguments).toBe('{"code_bytes":23,"timeout_ms":5000}')
      expect(tool.arguments).not.toContain('document.body')
    }
  })

  it('preserves the first-seen interleaving of reasoning and tools', () => {
    const reasoningA = key('turn_1', 1, 'reasoning_a')
    const reasoningB = key('turn_1', 1, 'reasoning_b')
    const state = {
      runEpoch: 'epoch_1', runID: 'run_1', runCursor: '6', turnID: 'turn_1', status: 'running' as const,
      text: {},
      reasoning: {
        [reasoningA]: { key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'reasoning_a' }, text: 'first', baseLength: 0, checkpointed: false },
        [reasoningB]: { key: { turn_id: 'turn_1', agent_iteration: 1, item_id: 'reasoning_b' }, text: 'third', baseLength: 0, checkpointed: false },
      },
      tools: {
        tool_a: { tool_call_id: 'tool_a', turn_id: 'turn_1', agent_iteration: 1, name: 'shell', status: 'finished' as const, arguments: '{}', content: 'ok', is_error: false },
        tool_b: { tool_call_id: 'tool_b', turn_id: 'turn_1', agent_iteration: 1, name: 'read_file', status: 'running' as const, arguments: '{"path":"a.ts"}' },
      },
      stepOrder: [
        { kind: 'reasoning' as const, key: reasoningA },
        { kind: 'tool' as const, key: 'tool_a' },
        { kind: 'reasoning' as const, key: reasoningB },
        { kind: 'tool' as const, key: 'tool_b' },
      ],
      promptQueue: [], appendedPrompts: [], stale: false, recoveryRequired: false,
    } satisfies SessionRunState

    const active = activeRunForConversation(viewWithRun(state), 'session_1')
    expect(active?.steps.map((step) => step.kind === 'reasoning' ? step.text : step.id)).toEqual([
      'first', 'tool_a', 'third', 'tool_b',
    ])
  })

  it('preserves Go safety summaries and fail-closed redaction', () => {
    const makeState = (argumentsText: string) => ({
      runEpoch: 'epoch_1', runID: 'run_1', runCursor: '3', turnID: 'turn_1', status: 'running' as const,
      text: {}, reasoning: {},
      tools: {
        call_1: {
          tool_call_id: 'call_1', turn_id: 'turn_1', agent_iteration: 1, name: 'web.eval', status: 'running' as const,
          arguments: argumentsText,
        },
      },
      stepOrder: [{ kind: 'tool', key: 'call_1' }],
      promptQueue: [], appendedPrompts: [], stale: false, recoveryRequired: false,
    } satisfies SessionRunState)
    const summary = '{"code_bytes":23,"timeout_ms":5000}'
    expect(activeRunForConversation(viewWithRun(makeState(summary)), 'session_1')?.steps.find((step) => step.kind === 'tool')?.arguments).toBe(summary)
    expect(activeRunForConversation(viewWithRun(makeState('{"arguments":"redacted"}')), 'session_1')?.steps.find((step) => step.kind === 'tool')?.arguments).toBe('{"arguments":"redacted"}')
    expect(activeRunForConversation(viewWithRun(makeState('{"code":"secret"}')), 'session_1')?.steps.find((step) => step.kind === 'tool')?.arguments).toBe('{"code_bytes":6}')
    expect(activeRunForConversation(viewWithRun(makeState('{"code":""}')), 'session_1')?.steps.find((step) => step.kind === 'tool')?.arguments).toBe('{"arguments":"redacted"}')
    expect(activeRunForConversation(viewWithRun(makeState(JSON.stringify({ code: '界'.repeat(32769) }))), 'session_1')?.steps.find((step) => step.kind === 'tool')?.arguments).toBe('{"arguments":"redacted"}')
    expect(activeRunForConversation(viewWithRun(makeState('{"code_bytes":0}')), 'session_1')?.steps.find((step) => step.kind === 'tool')?.arguments).toBe('{"arguments":"redacted"}')
  })
})
