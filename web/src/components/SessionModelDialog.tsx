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
  loading: boolean
}

export function SessionModelDialog(props: {
  project?: Project
  state: SessionCreatorState
  creating: boolean
  onSelect: (key: string) => void
  onReasoningLevel: (level: string) => void
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
            <span className="eyebrow">新建会话</span>
            <h2 id="model-dialog-title">选择模型</h2>
            <p>{props.project ? `${projectName(props.project)} · ${props.project.root}` : '加载 Server Root 配置'}</p>
          </div>
          <button className="model-dialog-close" disabled={props.creating} onClick={props.onCancel} aria-label="关闭">×</button>
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
                  <span><strong>{model.model_profile}</strong>{isDefault && <small>默认</small>}</span>
                  <code>{model.provider} / {model.model_id}</code>
                </span>
              </button>
            )
          }) : (
            <p className="model-choice-empty">Server Root 配置中没有可用模型。</p>
          )}
        </div>
        {selected && (selected.reasoning_levels?.length ?? 0) > 0 && (
          <label className="reasoning-choice">
            <span>推理强度</span>
            <select value={props.state.reasoningLevel || selected.default_reasoning_level || ''} disabled={props.creating} onChange={(event) => props.onReasoningLevel(event.target.value)}>
              {selected.reasoning_levels?.map((level) => <option value={level} key={level}>{reasoningLevelLabel(level)}</option>)}
            </select>
            <small>界面使用统一等级，实际请求值由该模型的 reasoning config 映射。</small>
          </label>
        )}
        <div className="model-dialog-actions">
          <button className="secondary-button" disabled={props.creating} onClick={props.onCancel}>取消</button>
          <button className="primary-button" disabled={!selected || props.creating} onClick={() => selected && props.onCreate(selected)}>
            {props.creating ? '创建中…' : '创建会话'}
          </button>
        </div>
      </section>
    </div>
  )
}
