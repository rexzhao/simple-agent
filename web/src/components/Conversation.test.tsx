// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, fireEvent, waitFor } from '@testing-library/react'
import type { ReactElement } from 'react'

// VirtuosoMockContext deliberately renders only the mocked viewport range.
// Conversation tests assert row rendering, rather than virtualization, so use
// a small deterministic implementation that expands every item and exposes
// the imperative methods the production wrapper calls.
vi.mock('react-virtuoso', async () => {
  const React = await import('react')
  const MockVirtuoso = React.forwardRef<any, any>(function MockVirtuoso(props, ref) {
    const scrollerRef = React.useRef<HTMLDivElement | null>(null)
    const handle = React.useMemo(() => ({
      autoscrollToBottom: () => {},
      getState: (callback: (state: { ranges: []; scrollTop: number }) => void) => callback({ ranges: [], scrollTop: scrollerRef.current?.scrollTop ?? 0 }),
      scrollBy: (location: ScrollToOptions) => scrollerRef.current?.scrollBy(location),
      scrollIntoView: () => {},
      scrollTo: (location: ScrollToOptions) => scrollerRef.current?.scrollTo(location),
      scrollToIndex: () => {},
    }), [])
    React.useImperativeHandle(ref, () => handle, [handle])
    React.useEffect(() => {
      props.scrollerRef?.(scrollerRef.current)
      props.atBottomStateChange?.(true)
      return () => props.scrollerRef?.(null)
    }, [props.atBottomStateChange, props.scrollerRef])

    const Scroller = props.components?.Scroller ?? 'div'
    const Header = props.components?.Header
    const Footer = props.components?.Footer
    const List = props.components?.List ?? 'div'
    const EmptyPlaceholder = props.components?.EmptyPlaceholder
    const data = props.data ?? []
    return React.createElement(
      Scroller,
      { ref: scrollerRef },
      Header ? React.createElement(Header) : null,
      React.createElement(
        List,
        null,
        data.length > 0
          ? data.map((row: unknown, index: number) => React.createElement(React.Fragment, { key: props.computeItemKey?.(index, row) ?? index }, props.itemContent?.(index, row)))
          : EmptyPlaceholder ? React.createElement(EmptyPlaceholder) : null,
      ),
      Footer ? React.createElement(Footer) : null,
    )
  })
  return { Virtuoso: MockVirtuoso }
})

import { Conversation } from './Conversation'
import type { ActiveRun, ItemsPage, Session } from '../types'
import { emptyComposerDraft } from './Composer'

afterEach(cleanup)

function renderConversation(ui: ReactElement) {
  const result = render(ui)
  return {
    ...result,
    rerender: (next: ReactElement) => result.rerender(next),
  }
}

const session = (id: string): Session => ({
  id, project_id: 'p', display_name: `Session ${id}`, last_seq: 0,
  provider: 'fake', model_profile: 'default', model_id: 'fake-default',
  created_at: '', updated_at: '', root_session_id: id, spawn_depth: 0,
  archived: false, last_used_at: '', full_access: false, created_cwd: '',
} as Session)

const emptyPage: ItemsPage = { items: [], oldest_seq: 0, newest_seq: 0, has_more_before: false, has_more_after: false }

const baseProps = {
  page: emptyPage,
  activeRun: null,
  compacting: false,
  draft: emptyComposerDraft,
  onDraftChange: vi.fn(),
  onPastedTextAdd: vi.fn(),
  onPastedTextRemove: vi.fn(),
  onPastedImageAdd: vi.fn(),
  onPastedImageRemove: vi.fn(),
  onDraftClear: vi.fn(),
  sessionNames: {},
  turnError: null,
  onDismissTurnError: vi.fn(),
  onLoadOlder: vi.fn(),
  onSend: vi.fn(),
  onCancel: vi.fn(),
  onCancelTool: vi.fn(),
  onContinue: vi.fn(),
  onRetryRefresh: vi.fn(),
  onDebug: vi.fn(),
  onToggleFullAccess: vi.fn(),
  onRemoveQueuedPrompt: vi.fn(),
  onSteerQueuedPrompt: vi.fn(),
  onMoveQueuedPrompt: vi.fn(),
  onCompact: vi.fn(),
}

