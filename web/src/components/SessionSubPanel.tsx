import { memo, useState } from 'react'
import type { Session } from '../types'
import type { SessionSubPanelContext } from '../lib/session'
import { sessionName } from '../lib/session'
import { ArchiveIcon, ChevronIcon, EditIcon, TrashIcon } from './icons'

export const SessionSubPanel = memo(function SessionSubPanel(props: {
  context: SessionSubPanelContext
  viewingSessionID: string
  runningSessionIDs: ReadonlySet<string>
  onSelectSession: (sessionID: string) => void
  onRenameSession: (session: Session) => void
  onArchiveSession: (session: Session) => void
  onDeleteSession: (session: Session) => void
}) {
  const [minimized, setMinimized] = useState(false)
  const { parent, children } = props.context

  const renderTab = (session: Session, isParent: boolean) => {
    const running = session.status === 'running' || props.runningSessionIDs.has(session.id)
    const interrupted = !running && (session.status === 'interrupted' || session.status === 'failed')
    return (
      <div
        key={session.id}
        className={`sub-panel-tab ${session.id === props.viewingSessionID ? 'selected' : ''} ${isParent ? 'parent' : ''}`}
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
            <button disabled={running} onClick={() => props.onRenameSession(session)} aria-label={`Rename ${sessionName(session)}`} title="Rename"><EditIcon /></button>
            <button disabled={running} onClick={() => props.onArchiveSession(session)} aria-label={`Archive ${sessionName(session)}`} title="Archive"><ArchiveIcon /></button>
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
        <span className="sub-panel-minimized-count">{children.length}</span>
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
      </nav>
    </div>
  )
})
