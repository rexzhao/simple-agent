import { useState } from 'react'
import { ChatIcon, FolderIcon, LogoIcon, PlusIcon } from './icons'

export function ProjectSetup(props: { suggestedRoot: string; hasProjects: boolean; onCancel: () => void; onSubmit: (root: string, name: string) => void }) {
  const [root, setRoot] = useState(props.suggestedRoot)
  const [name, setName] = useState('')
  return (
    <div className="setup-screen">
      <div className="setup-card">
        <div className="setup-icon"><FolderIcon /></div>
        <span className="eyebrow">Local workspace</span>
        <h1>{props.hasProjects ? 'Add another project' : 'Connect your first project'}</h1>
        <p>The project directory is used only as a workspace; providers, models, and run settings are managed centrally by the current Server Root.</p>
        <label>Project directory<input value={root} onChange={(event) => setRoot(event.target.value)} placeholder="F:\work\project" autoFocus /></label>
        <label>Display name <small>Optional</small><input value={name} onChange={(event) => setName(event.target.value)} placeholder="e.g. Simple Agent" /></label>
        <div className="setup-actions">
          {props.hasProjects && <button className="secondary-button" onClick={props.onCancel}>Cancel</button>}
          <button className="primary-button" disabled={!root.trim()} onClick={() => props.onSubmit(root.trim(), name.trim())}>Connect project</button>
        </div>
      </div>
    </div>
  )
}

export function EmptySession({ disabled, onCreate }: { disabled: boolean; onCreate: () => void }) {
  return <div className="setup-screen"><div className="empty-session"><ChatIcon /><h1>No sessions yet</h1><p>Create a session to start working with the agent in this project.</p><button className="primary-button" disabled={disabled} onClick={onCreate}><PlusIcon /> New session</button></div></div>
}

export function ErrorBanner({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return <div className="error-banner" role="alert"><span>{message}</span><button onClick={onDismiss} aria-label="Close">×</button></div>
}

export function Splash() {
  return <div className="splash"><div className="splash-logo"><LogoIcon /></div><span>SAI</span></div>
}

export function MessageSkeleton() {
  return <div className="skeleton"><i /><i /><i /></div>
}
