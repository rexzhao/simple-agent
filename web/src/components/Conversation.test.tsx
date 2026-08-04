// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, fireEvent } from '@testing-library/react'
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
  recentStepsByTurn: {},
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
      id: 'run-1', sessionID: 's1', userText: '', assistantText: '', steps: [],
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
        id: 'run-1', sessionID: 's1', userText: 'hi', assistantText: '', steps: [],
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
        id: 'run-1', sessionID: 's1', userText: 'hi', assistantText: '', steps: [],
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
      id: 'run-1', sessionID: 's1', userText: 'hi', assistantText: '', steps: [],
      agentIteration: 0, status: 'running',
    }
    renderConversation(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
    expect(screen.queryByText('Refresh session')).toBeNull()
  })
})

describe('Conversation queued prompt list', () => {
  const runWithQueue = (queuedPrompts: ActiveRun['queuedPrompts']): ActiveRun => ({
    id: 'run-1', sessionID: 's1', userText: 'hi', assistantText: '', steps: [],
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

describe('Conversation streaming cursor', () => {
  const runWith = (overrides: Partial<ActiveRun>): ActiveRun => ({
    id: 'run-1', sessionID: 's1', userText: 'hi', assistantText: '', steps: [],
    agentIteration: 0, status: 'running', ...overrides,
  })

  it('shows the cursor after streaming text while the turn is running', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ assistantText: 'partial output' })} />)
    const stream = container.querySelector('.assistant-stream')
    expect(stream).not.toBeNull()
    expect(stream?.querySelector('.cursor')).not.toBeNull()
  })

  it('keeps the cursor visible between iterations when tools run before any usage update', () => {
    const { container } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ steps: [{ kind: 'tool', id: 't1', name: 'shell', iteration: 1, status: 'running' }] })} />)
    expect(container.querySelector('.cursor')).not.toBeNull()
  })

  it('hides the cursor once the run leaves the running state', () => {
    const { container, rerender } = renderConversation(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ assistantText: 'partial output', status: 'reconciling' })} />)
    expect(container.querySelector('.cursor')).toBeNull()
    rerender(<Conversation {...baseProps} sessionID="s1" detail={session('s1')}
      activeRun={runWith({ status: 'failed' })} />)
    expect(container.querySelector('.cursor')).toBeNull()
  })
})
