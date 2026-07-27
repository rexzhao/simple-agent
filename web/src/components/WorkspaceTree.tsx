import { memo, useState } from 'react'
import type { Project, Session } from '../types'
import { projectName, sessionName } from '../lib/session'
import { relativeTime } from '../lib/format'
import { ArchiveIcon, ChatIcon, ChevronIcon, LogoIcon, PlusIcon, RestoreIcon, SettingsIcon, TrashIcon } from './icons'

export const WorkspaceTree = memo(function WorkspaceTree(props: {
  projects: Project[]
  sessionsByProject: Record<string, Session[]>
  archivedSessionsByProject: Record<string, Session[]>
  selectedProjectID: string
  selectedSessionID: string
  runningSessionIDs: ReadonlySet<string>
  version: string
  onSelectProject: (id: string) => void
  onSelectSession: (projectID: string, sessionID: string) => void
  onCreateSession: (projectID: string) => void
  onManageProviders: () => void
  onArchiveSession: (session: Session) => void
  onRestoreSession: (session: Session) => void
  onDeleteSession: (session: Session) => void
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

  return (
    <aside className="project-rail">
      <div className="brand"><LogoIcon /><span>SAI</span><button className="brand-settings" onClick={props.onManageProviders} aria-label="Manage Server Root settings" title="Server Root settings"><SettingsIcon /></button></div>
      <div className="rail-label">Projects & sessions</div>
      <nav className="project-tree" aria-label="Project and session tree">
        {props.projects.map((project) => {
          const sessions = props.sessionsByProject[project.id] ?? []
          const archivedSessions = props.archivedSessionsByProject[project.id] ?? []
          const expanded = expandedProjects.has(project.id)
		  const archivedExpanded = expandedArchivedProjects.has(project.id)
		  const collapsedSessions = sessions.slice(0, 3)
		  const visibleSessions = expanded
			? sessions
			: sessions.filter((session) => collapsedSessions.some((item) => item.id === session.id) || props.runningSessionIDs.has(session.id))
          return (
            <section className="project-tree-group" key={project.id}>
              <div className={`project-tree-header ${project.id === props.selectedProjectID ? 'selected' : ''}`}>
                <button className="project-node" onClick={() => props.onSelectProject(project.id)} title={project.root}>
                  <span className="project-avatar">{projectName(project).slice(0, 1).toUpperCase()}</span>
                  <span className="project-button-copy">
                    <strong>{projectName(project)}</strong>
                    <small>{project.root}</small>
                  </span>
                  <span className="project-session-count">{sessions.length}</span>
                </button>
                <button
                  className="tree-icon-button"
                  onClick={() => props.onCreateSession(project.id)}
                  aria-label={`New session in ${projectName(project)}`}
                  title="New session"
                ><PlusIcon /></button>
              </div>
              <div className="session-tree" role="group" aria-label={`Sessions of ${projectName(project)}`}>
                {visibleSessions.map((session) => (
                  <div className={`session-tree-row ${session.id === props.selectedSessionID ? 'selected' : ''}`} key={session.id}>
                    <button
                      className="session-tree-button"
                      onClick={() => props.onSelectSession(project.id, session.id)}
                    >
                      <span className="session-icon"><ChatIcon /></span>
                      <span className="session-copy">
                        <strong>{sessionName(session)}</strong>
                        <small>{relativeTime(session.last_used_at || session.updated_at)} · {session.model_id || session.model_profile}</small>
                      </span>
					  {(session.status === 'running' || props.runningSessionIDs.has(session.id)) && <span className="live-dot" />}
                    </button>
                    <div className="session-tree-actions">
                      <button disabled={session.status === 'running' || props.runningSessionIDs.has(session.id)} onClick={() => props.onArchiveSession(session)} aria-label={`Archive ${sessionName(session)}`} title="Archive"><ArchiveIcon /></button>
                      <button className="danger" disabled={session.status === 'running' || props.runningSessionIDs.has(session.id)} onClick={() => props.onDeleteSession(session)} aria-label={`Delete ${sessionName(session)}`} title="Delete"><TrashIcon /></button>
                    </div>
                  </div>
                ))}
                {sessions.length === 0 && <p className="tree-empty">No sessions yet</p>}
                {(expanded ? sessions.length > 3 : sessions.length > visibleSessions.length) && (
                  <button className="tree-expand-button" onClick={() => toggleProject(project.id)}>
                    <ChevronIcon expanded={expanded} />
					{expanded ? 'Collapse' : `Show ${sessions.length - visibleSessions.length} more sessions`}
                  </button>
                )}
                {archivedSessions.length > 0 && (
                  <>
                    <button className="tree-expand-button archived-session-toggle" onClick={() => toggleArchivedProject(project.id)} aria-expanded={archivedExpanded}>
                      <ArchiveIcon /> Archived ({archivedSessions.length}) <ChevronIcon expanded={archivedExpanded} />
                    </button>
                    {archivedExpanded && archivedSessions.map((session) => (
                      <div className="session-tree-row archived" key={session.id}>
                        <button className="session-tree-button" disabled title="Restore this session to open it">
                          <span className="session-icon"><ArchiveIcon /></span>
                          <span className="session-copy">
                            <strong>{sessionName(session)}</strong>
                            <small>Archived · {session.model_id || session.model_profile}</small>
                          </span>
                        </button>
                        <div className="session-tree-actions">
                          <button onClick={() => props.onRestoreSession(session)} aria-label={`Restore ${sessionName(session)}`} title="Restore"><RestoreIcon /></button>
                          <button className="danger" onClick={() => props.onDeleteSession(session)} aria-label={`Delete ${sessionName(session)}`} title="Delete permanently"><TrashIcon /></button>
                        </div>
                      </div>
                    ))}
                  </>
                )}
              </div>
            </section>
          )
        })}
      </nav>
      <div className="project-rail-footer">
		<button className="secondary-button full" onClick={props.onAdd}><PlusIcon /> Add project</button>
        <span className="version">v{props.version || 'dev'} · local</span>
      </div>
    </aside>
  )
})
