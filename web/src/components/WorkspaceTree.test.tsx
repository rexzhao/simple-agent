// @vitest-environment jsdom
import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Project, Session } from '../types'
import type { SessionIndexReadModel, SessionSummary } from '../repositories/sessionIndex'
import { WorkspaceTree } from './WorkspaceTree'

const project: Project = {
  id: 'project-1', root: '/workspace', display_name: 'Workspace', archived: false,
  created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
}

function session(id: string, name: string, options: Partial<Session> = {}): Session {
  return {
    id,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    display_name: name,
    created_by: 'user',
    root_session_id: id,
    spawn_depth: 0,
    archived: false,
    last_used_at: '2026-01-01T00:00:00Z',
    provider: 'fake',
    model_profile: 'default',
    model_id: 'fake-model',
    project_id: project.id,
    created_cwd: project.root,
    last_seq: 0,
    full_access: false,
    ...options,
  }
}

function renderTree(sessions: Session[], runningSessionIDs: ReadonlySet<string> = new Set(), selectedSessionID = '') {
  const onSelectSession = vi.fn()
  const summaries: SessionSummary[] = sessions.map((item) => ({
    session_id: item.id,
    project_id: item.project_id,
    parent_session_id: item.parent_session_id ?? null,
    display_name: item.display_name,
    archived: item.archived,
    status: item.status === 'running' || item.status === 'completed' || item.status === 'failed' || item.status === 'interrupted' ? item.status : 'idle',
    run_id: item.status === 'running' ? item.running_run_id ?? 'run-test' : null,
    resource_revision: item.revision ?? '0',
    updated_at: item.updated_at,
    has_unread_result: false,
  }))
  const active = summaries.filter((summary) => !summary.archived)
  const archived = summaries.filter((summary) => summary.archived)
  const sessionIndexes: Record<string, SessionIndexReadModel> = {
    [project.id]: { status: 'ready', summaries, active, archived },
  }
  render(<WorkspaceTree
    projects={[project]}
    sessionIndexes={sessionIndexes}
    selectedProjectID={project.id}
    selectedSessionID={selectedSessionID}
    runningSessionIDs={runningSessionIDs}
    version="test"
    onSelectProject={vi.fn()}
    onSelectSession={onSelectSession}
    onCreateSession={vi.fn()}
    onManageProviders={vi.fn()}
    onRenameProject={vi.fn()}
    onDeleteProject={vi.fn()}
    onRenameSession={vi.fn()}
    onArchiveSession={vi.fn()}
    onRestoreSession={vi.fn()}
    onDeleteSession={vi.fn()}
    onRetrySessionIndex={vi.fn()}
    onAdd={vi.fn()}
  />)
  return onSelectSession
}

afterEach(() => { document.body.innerHTML = '' })

