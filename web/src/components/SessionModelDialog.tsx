import type { Project, SessionModelOption } from '../types'
import { modelKey, projectName, reasoningLevelLabel } from '../lib/session'
import { ChatIcon } from './icons'

export interface SessionCreatorState {
  projectID: string
  models: SessionModelOption[]
  selectedKey: string
  defaultProvider: string
  defaultModel: string
  reasoningLevel: string
  fullAccess: boolean
  automaticCompaction: boolean
  loading: boolean
}

export function SessionModelDialog(props: {
  project?: Project
  state: SessionCreatorState
  creating: boolean
  onSelect: (key: string) => void
  onReasoningLevel: (level: string) => void
  onFullAccess: (fullAccess: boolean) => void
  onAutomaticCompaction: (enabled: boolean) => void
  onCancel: () => void
  onCreate: (model: SessionModelOption) => void
}) {
  const selected = props.state.models.find((model) => modelKey(model) === props.state.selectedKey)
  return (
    <div className="model-dialog-backdrop">
      <section className="model-dialog" role="dialog" aria-modal="true" aria-labelledby="model-dialog-title">
        <div className="model-dialog-header">
          <div className="model-dialog-icon"><ChatIcon /></div>
          <div>
            <span className="eyebrow">New session</span>
            <h2 id="model-dialog-title">Choose a model</h2>
            <p>{props.project ? `${projectName(props.project)} · ${props.project.root}` : 'Loading Server Root settings'}</p>
          </div>
          <button className="model-dialog-close" disabled={props.creating} onClick={props.onCancel} aria-label="Close">×</button>
        </div>
        <div className="model-choice-list">
          {props.state.loading ? (
            <div className="model-choice-loading"><i /><i /><i /></div>
          ) : props.state.models.length > 0 ? props.state.models.map((model) => {
            const selectedModel = modelKey(model) === props.state.selectedKey
            const isDefault = model.provider === props.state.defaultProvider && model.model_profile === props.state.defaultModel
            return (
              <button
                type="button"
                className={`model-choice ${selectedModel ? 'selected' : ''}`}
                disabled={props.creating}
                aria-pressed={selectedModel}
                onClick={() => props.onSelect(modelKey(model))}
                key={modelKey(model)}
              >
                <span className="model-choice-mark">{selectedModel ? '✓' : ''}</span>
                <span className="model-choice-copy">
                  <span><strong>{model.model_profile}</strong>{isDefault && <small>Default</small>}</span>
                  <code>{model.provider} / {model.model_id}</code>
                </span>
              </button>
            )
          }) : (
            <p className="model-choice-empty">No models available in the Server Root settings.</p>
          )}
        </div>
        {selected && (selected.reasoning_levels?.length ?? 0) > 0 && (
          <label className="reasoning-choice">
            <span>Reasoning effort</span>
            <select value={props.state.reasoningLevel || selected.default_reasoning_level || ''} disabled={props.creating} onChange={(event) => props.onReasoningLevel(event.target.value)}>
              {selected.reasoning_levels?.map((level) => <option value={level} key={level}>{reasoningLevelLabel(level)}</option>)}
            </select>
            <small>The UI uses unified levels; the actual request value is mapped by this model's reasoning config.</small>
          </label>
        )}
        <label className="full-access-choice">
          <input type="checkbox" checked={props.state.fullAccess} disabled={props.creating} onChange={(event) => props.onFullAccess(event.target.checked)} />
          <span>
            <strong>Full access</strong>
            <small>File tools may read and write outside the workspace. Out-of-workspace paths are flagged in the conversation. Applies from the first turn; child sessions inherit it.</small>
          </span>
        </label>
        <label className="session-option-choice">
          <input type="checkbox" checked={props.state.automaticCompaction} disabled={props.creating} onChange={(event) => props.onAutomaticCompaction(event.target.checked)} />
          <span>
            <strong>Automatic context compaction</strong>
            <small>Summarize older context automatically when the model approaches its limit. Manual compaction remains available when this is off.</small>
          </span>
        </label>
        <div className="model-dialog-actions">
          <button className="secondary-button" disabled={props.creating} onClick={props.onCancel}>Cancel</button>
          <button className="primary-button" disabled={!selected || props.creating} onClick={() => selected && props.onCreate(selected)}>
            {props.creating ? 'Creating…' : 'Create session'}
          </button>
        </div>
      </section>
    </div>
  )
}
