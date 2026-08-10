import { memo, useState } from 'react'
import type { ProjectSummary } from '../repositories/projectIndex'
import type { SessionIndexReadModel } from '../repositories/sessionIndex'
import { ancestorSessionIDs, buildSessionTree, flattenSessionTree, navigationSession, projectName, sessionName, sessionTreeContains, type SessionNavigation } from '../lib/session'
import { relativeTime } from '../lib/format'
import { ArchiveIcon, ChatIcon, ChevronIcon, EditIcon, LogoIcon, PlusIcon, RestoreIcon, SettingsIcon, TrashIcon } from './icons'

export const WorkspaceTree = memo(function WorkspaceTree(props: {
  projects: readonly ProjectSummary[]
  sessionIndexes: Readonly<Record<string, SessionIndexReadModel>>
  selectedProjectID: string
  selectedSessionID: string
  runningSessionIDs: ReadonlySet<string>
  version: string
  onSelectProject: (id: string) => void
  onSelectSession: (projectID: string, sessionID: string) => void
  onCreateSession: (projectID: string) => void
  onManageProviders: () => void
  onRenameProject: (project: ProjectSummary) => void
  onDeleteProject: (project: ProjectSummary) => void
  onRenameSession: (session: SessionNavigation) => void
  onArchiveSession: (session: SessionNavigation) => void
  onRestoreSession: (session: SessionNavigation) => void
  onDeleteSession: (session: SessionNavigation) => void
  onRetrySessionIndex: (projectID: string) => void
  onAdd: () => void
}) {
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(new Set())
  const [expandedArchivedProjects, setExpandedArchivedProjects] = useState<Set<string>>(new Set())
  const toggleProject = (projectID: string) => {
    setExpandedProjects((current) => {
      const next = new Set(current)
      if (next.has(projectID)) next.delete(projectID)
      else next.add(projectID)
      return next
    })
  }
  const toggleArchivedProject = (projectID: string) => {
    setExpandedArchivedProjects((current) => {
      const next = new Set(current)
      if (next.has(projectID)) next.delete(projectID)
      else next.add(projectID)
      return next
    })
  }

  const renderSessionRow = (projectID: string, session: SessionNavigation, sessions: readonly SessionNavigation[], archived = false) => {
    const running = session.status === 'running' || props.runningSessionIDs.has(session.id)
    const hasRunningDescendant = !running && ancestorSessionIDs(sessions, props.runningSessionIDs).has(session.id)
    return (
      <div className="session-tree-branch" key={session.id}>
        <div className={`session-tree-row ${archived ? 'archived' : ''} ${session.id === props.selectedSessionID ? 'selected' : ''} ${hasRunningDescendant ? 'running-descendant' : ''}`}>
          <span className="session-branch-toggle-spacer" aria-hidden="true" />
          <button
            className="session-tree-button"
            disabled={archived}
            onClick={archived ? undefined : () => props.onSelectSession(projectID, session.id)}
          >
            <span className="session-icon">{archived ? <ArchiveIcon /> : <ChatIcon />}</span>
            <span className="session-copy">
              <strong>{sessionName(session)}</strong>
              <small>{archived ? 'Archived' : relativeTime(session.updated_at)} · {session.status ?? 'idle'}</small>
            </span>
            {session.has_unread_result && <span className="unread-badge" title="Unread result" aria-label="Unread result">●</span>}
            {!archived && running && <span className="status-dot running" title="Session running" aria-label="Session running" />}
            {!archived && hasRunningDescendant && <span className="status-dot running-descendant" title="A sub-session is running" aria-label="A sub-session is running" />}
            {!archived && !running && !hasRunningDescendant && (session.status === 'interrupted' || session.status === 'failed') && <span className="status-dot interrupted" title="Session interrupted" aria-label="Session interrupted" />}
          </button>
          <div className="session-tree-actions">
            {archived ? (
              <>
                <button onClick={() => props.onRestoreSession(session)} aria-label={`Restore ${sessionName(session)}`} title="Restore"><RestoreIcon /></button>
                <button className="danger" onClick={() => props.onDeleteSession(session)} aria-label={`Delete ${sessionName(session)}`} title="Delete permanently"><TrashIcon /></button>
              </>
            ) : (
              <>
                <button disabled={running} onClick={() => props.onRenameSession(session)} aria-label={`Rename ${sessionName(session)}`} title="Rename"><EditIcon /></button>
                <button disabled={running} onClick={() => props.onArchiveSession(session)} aria-label={`Archive ${sessionName(session)}`} title="Archive"><ArchiveIcon /></button>
                <button className="danger" disabled={running} onClick={() => props.onDeleteSession(session)} aria-label={`Delete ${sessionName(session)}`} title="Delete"><TrashIcon /></button>
              </>
            )}
          </div>
        </div>
      </div>
    )
  }

  return (
    <aside className="project-rail">
      <div className="brand"><LogoIcon /><span>SAI</span><button className="brand-settings" onClick={props.onManageProviders} aria-label="Manage Server Root settings" title="Server Root settings"> <SettingsIcon /></button></div>
      <div className="rail-label">Projects & sessions</div>
      <nav className="project-tree" aria-label="Project and session tree">
        {props.projects.map((project) => {
          const index = props.sessionIndexes[project.id]
          const sessions = index?.active.map(navigationSession) ?? []
          const archivedSessions = index?.archived.map(navigationSession) ?? []
          const sessionRoots = buildSessionTree(sessions)
          const archivedSessionRoots = buildSessionTree(archivedSessions)
          const expanded = expandedProjects.has(project.id)
          const archivedExpanded = expandedArchivedProjects.has(project.id)
          const attentionSessionIDs = new Set(props.runningSessionIDs)
          if (props.selectedProjectID === project.id && props.selectedSessionID) attentionSessionIDs.add(props.selectedSessionID)
          const collapsedRoots = new Set(sessionRoots.slice(0, 3).map((node) => node.session.id))
          const visibleRoots = expanded
            ? sessionRoots
            : sessionRoots.filter((node) => collapsedRoots.has(node.session.id) || sessionTreeContains(node, attentionSessionIDs))
          const hiddenSessionCount = sessions.length - flattenSessionTree(visibleRoots).length
          return (
            <section className="project-tree-group" key={project.id}>
              <div className={`project-tree-header ${project.id === props.selectedProjectID ? 'selected' : ''}`}>
                <button className="project-node" onClick={() => props.onSelectProject(project.id)} title={project.root}>
                  <span className="project-avatar">{projectName(project).slice(0, 1).toUpperCase()}</span>
                  <span className="project-button-copy"><strong>{projectName(project)}</strong><small>{project.root}</small></span>
                  <span className="project-session-count">{sessions.length}</span>
                </button>
                <button className="tree-icon-button" onClick={() => props.onRenameProject(project)} aria-label={`Rename ${projectName(project)}`} title="Rename project"><EditIcon /></button>
                <button className="tree-icon-button" onClick={() => props.onCreateSession(project.id)} aria-label={`New session in ${projectName(project)}`} title="New session"><PlusIcon /></button>
                <button
                  className="tree-icon-button danger"
                  disabled={sessions.some((session) => session.status === 'running' || props.runningSessionIDs.has(session.id))}
                  onClick={() => props.onDeleteProject(project)}
                  aria-label={`Delete ${projectName(project)}`}
                  title="Delete project and sessions"
                ><TrashIcon /></button>
              </div>
              <div className="session-tree" role="group" aria-label={`Sessions of ${projectName(project)}`}>
                {index?.status === 'error' && <div className="tree-error"><span>Sessions unavailable.</span><button onClick={() => props.onRetrySessionIndex(project.id)}>Retry</button></div>}
                {index?.status === 'stale' && <div className="tree-stale"><span>Showing the last known sessions.</span><button onClick={() => props.onRetrySessionIndex(project.id)}>Retry</button></div>}
                {index?.status === 'loading' && <p className="tree-empty">Loading sessions…</p>}
                {visibleRoots.map((node) => renderSessionRow(project.id, node.session, sessions))}
                {sessions.length === 0 && index?.status !== 'loading' && <p className="tree-empty">No sessions yet</p>}
                {(expanded ? sessionRoots.length > 3 : hiddenSessionCount > 0) && (
                  <button className="tree-expand-button" onClick={() => toggleProject(project.id)}><ChevronIcon expanded={expanded} />{expanded ? 'Collapse' : `Show ${hiddenSessionCount} more sessions`}</button>
                )}
                {archivedSessions.length > 0 && (
                  <>
                    <button className="tree-expand-button archived-session-toggle" onClick={() => toggleArchivedProject(project.id)} aria-expanded={archivedExpanded}><ArchiveIcon /> Archived ({archivedSessions.length}) <ChevronIcon expanded={archivedExpanded} /></button>
                    {archivedExpanded && archivedSessionRoots.map((node) => renderSessionRow(project.id, node.session, archivedSessions, true))}
                  </>
                )}
              </div>
            </section>
          )
        })}
      </nav>
      <div className="project-rail-footer"><button className="secondary-button full" onClick={props.onAdd}><PlusIcon /> Add project</button><span className="version">v{props.version || 'dev'} · local</span></div>
    </aside>
  )
})
