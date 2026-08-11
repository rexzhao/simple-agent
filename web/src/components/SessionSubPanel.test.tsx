// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Session } from '../types'
import { SessionSubPanel } from './SessionSubPanel'
import { sessionSubPanelContext, type SessionSubPanelContext } from '../lib/session'

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
    project_id: 'project-1',
    created_cwd: '/workspace',
    last_seq: 0,
    full_access: false,
    ...options,
  }
}

function renderPanel(ctx: SessionSubPanelContext, viewingID: string, runningIDs: ReadonlySet<string> = new Set()) {
  const onSelectSession = vi.fn()
  render(<SessionSubPanel
    context={ctx}
    viewingSessionID={viewingID}
    runningSessionIDs={runningIDs}
    onSelectSession={onSelectSession}
    onRenameSession={vi.fn()}
    onArchiveSession={vi.fn()}
    onRestoreSession={vi.fn()}
    onDeleteSession={vi.fn()}
  />)
  return onSelectSession
}

function tabByLabel(label: string): HTMLElement {
  return screen.getByText(label, { selector: '.sub-panel-tab-label' })
}

afterEach(cleanup)

describe('SessionSubPanel', () => {
  it('renders parent at top and children below, newest first', () => {
    const parent = session('parent', 'Parent')
    const older = session('c1', 'Child 1', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1, created_at: '2026-01-02T00:00:00Z' })
    const newer = session('c2', 'Child 2', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1, created_at: '2026-01-04T00:00:00Z' })

    const ctx = { parent, children: [newer, older] }
    renderPanel(ctx, 'parent')

    const labels = screen.getAllByText(/Parent|Child/, { selector: '.sub-panel-tab-label' }).map((el) => el.textContent)
    // Parent first, then Child 2 (newer), then Child 1 (older)
    expect(labels).toEqual(['Parent', 'Child 2', 'Child 1'])
  })

  it('calls onSelectSession when a tab is clicked', () => {
    const parent = session('parent', 'Parent')
    const child = session('c1', 'Child 1', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1 })

    const ctx = { parent, children: [child] }
    const onSelect = renderPanel(ctx, 'parent')

    fireEvent.click(tabByLabel('Child 1'))
    expect(onSelect).toHaveBeenCalledWith('c1')
  })

  it('minimizes and restores via the minimize button', () => {
    const parent = session('parent', 'Parent')
    const child = session('c1', 'Child 1', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1 })

    const ctx = { parent, children: [child] }
    renderPanel(ctx, 'parent')

    expect(tabByLabel('Child 1')).not.toBeNull()

    const minimize = screen.getByRole('button', { name: 'Minimize sub-sessions panel' })
    fireEvent.click(minimize)

    // Minimized: tabs hidden, restore button visible
    expect(screen.queryByText('Child 1', { selector: '.sub-panel-tab-label' })).toBeNull()
    expect(screen.queryByText('Parent', { selector: '.sub-panel-tab-label' })).toBeNull()

    const restore = screen.getByRole('button', { name: 'Expand sub-sessions panel' })
    fireEvent.click(restore)
    expect(tabByLabel('Child 1')).not.toBeNull()
  })

  it('shows a count badge when minimized', () => {
    const parent = session('parent', 'Parent')
    const child1 = session('c1', 'Child 1', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1 })
    const child2 = session('c2', 'Child 2', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1 })

    const ctx = { parent, children: [child1, child2] }
    renderPanel(ctx, 'parent')

    fireEvent.click(screen.getByRole('button', { name: 'Minimize sub-sessions panel' }))
    expect(screen.getByText('2')).not.toBeNull()
  })

  it('shows running status dot for a running child', () => {
    const parent = session('parent', 'Parent')
    const child = session('c1', 'Child 1', {
      parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1,
      status: 'running',
    })

    const ctx = { parent, children: [child] }
    renderPanel(ctx, 'parent', new Set([child.id]))

    const childTab = tabByLabel('Child 1').closest('button')
    expect(childTab?.querySelector('.status-dot.running')).not.toBeNull()
  })

  it('keeps archived children in a collapsed group with restore and delete actions', () => {
    const parent = session('parent', 'Parent')
    const activeChild = session('active-child', 'Active child', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1 })
    const archivedChild = session('archived-child', 'Archived child', { parent_session_id: 'parent', root_session_id: 'parent', spawn_depth: 1, archived: true })
    const onRestore = vi.fn()
    const onDelete = vi.fn()
    render(<SessionSubPanel
      context={{ parent, children: [activeChild], archivedChildren: [archivedChild] }}
      viewingSessionID="parent"
      runningSessionIDs={new Set()}
      onSelectSession={vi.fn()}
      onRenameSession={vi.fn()}
      onArchiveSession={vi.fn()}
      onRestoreSession={onRestore}
      onDeleteSession={onDelete}
    />)

    expect(tabByLabel('Active child')).not.toBeNull()
    expect(screen.queryByText('Archived child', { selector: '.sub-panel-tab-label' })).toBeNull()
    const archivedToggle = screen.getByRole('button', { name: /Archived \(1\)/ })
    expect(archivedToggle.getAttribute('aria-expanded')).toBe('false')

    fireEvent.click(archivedToggle)
    expect(screen.getByText('Archived child', { selector: '.sub-panel-tab-label' })).not.toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Restore Archived child' }))
    fireEvent.click(screen.getByRole('button', { name: 'Delete Archived child' }))
    expect(onRestore).toHaveBeenCalledWith(archivedChild)
    expect(onDelete).toHaveBeenCalledWith(archivedChild)
  })

  it('does not offer child restore while the root is archived', () => {
    const parent = session('archived-parent', 'Archived parent', { archived: true })
    const descendant = session('descendant', 'Descendant', {
      parent_session_id: parent.id, root_session_id: parent.id, spawn_depth: 1, archived: false,
    })
    const context = sessionSubPanelContext([parent, descendant], parent.id)
    expect(context?.children).toEqual([])
    expect(context?.archivedChildren).toEqual([descendant])

    renderPanel(context!, parent.id)
    fireEvent.click(screen.getByRole('button', { name: /Archived \(1\)/ }))
    const restore = screen.getByRole('button', { name: 'Restore Descendant' })
    expect(restore.hasAttribute('disabled')).toBe(true)
    expect(restore.getAttribute('title')).toBe('Restore the root session first')
    expect(screen.getByRole('button', { name: 'Delete Descendant' })).not.toBeNull()
  })
})
