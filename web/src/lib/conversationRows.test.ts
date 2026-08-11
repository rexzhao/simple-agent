import { describe, expect, it } from 'vitest'
import type { ActiveRun, RunStep, SessionItem } from '../types'
import { buildConversationRows, completedTurnByAssistantItem, conversationRowItemKey, getConversationFirstItemIndex, prependedConversationRowCount } from './conversationRows'

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
	it('calculates completion timing for the final assistant output in a complete turn', () => {
		const completions = completedTurnByAssistantItem([
			item('user-1', 1, 'user', 'question', { turn_id: 'turn-1', created_at: '2026-01-01T00:00:00.000Z' }),
			item('assistant-tool', 2, 'assistant', 'planning', {
				turn_id: 'turn-1',
				created_at: '2026-01-01T00:00:01.000Z',
				message: { role: 'assistant', content: { inline: 'planning' }, tool_calls: [{ id: 'tool-1', name: 'shell' }] },
			}),
			item('assistant-final', 3, 'assistant', 'answer', { turn_id: 'turn-1', created_at: '2026-01-01T00:00:02.000Z' }),
		])

		expect(completions.get('assistant-final')).toEqual({
			completedAt: '2026-01-01T00:00:02.000Z',
			durationMS: 2000,
		})
		expect(completions.has('assistant-tool')).toBe(false)

		const legacyCompletions = completedTurnByAssistantItem([
			item('legacy-user', 4, 'user', 'question', { created_at: '2026-01-01T00:01:00.000Z' }),
			item('legacy-final', 5, 'assistant', 'answer', { created_at: '2026-01-01T00:01:03.000Z' }),
		])
		expect(legacyCompletions.get('legacy-final')?.durationMS).toBe(3000)
	})

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

  it('keeps one row identity when an item update carries a different record sequence', () => {
    const before = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-1', 2, 'assistant', 'before', { turn_id: 'turn-1', agent_iteration: 1 })],
    })
    const after = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-1', 9, 'assistant', 'after', { turn_id: 'turn-1', agent_iteration: 1 })],
    })
    expect(after.filter((row) => row.kind === 'message')).toHaveLength(1)
    expect(after[0].key).toBe(before[0].key)
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

  it('does not double-render a durable tool while retaining transient reasoning', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('assistant-call', 1, 'assistant', '', {
          turn_id: 'turn-live',
          message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: 'call-1', name: 'shell' }] },
        }),
      ],
      activeRun: run({
        steps: [
          { kind: 'reasoning', id: 'reasoning-live', text: 'still thinking', iteration: 1 },
          { kind: 'tool', id: 'call-1', name: 'shell', status: 'running', iteration: 1 },
        ],
      }),
    })
    const active = rows.find((row) => row.kind === 'active-process')
    expect(active?.steps).toEqual([{ kind: 'reasoning', id: 'reasoning-live', text: 'still thinking', iteration: 1 }])
    const durableProcess = rows.find((row) => row.kind === 'process')
    expect(durableProcess?.steps).toEqual(expect.arrayContaining([
      expect.objectContaining({ kind: 'tool', id: 'call-1', status: 'running' }),
    ]))
  })

  it('keeps assistant content with tool calls in one process output', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-call', 1, 'assistant', 'I will inspect it.', {
        turn_id: 'turn-1',
        agent_iteration: 1,
        message: {
          role: 'assistant',
          content: { inline: 'I will inspect it.' },
          tool_calls: [{ id: 'call-1', name: 'shell' }],
        },
      })],
    })
    const processes = rows.filter((row) => row.kind === 'process')
    expect(processes).toHaveLength(1)
    expect(processes[0].steps).toEqual(expect.arrayContaining([
      expect.objectContaining({ kind: 'output', text: 'I will inspect it.' }),
      expect.objectContaining({ kind: 'tool', id: 'call-1' }),
    ]))
    expect(rows.filter((row) => row.kind === 'message')).toHaveLength(0)
  })

  it('uses the completed durable tool result instead of a pending live copy', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('assistant-call', 1, 'assistant', 'inspect', {
          turn_id: 'turn-1',
          agent_iteration: 1,
          message: { role: 'assistant', content: { inline: 'inspect' }, tool_calls: [{ id: 'call-1', name: 'shell' }] },
        }),
        item('tool-result', 2, 'tool', 'done', {
          turn_id: 'turn-1',
          agent_iteration: 1,
          status: 'completed',
          message: { role: 'tool', content: { inline: 'done' }, tool_call_id: 'call-1' },
        }),
      ],
      activeRun: run({
        steps: [{ kind: 'tool', id: 'call-1', name: 'shell', status: 'requested', iteration: 1 }],
      }),
    })
    const process = rows.find((row) => row.kind === 'process')
    expect(process?.steps).toEqual(expect.arrayContaining([
      expect.objectContaining({ kind: 'tool', id: 'call-1', status: 'finished', result: 'done' }),
    ]))
    expect(process?.steps).not.toEqual(expect.arrayContaining([
      expect.objectContaining({ kind: 'tool', id: 'call-1', status: 'requested' }),
    ]))
  })

  it('keeps a new cursor placeholder when an older turn has durable tools', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [item('old-call', 1, 'assistant', '', {
        turn_id: 'old-turn',
        message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: 'old-call-1', name: 'shell' }] },
      })],
      activeRun: run({ id: 'new-run', turnID: 'new-turn', assistantText: '', steps: [] }),
    })
    expect(rows.filter((row) => row.kind === 'active-cursor')).toHaveLength(1)
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)
  })

  it('does not duplicate reasoning after the bound assistant item becomes durable', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-1', 2, 'assistant', 'answer', {
        turn_id: 'turn-live',
        agent_iteration: 1,
        message: {
          role: 'assistant',
          content: { inline: 'answer' },
          reasoning: 'Authoritative durable reasoning.',
        },
      })],
      activeRun: run({
        turnID: 'turn-live',
        agentIteration: 1,
        steps: [{ kind: 'reasoning', id: 'reasoning-live', text: 'stale transient copy.', iteration: 1, turnID: 'turn-live', itemID: 'assistant-1' }],
        assistantItems: { 'turn-live:1': { itemID: 'assistant-1', durableTextLength: 6 } },
      }),
    })

    const reasoningSteps = rows.flatMap((row) => row.kind === 'process' || row.kind === 'active-process' ? row.steps : [])
      .filter((step) => step.kind === 'reasoning')
    expect(reasoningSteps).toEqual([
      { kind: 'reasoning', id: 'assistant-1-reasoning', text: 'Authoritative durable reasoning.', iteration: 1 },
    ])
  })

  it('reconciles durable reasoning for every turn and iteration by item identity', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('assistant-old-1', 1, 'assistant', 'old answer 1', {
          turn_id: 'turn-old',
          agent_iteration: 1,
          message: { role: 'assistant', content: { inline: 'old answer 1' }, reasoning: 'durable old one' },
        }),
        item('assistant-old-2', 2, 'assistant', 'old answer 2', {
          turn_id: 'turn-old',
          agent_iteration: 2,
          message: { role: 'assistant', content: { inline: 'old answer 2' }, reasoning: 'durable old two' },
        }),
        item('assistant-current', 3, 'assistant', 'current answer', {
          turn_id: 'turn-current',
          agent_iteration: 1,
          message: { role: 'assistant', content: { inline: 'current answer' }, reasoning: 'durable current' },
        }),
      ],
      activeRun: run({
        turnID: 'turn-current',
        agentIteration: 2,
        steps: [
          { kind: 'reasoning', id: 'old-transient-1', text: 'old transient one', iteration: 1, turnID: 'turn-old', itemID: 'assistant-old-1' },
          { kind: 'reasoning', id: 'old-transient-2', text: 'old transient two', iteration: 2, turnID: 'turn-old', itemID: 'assistant-old-2' },
          { kind: 'reasoning', id: 'current-transient', text: 'current not durable yet', iteration: 2, turnID: 'turn-current', itemID: 'assistant-current-2' },
          { kind: 'tool', id: 'current-tool', name: 'shell', status: 'requested', iteration: 2 },
        ],
        assistantItems: {
          'turn-old:1': { itemID: 'assistant-old-1', durableTextLength: 12 },
          'turn-old:2': { itemID: 'assistant-old-2', durableTextLength: 12 },
          'turn-current:1': { itemID: 'assistant-current', durableTextLength: 14 },
        },
      }),
    })

    const processSteps = rows.flatMap((row) => row.kind === 'process' || row.kind === 'active-process' ? row.steps : [])
    expect(processSteps.filter((step) => step.kind === 'reasoning').map((step) => step.text)).toEqual([
      'durable old one',
      'durable old two',
      'durable current',
      'current not durable yet',
    ])
    expect(processSteps.filter((step) => step.kind === 'reasoning' && step.itemID?.startsWith('assistant-old'))).toHaveLength(0)
    expect(processSteps).toContainEqual(expect.objectContaining({ kind: 'tool', id: 'current-tool', status: 'requested' }))
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(1)
  })

  it('keeps same-text reasoning when an explicit item identity does not match', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-durable', 1, 'assistant', 'answer', {
        turn_id: 'turn-live',
        agent_iteration: 1,
        message: { role: 'assistant', content: { inline: 'answer' }, reasoning: 'same reasoning' },
      })],
      activeRun: run({
        steps: [{ kind: 'reasoning', id: 'other-item', text: 'same reasoning', iteration: 1, turnID: 'turn-live', itemID: 'assistant-other' }],
        assistantItems: { 'turn-live:1': { itemID: 'assistant-durable', durableTextLength: 6 } },
      }),
    })

    const reasoningSteps = rows.flatMap((row) => row.kind === 'process' || row.kind === 'active-process' ? row.steps : [])
      .filter((step) => step.kind === 'reasoning')
    expect(reasoningSteps.map((step) => step.text)).toEqual(['same reasoning', 'same reasoning'])
    expect(reasoningSteps.some((step) => step.itemID === 'assistant-other')).toBe(true)
  })

  it('drops emptied old segments but keeps one cursor placeholder when no output remains', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-old', 1, 'assistant', 'old answer', {
        turn_id: 'turn-old',
        agent_iteration: 1,
        message: { role: 'assistant', content: { inline: 'old answer' }, reasoning: 'durable old reasoning' },
      })],
      activeRun: run({
        turnID: 'turn-current',
        agentIteration: 1,
        steps: [{ kind: 'reasoning', id: 'old-transient', text: 'old transient', iteration: 1, turnID: 'turn-old', itemID: 'assistant-old' }],
        assistantItems: { 'turn-old:1': { itemID: 'assistant-old', durableTextLength: 10 } },
      }),
    })

    const active = rows.filter((row) => row.kind === 'active-cursor')
    expect(active).toHaveLength(1)
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)
  })

  it('retains prior-segment reasoning when the current turn reuses its iteration number', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [item('assistant-current', 3, 'assistant', 'answer', {
        turn_id: 'turn-current',
        agent_iteration: 1,
        message: {
          role: 'assistant',
          content: { inline: 'answer' },
          reasoning: 'Current turn reasoning.',
        },
      })],
      activeRun: run({
        turnID: 'turn-current',
        agentIteration: 1,
        steps: [
          { kind: 'reasoning', id: 'reasoning-previous', text: 'Previous turn reasoning.', iteration: 1 },
          { kind: 'reasoning', id: 'reasoning-current', text: 'Current turn reasoning.', iteration: 1 },
        ],
        processBoundaries: [{ id: 'prompt-boundary', stepIndex: 1 }],
        assistantItems: { 'turn-current:1': { itemID: 'assistant-current', durableTextLength: 6 } },
      }),
    })

    const reasoningSteps = rows.flatMap((row) => row.kind === 'process' || row.kind === 'active-process' ? row.steps : [])
      .filter((step) => step.kind === 'reasoning')
    expect(reasoningSteps).toEqual([
      { kind: 'reasoning', id: 'assistant-current-reasoning', text: 'Current turn reasoning.', iteration: 1 },
      { kind: 'reasoning', id: 'reasoning-previous', text: 'Previous turn reasoning.', iteration: 1 },
      { kind: 'reasoning', id: 'reasoning-current', text: 'Current turn reasoning.', iteration: 1 },
    ])
  })

  it('reconstructs terminal reasoning from durable assistant items after the run is gone', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [
        item('user-1', 1, 'user', 'inspect the repository', { turn_id: 'turn-1' }),
        item('assistant-1', 2, 'assistant', 'The answer', {
          turn_id: 'turn-1',
          agent_iteration: 1,
          message: {
            role: 'assistant',
            content: { inline: 'The answer' },
            reasoning: 'I checked the relevant files first.',
          },
        }),
      ],
      // A settled run is intentionally absent. The item DTO is the only
      // source for terminal reasoning; no recentStepsByTurn bridge exists.
    })

    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)
    expect(rows.find((row) => row.kind === 'process')?.steps).toEqual([
      { kind: 'reasoning', id: 'assistant-1-reasoning', text: 'I checked the relevant files first.', iteration: 1 },
    ])
    expect(rows.filter((row) => row.kind === 'message').map((row) => row.item.id)).toEqual(['user-1', 'assistant-1'])
  })

  it('keeps one transient cursor row until the bound durable item is loaded', () => {
    const rows = buildConversationRows({
      sessionID: 'session-1',
      items: [],
      activeRun: run({
        assistantText: 'tail',
        assistantItems: { 'turn-live:1': { itemID: 'assistant-not-on-page', durableTextLength: 1 } },
      }),
    })
    expect(rows.filter((row) => row.kind === 'message')).toHaveLength(0)
    expect(rows.filter((row) => row.kind === 'active-cursor')).toHaveLength(1)
    expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)
  })

  it('does not keep an empty presentation row after a run stops', () => {
    for (const status of ['reconciling', 'failed', 'cancelled'] as const) {
      const rows = buildConversationRows({
        sessionID: 'session-1',
        items: [],
        activeRun: run({ status }),
      })
      expect(rows.filter((row) => row.kind === 'active-cursor')).toHaveLength(0)
      expect(rows.filter((row) => row.kind === 'active-process')).toHaveLength(0)
    }
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
