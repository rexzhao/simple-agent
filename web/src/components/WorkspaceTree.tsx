import { useState } from 'react'
import type { Project, Session } from '../types'
import { projectName, sessionName } from '../lib/session'
import { relativeTime } from '../lib/format'
import { ArchiveIcon, ChatIcon, ChevronIcon, LogoIcon, PlusIcon, SettingsIcon, TrashIcon } from './icons'

export function WorkspaceTree(props: {
  projects: Project[]
  sessionsByProject: Record<string, Session[]>
  selectedProjectID: string
  selectedSessionID: string
  runningSessionIDs: ReadonlySet<string>
  version: string
  onSelectProject: (id: string) => void
  onSelectSession: (projectID: string, sessionID: string) => void
  onCreateSession: (projectID: string) => void
  onManageProviders: () => void
  onArchiveSession: (session: Session) => void
  onDeleteSession: (session: Session) => void
  onAdd: () => void
}) {
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(new Set())
  const toggleProject = (projectID: string) => {
    setExpandedProjects((current) => {
      const next = new Set(current)
      if (next.has(projectID)) next.delete(projectID)
      else next.add(projectID)
      return next
    })
  }

  return (
    <aside className="project-rail">
      <div className="brand"><LogoIcon /><span>SAI</span><button className="brand-settings" onClick={props.onManageProviders} aria-label="管理 Server Root 配置" title="Server Root 配置"><SettingsIcon /></button></div>
      <div className="rail-label">项目与会话</div>
      <nav className="project-tree" aria-label="项目和会话树">
        {props.projects.map((project) => {
          const sessions = props.sessionsByProject[project.id] ?? []
          const expanded = expandedProjects.has(project.id)
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
                  aria-label={`在 ${projectName(project)} 中新建会话`}
                  title="新建会话"
                ><PlusIcon /></button>
              </div>
              <div className="session-tree" role="group" aria-label={`${projectName(project)} 的会话`}>
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
                      <button disabled={session.status === 'running' || props.runningSessionIDs.has(session.id)} onClick={() => props.onArchiveSession(session)} aria-label={`归档 ${sessionName(session)}`} title="归档"><ArchiveIcon /></button>
                      <button className="danger" disabled={session.status === 'running' || props.runningSessionIDs.has(session.id)} onClick={() => props.onDeleteSession(session)} aria-label={`删除 ${sessionName(session)}`} title="删除"><TrashIcon /></button>
                    </div>
                  </div>
                ))}
                {sessions.length === 0 && <p className="tree-empty">暂无会话</p>}
                {(expanded ? sessions.length > 3 : sessions.length > visibleSessions.length) && (
                  <button className="tree-expand-button" onClick={() => toggleProject(project.id)}>
                    <ChevronIcon expanded={expanded} />
					{expanded ? '收起' : `展开另外 ${sessions.length - visibleSessions.length} 个会话`}
                  </button>
                )}
              </div>
            </section>
          )
        })}
      </nav>
      <div className="project-rail-footer">
		<button className="secondary-button full" onClick={props.onAdd}><PlusIcon /> 添加项目</button>
        <span className="version">v{props.version || 'dev'} · local</span>
      </div>
    </aside>
  )
}