describe('Conversation identity boundary', () => {
  it('updates header, composer, and continue presentation from Session Index status alone', () => {
    const detail = { ...session('s1'), status: 'idle', interrupted_run_id: 'run-interrupted', interrupted_turn_id: 'turn-interrupted' } as Session
    const draft = { ...emptyComposerDraft, content: 'pending input' }
    const view = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} draft={draft} sessionIndexStatus="idle" />)
    expect(screen.getByLabelText('Session status: idle')).toBeDefined()
    expect((screen.getByRole('textbox') as HTMLTextAreaElement).disabled).toBe(false)
    expect(screen.queryByRole('button', { name: 'Continue' })).toBeNull()

    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} draft={draft} sessionIndexStatus="running" />)
    expect(screen.getByLabelText('Session status: running')).toBeDefined()
    expect((screen.getByRole('textbox') as HTMLTextAreaElement).disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Continue' })).toBeNull()

    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} draft={draft} sessionIndexStatus="interrupted" />)
    expect(screen.getByLabelText('Session status: interrupted')).toBeDefined()
    expect((screen.getByRole('textbox') as HTMLTextAreaElement).disabled).toBe(false)
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDefined()
  })

  it('uses a new image reader identity for subsequent stored-image loads', async () => {
    const detail = session('s1')
    const page: ItemsPage = {
      items: [{
        seq: 1, id: 'image-item', created_at: '', kind: 'message', visibility: 'visible', audience: 'user',
        message: { role: 'user', images: [{ hash: 'hash-1', media_type: 'image/png', size_bytes: 1 }] },
      }],
      oldest_seq: 1, newest_seq: 1, has_more_before: false, has_more_after: false,
    }
    const firstReader = vi.fn().mockResolvedValue({ bytes: new Uint8Array([1]), contentType: 'image/png' })
    const secondReader = vi.fn().mockResolvedValue({ bytes: new Uint8Array([2]), contentType: 'image/png' })
    const view = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} loadSessionImage={firstReader} />)
    await waitFor(() => expect(firstReader).toHaveBeenCalledWith('s1', 'hash-1', expect.any(AbortSignal)))
    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} loadSessionImage={secondReader} />)
    await waitFor(() => expect(secondReader).toHaveBeenCalledWith('s1', 'hash-1', expect.any(AbortSignal)))
  })

  it('rerenders the composer when admissionPending toggles through the memo boundary', () => {
    const detail = session('s1')
    const draft = { ...emptyComposerDraft, content: 'pending input' }
    const view = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} draft={draft} admissionPending={false} />)
    const composer = screen.getByRole('textbox') as HTMLTextAreaElement
    const send = screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement
    expect(composer.disabled).toBe(false)
    expect(send.disabled).toBe(false)

    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} draft={draft} admissionPending />)
    expect(composer.disabled).toBe(true)
    expect(send.disabled).toBe(true)

    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} draft={draft} admissionPending={false} />)
    expect(composer.disabled).toBe(false)
    expect(send.disabled).toBe(false)
  })

  it('renders detail when sessionID matches detail.id', () => {
    const detail = session('s1')
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} />)
    expect(screen.getByText('Session s1')).toBeDefined()
  })

  it('shows the session debug button and opens the configured handler', () => {
    const onDebug = vi.fn()
    renderConversation(<Conversation {...baseProps} onDebug={onDebug} sessionID="s1" detail={session('s1')} />)
    fireEvent.click(screen.getByRole('button', { name: /Debug/ }))
    expect(onDebug).toHaveBeenCalledTimes(1)
  })

  it('shows session cost, API request count, and token count when usage is available', () => {
    const detail = {
      ...session('s1'),
      pricing: { input_cache_hit: 0.5, input_cache_miss: 5, cache_write: 6.25, output: 30, currency: 'USD' },
      context: {
        context_window: 1000,
        context_window_source: 'configured',
        warning_threshold_percent: 80,
        total_input_tokens: 100,
        total_output_tokens: 20,
        total_tokens: 120,
        total_requests: 2,
      },
    } as Session
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} />)
    expect(screen.getByText('Cost')).toBeDefined()
    expect(screen.getByText('API requests')).toBeDefined()
    expect(screen.getByText('Tokens')).toBeDefined()
  })

  it('does not render stale detail when sessionID does not match', () => {
    const detail = session('old')
    renderConversation(<Conversation {...baseProps} sessionID="new" detail={detail} />)
    // safeDetail is null → shows "Loading…" not the stale session name
    expect(screen.getByText('Loading…')).toBeDefined()
  })

  it('shows refresh button when activeRun status is error_pending_refresh', () => {
    const detail = session('s1')
    const run: ActiveRun = {
      id: 'run-1', sessionID: 's1', assistantText: '', steps: [],
      agentIteration: 0, status: 'error_pending_refresh',
    }
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
    expect(screen.getByText('Refresh')).toBeDefined()
    expect(screen.getByTitle('Refresh session')).toBeDefined()
  })

  it('renders a compaction item as a one-line record instead of a chat bubble', () => {
    const detail = session('s1')
    const page: ItemsPage = {
      items: [
        { seq: 1, id: 'm1', created_at: '', kind: 'message', visibility: 'visible', audience: 'user', message: { role: 'user', content: { inline: 'hello' } } },
        { seq: 2, id: 'summary-1-record', created_at: '', kind: 'compaction', visibility: 'visible', audience: 'user', message: { role: 'developer', content: { inline: 'Context compacted automatically' } } },
        { seq: 3, id: 'm2', created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'hi there' } } },
      ],
      oldest_seq: 1, newest_seq: 3, has_more_before: false, has_more_after: false,
    }
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} />)
    const record = screen.getByText('Context compacted automatically')
    expect(record.closest('.compaction-record')).not.toBeNull()
    expect(record.closest('.message')).toBeNull()
    expect(container.querySelectorAll('.compaction-record .compaction-record-rule')).toHaveLength(2)
    // The surrounding conversation renders as regular bubbles around the record.
    expect(container.querySelectorAll('.message')).toHaveLength(2)
  })

  it('renders a durable assistant prefix and transient tail as one bubble', () => {
    const detail = session('s1')
    const page: ItemsPage = {
      items: [{
        seq: 2, id: 'assistant-stream', turn_id: 'turn-live', agent_iteration: 1, created_at: '',
        kind: 'message', visibility: 'visible', audience: 'model',
        message: { role: 'assistant', content: { inline: 'a' } },
      }],
      oldest_seq: 2, newest_seq: 2, has_more_before: false, has_more_after: false,
    }
    const activeRun: ActiveRun = {
      id: 'run-1', sessionID: 's1', turnID: 'turn-live', assistantText: 'b', steps: [],
      agentIteration: 1, status: 'running',
      assistantItems: { 'turn-live:1': { itemID: 'assistant-stream', durableTextLength: 1 } },
    }
    const { container, rerender } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} activeRun={activeRun} />)
    expect(container.querySelectorAll('.message.assistant:not(.transient)')).toHaveLength(1)
    expect(container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    expect(container.querySelector('.message.assistant:not(.transient)')?.textContent).toContain('ab')
		expect(container.querySelector('[title="Copy full output"]')).toBeNull()

    const settledPage: ItemsPage = {
      ...page,
      items: [{ ...page.items[0], message: { role: 'assistant', content: { inline: 'ab' } } }],
    }
    rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} page={settledPage} activeRun={{ ...activeRun, assistantText: '', status: 'failed' }} />)
    expect(container.querySelectorAll('.message.assistant:not(.transient)')).toHaveLength(1)
    expect(container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    expect(container.querySelector('.message.assistant:not(.transient)')?.textContent).toContain('ab')
		expect(container.querySelector('[title="Copy full output"]')).not.toBeNull()
  })

	it('hides copy for every durable assistant item owned by the active run but keeps historical run actions', () => {
		const detail = session('s1')
		const page: ItemsPage = {
			items: [
				{ seq: 1, id: 'assistant-old', turn_id: 'turn-old', agent_iteration: 1, created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'old answer' } } },
				{ seq: 2, id: 'assistant-current-1', turn_id: 'turn-current', agent_iteration: 1, created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'first current answer' } } },
				{ seq: 3, id: 'assistant-current-2', turn_id: 'turn-current', agent_iteration: 2, created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'second current answer' } } },
			],
			oldest_seq: 1, newest_seq: 3, has_more_before: false, has_more_after: false,
		}
		const activeRun: ActiveRun = {
			id: 'run-current', sessionID: 's1', turnID: 'turn-current', assistantText: '', steps: [], agentIteration: 2, status: 'running',
			assistantItems: {
				'turn-current:1': { itemID: 'assistant-current-1', durableTextLength: 20 },
				'turn-current:2': { itemID: 'assistant-current-2', durableTextLength: 21 },
			},
		}
		const view = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} activeRun={activeRun} />)
		expect(view.container.querySelectorAll('[title="Copy full output"]')).toHaveLength(1)
		expect(view.container.querySelector('[data-seq="1"] [title="Copy full output"]')).not.toBeNull()
		expect(view.container.querySelector('[data-seq="2"] [title="Copy full output"]')).toBeNull()
		expect(view.container.querySelector('[data-seq="3"] [title="Copy full output"]')).toBeNull()

		view.rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} activeRun={null} />)
		expect(view.container.querySelectorAll('[title="Copy full output"]')).toHaveLength(3)
	})

	it('shows completion time and total duration to the right of the final copy action', () => {
		const page: ItemsPage = {
			items: [
				{ seq: 1, id: 'user-1', turn_id: 'turn-1', created_at: '2026-01-01T00:00:00.000Z', kind: 'message', visibility: 'visible', audience: 'user', message: { role: 'user', content: { inline: 'question' } } },
				{ seq: 2, id: 'assistant-tool', turn_id: 'turn-1', created_at: '2026-01-01T00:00:01.000Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'planning' }, tool_calls: [{ id: 'tool-1', name: 'shell' }] } },
				{ seq: 3, id: 'tool-result', turn_id: 'turn-1', created_at: '2026-01-01T00:00:01.500Z', kind: 'tool', visibility: 'visible', audience: 'user', message: { role: 'tool', content: { inline: 'done' }, tool_call_id: 'tool-1' } },
				{ seq: 4, id: 'assistant-final', turn_id: 'turn-1', created_at: '2026-01-01T00:00:02.000Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'answer' } } },
			],
			oldest_seq: 1, newest_seq: 4, has_more_before: false, has_more_after: false,
		}
		const view = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')} page={page} />)
		const { container } = view
		const finalMessage = container.querySelector('[data-seq="4"]')
		const tools = finalMessage?.querySelector('.message-tools')

		expect(finalMessage?.querySelector('.message-completion')).not.toBeNull()
		expect(finalMessage?.querySelector('.message-completion')?.textContent).toContain('Completed')
		expect(finalMessage?.querySelector('.message-completion')?.textContent).toContain('Duration 2s')
		expect(tools?.firstElementChild?.getAttribute('title')).toBe('Copy full output')
		expect(container.querySelector('[data-seq="2"] .message-completion')).toBeNull()

		const activeRun = { id: 'run-active', sessionID: 's1', turnID: 'turn-1', assistantText: '', steps: [], agentIteration: 1, status: 'running' as const }
		view.rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')} page={page} activeRun={activeRun} />)
		expect(view.container.querySelector('[data-seq="4"] .message-completion')).toBeNull()

		view.rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')} page={page} activeRun={{ ...activeRun, status: 'failed' }} />)
		expect(view.container.querySelector('[data-seq="4"] .message-completion')).toBeNull()
	})

  it('splits one turn into separate process groups around a compaction record without key collisions', () => {
    const detail = session('s1')
    const toolCallItem = (seq: number, callID: string) => ({
      seq, id: `asst-${callID}`, turn_id: 'turn-1', created_at: '', kind: 'message', visibility: 'visible', audience: 'model',
      message: { role: 'assistant', content: { inline: '' }, tool_calls: [{ id: callID, name: 'bash' }] },
    })
    const toolResultItem = (seq: number, callID: string, result: string) => ({
      seq, id: `tool-${callID}`, turn_id: 'turn-1', created_at: '', kind: 'message', visibility: 'visible', audience: 'model',
      message: { role: 'tool', tool_call_id: callID, content: { inline: result } },
    })
    const page: ItemsPage = {
      items: [
        { seq: 1, id: 'u1', turn_id: 'turn-1', created_at: '', kind: 'message', visibility: 'visible', audience: 'user', message: { role: 'user', content: { inline: 'run tools' } } },
        toolCallItem(2, 'tc-1'),
        toolResultItem(3, 'tc-1', 'result one'),
        { seq: 4, id: 'summary-1-record', turn_id: 'turn-1', created_at: '', kind: 'compaction', visibility: 'visible', audience: 'user', message: { role: 'developer', content: { inline: 'Context compacted automatically' } } },
        toolCallItem(5, 'tc-2'),
        toolResultItem(6, 'tc-2', 'result two'),
        { seq: 7, id: 'a-final', turn_id: 'turn-1', created_at: '', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'done' } } },
      ],
      oldest_seq: 1, newest_seq: 7, has_more_before: false, has_more_after: false,
    }
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} page={page} />)
      expect(container.querySelectorAll('.process-message')).toHaveLength(2)
      expect(screen.getByText('Context compacted automatically').closest('.compaction-record')).not.toBeNull()
      const keyWarnings = errorSpy.mock.calls.filter((args) => String(args[0]).includes('same key'))
      expect(keyWarnings).toHaveLength(0)
    } finally {
      errorSpy.mockRestore()
    }
  })

  it('counts down the provider retry delay, then switches to the reconnecting label', () => {
    vi.useFakeTimers()
    try {
      const detail = session('s1')
      const run: ActiveRun = {
        id: 'run-1', sessionID: 's1', assistantText: '', steps: [],
        agentIteration: 1, status: 'running',
        providerRetry: { attempt: 3, maxAttempts: 5, delayMS: 10000 },
      }
      renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
      expect(screen.getByText(/Retrying request 3 of 5 in 10s/)).toBeDefined()
      act(() => { vi.advanceTimersByTime(4100) })
      expect(screen.getByText(/Retrying request 3 of 5 in 6s/)).toBeDefined()
      act(() => { vi.advanceTimersByTime(6000) })
      expect(screen.queryByText(/Retrying request/)).toBeNull()
      expect(screen.getByText(/Reconnecting \(attempt 3 of 5\)/)).toBeDefined()
    } finally {
      vi.useRealTimers()
    }
  })

  it('restarts the provider retry countdown when a new attempt arrives', () => {
    vi.useFakeTimers()
    try {
      const detail = session('s1')
      const run: ActiveRun = {
        id: 'run-1', sessionID: 's1', assistantText: '', steps: [],
        agentIteration: 1, status: 'running',
        providerRetry: { attempt: 3, maxAttempts: 5, delayMS: 10000 },
      }
      const { rerender } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
      act(() => { vi.advanceTimersByTime(5000) })
      expect(screen.getByText(/in 5s/)).toBeDefined()
      // The retried connection failed again: attempt 4 with a fresh 20s delay.
      const next: ActiveRun = { ...run, providerRetry: { attempt: 4, maxAttempts: 5, delayMS: 20000 } }
      rerender(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={next} />)
      expect(screen.getByText(/Retrying request 4 of 5 in 20s/)).toBeDefined()
    } finally {
      vi.useRealTimers()
    }
  })

  it('does not show refresh button when activeRun is running', () => {
    const detail = session('s1')
    const run: ActiveRun = {
      id: 'run-1', sessionID: 's1', assistantText: '', steps: [],
      agentIteration: 0, status: 'running',
    }
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
    expect(screen.queryByText('Refresh session')).toBeNull()
  })
})

