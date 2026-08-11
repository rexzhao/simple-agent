import { memo, useEffect, useState } from 'react'
import type { SessionSubPanelContext, SessionNavigation } from '../lib/session'
import { sessionName } from '../lib/session'
import { ArchiveIcon, ChevronIcon, EditIcon, RestoreIcon, TrashIcon } from './icons'

export const SessionSubPanel = memo(function SessionSubPanel(props: {
  context: SessionSubPanelContext<SessionNavigation>
  viewingSessionID: string
  runningSessionIDs: ReadonlySet<string>
  onSelectSession: (sessionID: string) => void
  onRenameSession: (session: SessionNavigation) => void
  onArchiveSession: (session: SessionNavigation) => void
  onRestoreSession: (session: SessionNavigation) => void
  onDeleteSession: (session: SessionNavigation) => void
}) {
  const [minimized, setMinimized] = useState(false)
  const [archivedExpanded, setArchivedExpanded] = useState(false)
  const { parent, children, archivedChildren = [] } = props.context

  useEffect(() => {
    setArchivedExpanded(false)
  }, [parent.id])

  const renderTab = (session: SessionNavigation, isParent: boolean, archived = false) => {
    const running = session.status === 'running' || props.runningSessionIDs.has(session.id)
    const interrupted = !running && (session.status === 'interrupted' || session.status === 'failed')
    return (
      <div
        key={session.id}
        className={`sub-panel-tab ${session.id === props.viewingSessionID ? 'selected' : ''} ${isParent ? 'parent' : ''} ${archived ? 'archived' : ''}`}
      >
        <button
          className="sub-panel-tab-main"
          onClick={() => props.onSelectSession(session.id)}
          title={sessionName(session)}
        >
          <span className="sub-panel-tab-label">{sessionName(session)}</span>
          {running && <span className="status-dot running" title="Session running" />}
          {interrupted && <span className="status-dot interrupted" title="Session interrupted" />}
        </button>
        {!isParent && (
          <div className="sub-panel-tab-actions">
            {archived ? (
              <button
                disabled={parent.archived}
                onClick={() => { if (!parent.archived) props.onRestoreSession(session) }}
                aria-label={`Restore ${sessionName(session)}`}
                title={parent.archived ? 'Restore the root session first' : 'Restore'}
              ><RestoreIcon /></button>
            ) : (
              <>
                <button disabled={running} onClick={() => props.onRenameSession(session)} aria-label={`Rename ${sessionName(session)}`} title="Rename"><EditIcon /></button>
                <button disabled={running} onClick={() => props.onArchiveSession(session)} aria-label={`Archive ${sessionName(session)}`} title="Archive"><ArchiveIcon /></button>
              </>
            )}
            <button className="danger" disabled={running} onClick={() => props.onDeleteSession(session)} aria-label={`Delete ${sessionName(session)}`} title="Delete"><TrashIcon /></button>
          </div>
        )}
      </div>
    )
  }

  if (minimized) {
    return (
      <button
        className="session-sub-panel minimized"
        onClick={() => setMinimized(false)}
        aria-label="Expand sub-sessions panel"
        title="Show sub-sessions"
      >
        <ChevronIcon expanded={false} />
        <span className="sub-panel-minimized-count">{children.length + archivedChildren.length}</span>
      </button>
    )
  }

  return (
    <div className="session-sub-panel">
      <div className="sub-panel-header">
        <span className="sub-panel-title">Sub-sessions</span>
        <button
          className="sub-panel-minimize"
          onClick={() => setMinimized(true)}
          aria-label="Minimize sub-sessions panel"
          title="Minimize"
        >
          <ChevronIcon expanded={true} />
        </button>
      </div>
      <nav className="sub-panel-tabs" aria-label="Session and sub-sessions">
        {renderTab(parent, true)}
        <div className="sub-panel-divider" />
        {children.map((child) => renderTab(child, false))}
        {archivedChildren.length > 0 && (
          <>
            <div className="sub-panel-divider" />
            <button
              className="sub-panel-archived-toggle"
              onClick={() => setArchivedExpanded((expanded) => !expanded)}
              aria-expanded={archivedExpanded}
            >
              <ArchiveIcon />
              <span>Archived ({archivedChildren.length})</span>
              <ChevronIcon expanded={archivedExpanded} />
            </button>
            {archivedExpanded && archivedChildren.map((child) => renderTab(child, false, true))}
          </>
        )}
      </nav>
    </div>
  )
})
