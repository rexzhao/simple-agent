// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, fireEvent } from '@testing-library/react'
import { Conversation } from './Conversation'
import type { ActiveRun, ItemsPage, Session } from '../types'
import { emptyComposerDraft } from './Composer'

afterEach(cleanup)

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
  onResend: vi.fn(),
  onLoadOlder: vi.fn(),
  onSend: vi.fn(),
  onCancel: vi.fn(),
  onCancelTool: vi.fn(),
  onRetry: vi.fn(),
  onRetryRefresh: vi.fn(),
  onToggleFullAccess: vi.fn(),
  onRemoveQueuedPrompt: vi.fn(),
  onSteerQueuedPrompt: vi.fn(),
  onMoveQueuedPrompt: vi.fn(),
  onCompact: vi.fn(),
}

describe('Conversation identity boundary', () => {
  it('renders detail when sessionID matches detail.id', () => {
    const detail = session('s1')
    render(<Conversation {...baseProps} sessionID="s1" detail={detail} />)
    expect(screen.getByText('Session s1')).toBeDefined()
  })

  it('does not render stale detail when sessionID does not match', () => {
    const detail = session('old')
    render(<Conversation {...baseProps} sessionID="new" detail={detail} />)
    // safeDetail is null → shows "Loading…" not the stale session name
    expect(screen.getByText('Loading…')).toBeDefined()
  })

  it('shows refresh button when activeRun status is error_pending_refresh', () => {
    const detail = session('s1')
    const run: ActiveRun = {
      id: 'run-1', sessionID: 's1', userText: '', assistantText: '', steps: [],
      agentIteration: 0, status: 'error_pending_refresh',
    }
    render(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
    expect(screen.getByText('Refresh')).toBeDefined()
    expect(screen.getByTitle('Refresh session')).toBeDefined()
  })

  it('does not show refresh button when activeRun is running', () => {
    const detail = session('s1')
    const run: ActiveRun = {
      id: 'run-1', sessionID: 's1', userText: 'hi', assistantText: '', steps: [],
      agentIteration: 0, status: 'running',
    }
    render(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
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
    render(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} />)
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
    render(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} onSteerQueuedPrompt={onSteerQueuedPrompt} />)
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
    render(<Conversation {...baseProps} sessionID="s1" detail={detail} activeRun={run} onMoveQueuedPrompt={onMoveQueuedPrompt} />)
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
