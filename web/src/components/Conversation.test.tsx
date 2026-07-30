// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { Conversation } from './Conversation'
import type { ActiveRun, ItemsPage, Session } from '../types'
import { emptyComposerDraft } from './Composer'

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
  otherSessionsRunning: false,
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
