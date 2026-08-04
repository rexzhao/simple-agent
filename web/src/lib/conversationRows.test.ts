import { describe, expect, it } from 'vitest'
import type { ActiveRun, RunStep, SessionItem } from '../types'
import { buildConversationRows, conversationRowItemKey, getConversationFirstItemIndex, prependedConversationRowCount } from './conversationRows'

function item(id: string, seq: number, role: string, content: string, options: Partial<SessionItem> = {}): SessionItem {
  return {
    id,
    seq,
    turn_id: options.turn_id,
    created_at: '',
    kind: options.kind ?? 'message',
    visibility: 'visible',
    audience: 'user',
    message: { role, content: { inline: content }, ...options.message },
    ...options,
  }
}

function run(overrides: Partial<ActiveRun> = {}): ActiveRun {
  return {
    id: 'run-1',
    sessionID: 'session-1',
    turnID: 'turn-live',
    assistantText: '',
    steps: [],
    agentIteration: 1,
    status: 'running',
    ...overrides,
  }
}

function keys(rows: ReturnType<typeof buildConversationRows>): string[] {
  return rows.map((row) => row.key)
}

describe('buildConversationRows stable identities', () => {
	it('uses the row key for virtualization and counts grouped rows, not raw items, on prepend', () => {
		const previous = buildConversationRows({
			sessionID: 'session-1',
			items: [
				item('old-user', 3, 'user', 'old', { turn_id: 'turn-1' }),
				item('old-call', 4, 'assistant', '', {
					turn_id: 'turn-1',
					message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: 'tool-1', name: 'shell' }] },
				}),
				item('old-result', 5, 'tool', 'result', {
					turn_id: 'turn-1',
					message: { role: 'tool', content: { inline: 'result' }, tool_call_id: 'tool-1' },
				}),
			],
		})
		const next = buildConversationRows({
			sessionID: 'session-1',
			items: [
				item('new-user', 1, 'user', 'new', { turn_id: 'turn-0' }),
				item('new-answer', 2, 'assistant', 'answer', { turn_id: 'turn-0' }),
				item('old-user', 3, 'user', 'old', { turn_id: 'turn-1' }),
				item('old-call', 4, 'assistant', '', {
					turn_id: 'turn-1',
					message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: 'tool-1', name: 'shell' }] },
				}),
				item('old-result', 5, 'tool', 'result', {
					turn_id: 'turn-1',
					message: { role: 'tool', content: { inline: 'result' }, tool_call_id: 'tool-1' },
				}),
			],
		})

		expect(conversationRowItemKey(999, next[0])).toBe(next[0].key)
		expect(prependedConversationRowCount(previous, next)).toBe(2)
		expect(getConversationFirstItemIndex(previous, next, 1000)).toBe(998)
	})

  it('gives ordinary messages, compaction, and historical process groups distinct stable keys', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('user-1', 1, 'user', 'run tools', { turn_id: 'turn-1' }),
        item('assistant-tools', 2, 'assistant', '', {
          turn_id: 'turn-1',
          agent_iteration: 1,
          message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: 'tool-1', name: 'shell' }] },
        }),
        item('tool-result', 3, 'tool', 'done', {
          turn_id: 'turn-1',
          agent_iteration: 1,
          message: { role: 'tool', content: { inline: 'done' }, tool_call_id: 'tool-1' },
        }),
        item('compact-1', 4, 'developer', 'Context compacted', {
          turn_id: 'turn-1',
          kind: 'compaction',
        }),
        item('assistant-final', 5, 'assistant', 'final', { turn_id: 'turn-1' }),
      ],
    })

    expect(rows.map((row) => row.kind)).toEqual(['message', 'process', 'compaction', 'message'])
    expect(new Set(keys(rows)).size).toBe(rows.length)
    expect(rows.every((row) => row.key.includes('session-1'))).toBe(true)
  })

  it('keeps existing durable row keys unchanged when a turn-aligned older page is prepended', () => {
    const current = [
      item('new-user', 3, 'user', 'new', { turn_id: 'turn-new' }),
      item('new-tools', 4, 'assistant', '', {
        turn_id: 'turn-new',
        agent_iteration: 1,
        message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: 'new-tool', name: 'shell' }] },
      }),
      item('new-tool-result', 5, 'tool', 'tool output', {
        turn_id: 'turn-new',
        agent_iteration: 1,
        message: { role: 'tool', content: { inline: 'tool output' }, tool_call_id: 'new-tool' },
      }),
      item('new-answer', 6, 'assistant', 'answer', { turn_id: 'turn-new' }),
    ]
    const before = buildConversationRows({ sessionID: 'session-1', items: current })
    const after = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('old-user', 1, 'user', 'old', { turn_id: 'turn-old' }),
        item('old-answer', 2, 'assistant', 'old answer', { turn_id: 'turn-old' }),
        ...current,
      ],
    })

    expect(keys(after).slice(-before.length)).toEqual(keys(before))
  })

  it('keeps the active tail key while stream content, compaction, and retry state update', () => {
    const first = buildConversationRows({
      sessionID: 'session-1',
      items: [],
      activeRun: run({
        assistantText: 'partial',
        compaction: { trigger: 'auto', status: 'running' },
        providerRetry: { attempt: 1, maxAttempts: 3, delayMS: 1000 },
      }),
    })
    const second = buildConversationRows({
      sessionID: 'session-1',
      items: [],
      activeRun: run({
        assistantText: 'partial output',
        steps: [{ kind: 'reasoning', id: 'reasoning-1', text: 'thinking', iteration: 1 }],
        compaction: { trigger: 'auto', status: 'completed', activeContextTokens: 500 },
        providerRetry: { attempt: 2, maxAttempts: 3, delayMS: 2000 },
      }),
    })

    expect(keys(second)).toEqual(keys(first))
    expect(second.find((row) => row.kind === 'active-process')).toMatchObject({ isLast: true, steps: [{ id: 'reasoning-1' }] })
    expect(second.find((row) => row.kind === 'active-compaction')).toMatchObject({ compaction: { status: 'completed' } })
    expect(second.find((row) => row.kind === 'provider-retry')).toMatchObject({ retry: { attempt: 2 } })
  })

  it('keeps prompt process boundaries without creating user rows and preserves durable identities', () => {
    const firstSteps: RunStep[] = [{ kind: 'reasoning', id: 'reasoning-1', text: 'before', iteration: 1 }]
    const durableItems = [
      item('backend-user-1', 1, 'user', 'same text', { turn_id: 'turn-1' }),
      item('backend-user-2', 2, 'user', 'same text', { turn_id: 'turn-2' }),
    ]
    const first = buildConversationRows({
      sessionID: 'session-1',
      items: durableItems,
      activeRun: run({
        steps: [...firstSteps, { kind: 'output', id: 'output-2', text: 'after', iteration: 1 }],
        processBoundaries: [{ id: 'boundary-1', stepIndex: firstSteps.length }],
      }),
    })

    const processes = first.filter((row) => row.kind === 'active-process')
    expect(processes).toHaveLength(2)
    expect(processes[0].steps).toEqual(firstSteps)
    expect(processes[1].steps).toEqual([{ kind: 'output', id: 'output-2', text: 'after', iteration: 1 }])
    expect(first.filter((row) => row.kind === 'message').map((row) => row.item.id)).toEqual(['backend-user-1', 'backend-user-2'])
    expect(new Set(keys(first)).size).toBe(first.length)
  })

  it('attaches an uncheckpointed tail to the loaded durable assistant item', () => {
    const durable = item('assistant-1', 2, 'assistant', 'a', {
      turn_id: 'turn-live',
      agent_iteration: 1,
    })
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [durable],
      activeRun: run({
        assistantText: 'b',
        assistantItems: { 'turn-live:1': { itemID: 'assistant-1', durableTextLength: 1 } },
      }),
    })
    const messages = rows.filter((row) => row.kind === 'message')
    expect(messages).toHaveLength(1)
    expect(messages[0]).toMatchObject({ item: { id: 'assistant-1' }, assistantTail: 'b' })
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)

    const afterCheckpoint = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-1', 2, 'assistant', 'ab', { turn_id: 'turn-live', agent_iteration: 1 })],
      activeRun: run({
        assistantItems: { 'turn-live:1': { itemID: 'assistant-1', durableTextLength: 2 } },
      }),
    })
    expect(afterCheckpoint.filter((row) => row.kind === 'message')).toHaveLength(1)
    expect(afterCheckpoint.filter((row) => row.kind === 'message')[0]).toMatchObject({ assistantTail: undefined })
  })

  it('falls back to one transient output row until the bound durable item is loaded', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [],
      activeRun: run({
        assistantText: 'tail',
        assistantItems: { 'turn-live:1': { itemID: 'assistant-not-on-page', durableTextLength: 1 } },
      }),
    })
    expect(rows.filter((row) => row.kind === 'message')).toHaveLength(0)
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(1)
  })

  it('keeps identical text as separate messages when item ids differ', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('assistant-a', 1, 'assistant', 'same', { turn_id: 'turn-1' }),
        item('assistant-b', 2, 'assistant', 'same', { turn_id: 'turn-2' }),
      ],
    })
    expect(rows.filter((row) => row.kind === 'message').map((row) => row.item.id)).toEqual(['assistant-a', 'assistant-b'])
  })
})
