// @vitest-environment jsdom
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { Project, Session } from '../types'
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
    ...options,
  }
}

function renderTree(sessions: Session[], runningSessionIDs: ReadonlySet<string> = new Set()) {
  const onSelectSession = vi.fn()
  render(<WorkspaceTree
    projects={[project]}
    sessionsByProject={{ [project.id]: sessions }}
    archivedSessionsByProject={{ [project.id]: [] }}
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
    onAdd={vi.fn()}
  />)
  return onSelectSession
}

describe('WorkspaceTree session lineage', () => {
  it('renders, collapses, expands, and selects an agent child beneath its parent', () => {
    const root = session('root', 'Root')
    const child = session('child', 'Child', {
      created_by: 'agent', parent_session_id: root.id, root_session_id: root.id, spawn_depth: 1,
    })
    const onSelectSession = renderTree([child, root])

    let childLabel = screen.getByText('Child')
    expect(childLabel.closest('.session-tree-row')?.parentElement?.parentElement?.classList.contains('session-tree-children')).toBe(true)
    expect(screen.getByText(/Agent/)).not.toBeNull()

    const collapse = screen.getByRole('button', { name: 'Collapse child sessions of Root' })
    expect(collapse.getAttribute('aria-expanded')).toBe('true')
    fireEvent.click(collapse)
    expect(screen.queryByText('Child')).toBeNull()

    const expand = screen.getByRole('button', { name: 'Expand child sessions of Root' })
    expect(expand.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(expand)
    childLabel = screen.getByText('Child')
    fireEvent.click(childLabel)
    expect(onSelectSession).toHaveBeenCalledWith(project.id, child.id)
  })

  it('keeps a running descendant visible when its root is outside the collapsed top three', () => {
    const roots = [1, 2, 3, 4].map((index) => session(`root-${index}`, `Root ${index}`, {
      last_used_at: `2026-01-0${6 - index}T00:00:00Z`,
    }))
    const runningChild = session('running-child', 'Running child', {
      created_by: 'agent', parent_session_id: roots[3].id, root_session_id: roots[3].id,
      spawn_depth: 1, status: 'running', last_used_at: '2026-01-01T00:00:00Z',
    })
    renderTree([...roots, runningChild], new Set([runningChild.id]))

    expect(screen.getByText('Root 4')).not.toBeNull()
    expect(screen.getByText('Running child')).not.toBeNull()
    expect(screen.getByText('Running child').closest('.session-tree-row')?.querySelector('.live-dot')).not.toBeNull()
  })
})
