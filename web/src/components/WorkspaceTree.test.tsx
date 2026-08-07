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

function renderTree(sessions: Session[], runningSessionIDs: ReadonlySet<string> = new Set()) {
  const onSelectSession = vi.fn()
  const summaries: SessionSummary[] = sessions.map((item) => ({
    session_id: item.id,
    project_id: item.project_id,
    parent_session_id: item.parent_session_id ?? null,
    display_name: item.display_name,
    archived: item.archived,
    status: (item.status === 'running' ? 'running' : item.status === 'interrupted' ? 'interrupted' : 'idle'),
    run_id: item.status === 'running' ? item.running_run_id ?? 'run-test' : null,
    resource_revision: item.revision ?? '0',
    updated_at: item.updated_at,
    has_unread_result: false,
  }))
  const sessionIndexes: Record<string, SessionIndexReadModel> = {
    [project.id]: { status: 'ready', summaries, active: summaries, archived: [] },
  }
  render(<WorkspaceTree
    projects={[project]}
    sessionIndexes={sessionIndexes}
    selectedProjectID={project.id}
    selectedSessionID=""
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

  it('keeps a running root visible when it is outside the collapsed top three', () => {
    const roots = [1, 2, 3, 4].map((index) => session(`root-${index}`, `Root ${index}`, {
      last_used_at: `2026-01-0${6 - index}T00:00:00Z`,
      status: 'running',
    }))
    renderTree(roots, new Set([roots[3].id]))

    expect(screen.getByText('Root 4')).not.toBeNull()
    expect(screen.getByText('Root 4').closest('.session-tree-row')?.querySelector('.status-dot.running')).not.toBeNull()
  })

  it('shows a red interrupted indicator on failed sessions in the list', () => {
    const failed = session('failed', 'Failed', { status: 'interrupted' })
    const idle = session('idle', 'Idle', { status: 'idle' })
    renderTree([failed, idle])

    expect(screen.getByText('Failed').closest('.session-tree-row')?.querySelector('.status-dot.interrupted')).not.toBeNull()
    expect(screen.getByText('Idle').closest('.session-tree-row')?.querySelector('.status-dot')).toBeNull()
  })

  it('selects a root session when clicked', () => {
    const root = session('root', 'Root')
    const onSelectSession = renderTree([root])
    fireEvent.click(screen.getByText('Root'))
    expect(onSelectSession).toHaveBeenCalledWith(project.id, root.id)
  })
})