describe('WorkspaceTree session list', () => {
  it('shows only root sessions, not child sessions', () => {
    const root = session('root', 'Root')
    const child = session('child', 'Child', {
      created_by: 'agent', parent_session_id: root.id, root_session_id: root.id, spawn_depth: 1,
    })
    renderTree([child, root])

    expect(screen.getByText('Root')).not.toBeNull()
    expect(screen.queryByText('Child')).toBeNull()
  })

  it('keeps archived children out of the project rail', () => {
    const root = session('root', 'Root')
    const archivedChild = session('archived-child', 'Archived child', {
      created_by: 'agent', parent_session_id: root.id, root_session_id: root.id, spawn_depth: 1, archived: true,
    })
    renderTree([root, archivedChild])

    expect(screen.getByText('Root')).not.toBeNull()
    expect(screen.queryByText('Archived child')).toBeNull()
    expect(screen.queryByRole('button', { name: /Archived \(1\)/ })).toBeNull()
  })

  it('keeps a running root visible when it is outside the collapsed top three', () => {
    const roots = [1, 2, 3, 4].map((index) => session(`root-${index}`, `Root ${index}`, {
      last_used_at: `2026-01-0${6 - index}T00:00:00Z`,
      status: 'running',
    }))
    renderTree(roots, new Set([roots[3].id]))

    expect(screen.getByText('Root 4')).not.toBeNull()
    expect(screen.getByText('Root 4').closest('.session-tree-row')?.querySelector('.session-icon.running')).not.toBeNull()
    expect(screen.getByText('Root 4').closest('.session-tree-row')?.querySelector('.status-dot')).toBeNull()
  })

  it('uses the session icon for completed, idle, failed, and interrupted results', () => {
    const completed = session('completed', 'Completed', { status: 'completed' })
    const failed = session('failed', 'Failed', { status: 'interrupted' })
    const failedRun = session('failed-run', 'Failed run', { status: 'failed' })
    const idle = session('idle', 'Idle', { status: 'idle' })
    renderTree([completed, failed, failedRun, idle])
    fireEvent.click(screen.getByRole('button', { name: /Show 1 more sessions/ }))

    expect(screen.getByText('Completed').closest('.session-tree-row')?.querySelector('.session-icon.completed')).not.toBeNull()
    expect(screen.getByText('Failed').closest('.session-tree-row')?.querySelector('.session-icon.interrupted')).not.toBeNull()
    expect(screen.getByText('Failed run').closest('.session-tree-row')?.querySelector('.session-icon.failed')).not.toBeNull()
    expect(screen.getByText('Idle').closest('.session-tree-row')?.querySelector('.session-icon.idle')).not.toBeNull()
    expect(document.querySelectorAll('.session-tree-button .status-dot')).toHaveLength(0)
  })

  it('shows a dashed running icon on a root whose sub-session is running', () => {
    const root = session('root', 'Root')
    const child = session('child', 'Child', {
      created_by: 'agent', parent_session_id: root.id, root_session_id: root.id, spawn_depth: 1,
    })
    renderTree([child, root], new Set([child.id]))

    const rootRow = screen.getByText('Root').closest('.session-tree-row')!
    // The root is not itself running, so it must not get the solid running icon.
    expect(rootRow.querySelector('.session-icon.running')).toBeNull()
    expect(rootRow.querySelector('.session-icon.running-descendant')).not.toBeNull()
    expect(rootRow.querySelector('.session-icon')?.getAttribute('aria-label')).toBe('A sub-session is running')
    expect(rootRow.querySelector('.status-dot')).toBeNull()
    expect(rootRow.classList.contains('running-descendant')).toBe(false)
    expect(rootRow.classList.contains('selected')).toBe(false)
    expect(rootRow.classList.contains('hover')).toBe(false)
  })

  it('does not mark a root with a running-descendant when it is itself running', () => {
    const root = session('root', 'Root', { status: 'running' })
    const child = session('child', 'Child', {
      created_by: 'agent', parent_session_id: root.id, root_session_id: root.id, spawn_depth: 1,
    })
    renderTree([child, root], new Set([root.id, child.id]))

    const rootRow = screen.getByText('Root').closest('.session-tree-row')!
    expect(rootRow.querySelector('.session-icon.running')).not.toBeNull()
    expect(rootRow.querySelector('.session-icon.running-descendant')).toBeNull()
    expect(rootRow.querySelector('.status-dot')).toBeNull()
  })

  it('keeps the status icon legible on a selected session row', () => {
    const running = session('running', 'Running', { status: 'running' })
    renderTree([running], new Set([running.id]), running.id)

    const row = screen.getByText('Running').closest('.session-tree-row')!
    const icon = row.querySelector('.session-icon')!
    expect(row.classList.contains('selected')).toBe(true)
    expect(icon.classList.contains('running')).toBe(true)
    expect(icon.classList.contains('running-descendant')).toBe(false)
    expect(row.querySelector('.status-dot')).toBeNull()
  })

  it('keeps the archive icon separate from chat status styling', () => {
    const archived = session('archived', 'Archived', { archived: true, status: 'failed' })
    renderTree([archived])

    fireEvent.click(screen.getByRole('button', { name: /Archived \(1\)/ }))
    const row = screen.getByText('Archived').closest('.session-tree-row')!
    expect(row.querySelector('.session-icon')?.classList.contains('session-icon')).toBe(true)
    expect(row.querySelector('.session-icon')?.classList.contains('failed')).toBe(false)
    expect(row.querySelector('.session-icon svg')).not.toBeNull()
  })

  it('selects a root session when clicked', () => {
    const root = session('root', 'Root')
    const onSelectSession = renderTree([root])
    fireEvent.click(screen.getByText('Root'))
    expect(onSelectSession).toHaveBeenCalledWith(project.id, root.id)
  })
})
