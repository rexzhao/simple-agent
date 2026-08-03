import { useEffect, useState } from 'react'
import type { Session, SessionDebugSettings } from '../types'
import { BugIcon } from './icons'

export function DebugSettingsDialog(props: {
  session: Session
  saving: boolean
  onSave: (settings: SessionDebugSettings) => Promise<void>
  onClose: () => void
}) {
  const [requestBodies, setRequestBodies] = useState(props.session.debug?.request_bodies ?? false)

  useEffect(() => {
    setRequestBodies(props.session.debug?.request_bodies ?? false)
  }, [props.session.id])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !props.saving) props.onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [props.onClose, props.saving])

  const save = async () => {
    try {
      await props.onSave({ request_bodies: requestBodies })
    } catch {
      // The parent reports the API error through the app error banner. Keep
      // the dialog open so the user can retry without losing the selection.
    }
  }

  return (
    <div className="model-dialog-backdrop" onMouseDown={(event) => { if (event.target === event.currentTarget && !props.saving) props.onClose() }}>
      <section className="model-dialog debug-dialog" role="dialog" aria-modal="true" aria-labelledby="debug-dialog-title">
        <div className="model-dialog-header">
          <div className="model-dialog-icon"><BugIcon /></div>
          <div>
            <span className="eyebrow">Session debugging</span>
            <h2 id="debug-dialog-title">Debug settings</h2>
            <p>{props.session.display_name || props.session.id}</p>
          </div>
          <button className="model-dialog-close" disabled={props.saving} onClick={props.onClose} aria-label="Close">×</button>
        </div>
        <div className="debug-dialog-body">
          <p className="debug-dialog-intro">These settings apply from the next turn. Captured requests may contain prompts, tool definitions, reasoning state, and tool outputs.</p>
          <label className="debug-option">
            <input type="checkbox" checked={requestBodies} disabled={props.saving} onChange={(event) => setRequestBodies(event.target.checked)} />
            <span>
              <strong>Capture provider request bodies</strong>
              <small>Save the raw request body sent by supported OpenAI Responses and Codex providers beside the diagnostic log.</small>
            </span>
          </label>
        </div>
        <div className="model-dialog-actions">
          <button className="secondary-button" disabled={props.saving} onClick={props.onClose}>Cancel</button>
          <button className="primary-button" disabled={props.saving} onClick={() => void save()}>{props.saving ? 'Saving…' : 'Save settings'}</button>
        </div>
      </section>
    </div>
  )
}