describe('Conversation queued prompt list', () => {
  const runWithQueue = (queuedPrompts: ActiveRun['queuedPrompts']): ActiveRun => ({
    id: 'run-1', sessionID: 's1', assistantText: '', steps: [],
    agentIteration: 0, status: 'running', queuedPrompts,
  })

  it('renders steer prompts ahead with distinct badges and a demote button', () => {
    const detail = session('s1')
    const run = runWithQueue([
      { id: 'ap-2', content: 'priority', steer: true },
      { id: 'ap-1', content: 'plain' },
    ])
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
    const list = screen.getByLabelText('Queued messages')
    const badges = Array.from(list.querySelectorAll('.queued-prompt-badge')).map((badge) => badge.textContent)
    expect(badges).toEqual(['Steer', 'Queued'])
    const texts = Array.from(list.querySelectorAll('.queued-prompt-text')).map((node) => node.textContent)
    expect(texts).toEqual(['priority', 'plain'])
    // The steer row offers demotion back to the plain queue.
    expect(screen.getByRole('button', { name: 'Demote to queued message' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Promote to steer message' })).toBeDefined()
  })

  it('fires steer toggle with the prompt id and target state', () => {
    const detail = session('s1')
    const onSteerQueuedPrompt = vi.fn()
    const run = runWithQueue([
      { id: 'ap-2', content: 'priority', steer: true },
      { id: 'ap-1', content: 'plain' },
    ])
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} onSteerQueuedPrompt={onSteerQueuedPrompt} />)
    fireEvent.click(screen.getByRole('button', { name: 'Promote to steer message' }))
    expect(onSteerQueuedPrompt).toHaveBeenCalledWith('ap-1', true)
    fireEvent.click(screen.getByRole('button', { name: 'Demote to queued message' }))
    expect(onSteerQueuedPrompt).toHaveBeenCalledWith('ap-2', false)
  })

  it('fires move callbacks and disables reordering at group boundaries', () => {
    const detail = session('s1')
    const onMoveQueuedPrompt = vi.fn()
    const run = runWithQueue([
      { id: 'ap-3', content: 'priority', steer: true },
      { id: 'ap-1', content: 'plain one' },
      { id: 'ap-2', content: 'plain two' },
    ])
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} onMoveQueuedPrompt={onMoveQueuedPrompt} />)
    const ups = screen.getAllByRole('button', { name: 'Move queued message up' }) as HTMLButtonElement[]
    const downs = screen.getAllByRole('button', { name: 'Move queued message down' }) as HTMLButtonElement[]
    expect(ups).toHaveLength(3)
    expect(downs).toHaveLength(3)
    // Steer group top and plain group top cannot move up; the steer cannot
    // sink into the plain group, and the last row cannot move down.
    expect(ups[0].disabled).toBe(true)
    expect(ups[1].disabled).toBe(true)
    expect(ups[2].disabled).toBe(false)
    expect(downs[0].disabled).toBe(true)
    expect(downs[1].disabled).toBe(false)
    expect(downs[2].disabled).toBe(true)
    fireEvent.click(ups[2])
    expect(onMoveQueuedPrompt).toHaveBeenCalledWith('ap-2', 'up')
    fireEvent.click(downs[1])
    expect(onMoveQueuedPrompt).toHaveBeenCalledWith('ap-1', 'down')
  })
})

