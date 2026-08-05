import { useEffect, useState } from 'react'
import type { Session, SessionDebugSettings } from '../types'
import type { FrontendProtocolLoggingSnapshot } from '../lib/frontendProtocolLogger'
import { BugIcon } from './icons'

const emptyFrontendLogging: FrontendProtocolLoggingSnapshot = {
  enabled: false,
  records: [],
  droppedCount: 0,
}

export function DebugSettingsDialog(props: {
  session: Session
  saving: boolean
  onSave: (settings: SessionDebugSettings) => Promise<void>
  onClose: () => void
  frontendLogging?: FrontendProtocolLoggingSnapshot
  onFrontendProtocolLoggingToggle?: (enabled: boolean) => void
  onCopyFrontendProtocolLogs?: () => Promise<void>
  onDownloadFrontendProtocolLogs?: () => void
  onClearFrontendProtocolLogs?: () => void
}) {
  const [requestBodies, setRequestBodies] = useState(props.session.debug?.request_bodies ?? false)
  const frontendLogging = props.frontendLogging ?? emptyFrontendLogging
  const [frontendCopyError, setFrontendCopyError] = useState('')

  useEffect(() => {
    setRequestBodies(props.session.debug?.request_bodies ?? false)
  }, [props.session.id, props.session.debug?.request_bodies])

  useEffect(() => {
    setFrontendCopyError('')
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

  const copyFrontendLogs = async () => {
    setFrontendCopyError('')
    try {
      await props.onCopyFrontendProtocolLogs?.()
    } catch (reason) {
      setFrontendCopyError(reason instanceof Error ? reason.message : 'Clipboard access failed. Allow clipboard access to copy the frontend protocol log.')
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
          <section className="frontend-debug-section" aria-labelledby="frontend-protocol-title">
            <div className="frontend-debug-heading">
              <div>
                <strong id="frontend-protocol-title">Frontend protocol logging</strong>
                <small>Temporary browser-only diagnostics for this session. Nothing is uploaded or persisted on the server.</small>
              </div>
              <button
                className={`debug-switch${frontendLogging.enabled ? ' enabled' : ''}`}
                type="button"
                role="switch"
                aria-checked={frontendLogging.enabled}
                aria-label="Enable frontend protocol logging"
                onClick={() => props.onFrontendProtocolLoggingToggle?.(!frontendLogging.enabled)}
              >
                {frontendLogging.enabled ? 'On' : 'Off'}
              </button>
            </div>
            <div className="frontend-debug-status" role="status">
              {frontendLogging.records.length} records · {frontendLogging.droppedCount} dropped
            </div>
            <div className="frontend-debug-actions">
              <button className="secondary-button" type="button" onClick={() => void copyFrontendLogs()} disabled={!props.onCopyFrontendProtocolLogs}>Copy JSONL</button>
              <button className="secondary-button" type="button" onClick={props.onDownloadFrontendProtocolLogs} disabled={!props.onDownloadFrontendProtocolLogs}>Download JSONL</button>
              <button className="secondary-button" type="button" onClick={props.onClearFrontendProtocolLogs} disabled={!props.onClearFrontendProtocolLogs || frontendLogging.records.length === 0}>Clear</button>
            </div>
            {frontendCopyError && <p className="frontend-debug-error" role="alert">{frontendCopyError}</p>}
          </section>
        </div>
        <div className="model-dialog-actions">
          <button className="secondary-button" disabled={props.saving} onClick={props.onClose}>Cancel</button>
          <button className="primary-button" disabled={props.saving} onClick={() => void save()}>{props.saving ? 'Saving…' : 'Save settings'}</button>
        </div>
      </section>
    </div>
  )
}