describe('Conversation live cursor', () => {
  const runWith = (overrides: Partial<ActiveRun>): ActiveRun => ({
    id: 'run-1', sessionID: 's1', assistantText: '', steps: [],
    agentIteration: 0, status: 'running', ...overrides,
  })

  it('shows one placeholder cursor after a run starts before any item or delta', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({})} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(1)
    expect(container.querySelectorAll('.active-cursor')).toHaveLength(1)
    expect(container.querySelectorAll('.reasoning-step')).toHaveLength(0)
    expect(container.querySelectorAll('.process-timeline')).toHaveLength(0)
  })

  it('shows one cursor after reasoning without creating an empty reasoning step', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ steps: [{ kind: 'reasoning', id: 'r1', text: 'thinking', iteration: 1 }] })} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(1)
    expect(container.querySelectorAll('.reasoning-step')).toHaveLength(1)
    expect(container.querySelectorAll('.reasoning-step pre')).toHaveLength(0)
    expect(container.querySelector('.reasoning-step .reasoning-trigger')?.textContent).toContain('Thinking')
    expect(container.querySelector('[aria-label="Reasoning status: Thinking"]')).not.toBeNull()
    expect(container.querySelectorAll('.active-cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.process-timeline')).toHaveLength(1)
    expect(container.querySelector('.message.assistant.transient .cursor')).not.toBeNull()
  })

  it('removes the reasoning dot when output owns the still-running tail', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({
        assistantText: 'partial output',
        steps: [{ kind: 'reasoning', id: 'r-output', text: 'thinking', iteration: 1 }],
      })} />)
    expect(container.querySelector('.reasoning-step')).not.toBeNull()
    expect(container.querySelector('[aria-label="Reasoning status: Thinking"]')).toBeNull()
    expect(container.querySelector('.assistant-stream')).not.toBeNull()
  })

  it('keeps one cursor and the live tool status for a requested tool', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ steps: [{ kind: 'tool', id: 't1', name: 'shell', iteration: 1, status: 'requested' }] })} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(1)
    expect(container.querySelectorAll('.tool-row')).toHaveLength(1)
    expect(container.querySelector('.tool-row.requested')).not.toBeNull()
  })

  it('shows the cursor after transient text in the same text container', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ assistantText: 'partial output' })} />)
    const stream = container.querySelector('.assistant-stream')
    expect(stream).not.toBeNull()
    expect(stream?.querySelectorAll('.cursor')).toHaveLength(1)
    expect(container.querySelectorAll('.cursor')).toHaveLength(1)
  })

  it('keeps one cursor through reasoning, tool, and text state changes', () => {
    const view = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ steps: [{ kind: 'reasoning', id: 'r1', text: 'thinking', iteration: 1 }] })} />)
    expect(view.container.querySelectorAll('.cursor')).toHaveLength(1)
    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ steps: [{ kind: 'tool', id: 't1', name: 'shell', iteration: 1, status: 'running' }] })} />)
    expect(view.container.querySelectorAll('.cursor')).toHaveLength(1)
    expect(view.container.querySelector('.tool-row.running')).not.toBeNull()
    view.rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ assistantText: 'partial output' })} />)
    expect(view.container.querySelectorAll('.cursor')).toHaveLength(1)
  })

  it('moves the cursor to the bound durable assistant item and not the active process', () => {
    const page: ItemsPage = {
      items: [{
        seq: 1, id: 'assistant-1', turn_id: 'turn-live', agent_iteration: 1, created_at: '',
        kind: 'message', visibility: 'visible', audience: 'model',
        message: { role: 'assistant', content: { inline: 'answer' } },
      }],
      oldest_seq: 1, newest_seq: 1, has_more_before: false, has_more_after: false,
    }
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')} page={page}
      activeRun={runWith({
        turnID: 'turn-live', agentIteration: 1, assistantText: ' tail',
        steps: [{ kind: 'reasoning', id: 'stale-reasoning', text: 'stale', iteration: 1, turnID: 'turn-live', itemID: 'assistant-1' }],
        assistantItems: { 'turn-live:1': { itemID: 'assistant-1', durableTextLength: 6 } },
      })} />)
    const message = container.querySelector('.message.assistant:not(.transient)')
    expect(container.querySelectorAll('.cursor')).toHaveLength(1)
    expect(message?.querySelector('.cursor')).not.toBeNull()
    expect(container.querySelectorAll('.active-process .cursor')).toHaveLength(0)
  })

  it('hides the cursor once the run leaves the running state', () => {
    const { container, rerender } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ status: 'reconciling' })} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.active-cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ status: 'failed' })} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.active-cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ status: 'cancelled' })} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.active-cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ assistantText: 'partial output', status: 'reconciling' })} />)
    expect(container.querySelectorAll('.cursor')).toHaveLength(0)
    expect(container.querySelectorAll('.active-cursor')).toHaveLength(1)
    expect(container.querySelector('.active-cursor')?.textContent).toContain('partial output')
  })
})
