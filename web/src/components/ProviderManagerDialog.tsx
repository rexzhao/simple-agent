import { useCallback, useEffect, useState } from 'react'
import { api } from '../api'
import type { CodexAuthStatus, ProviderModelSettings, ProviderSettings, ProviderSettingsDocument, ProviderSettingsInput } from '../types'
import { copyText, errorMessage, parseJSONRecord, prettyJSON } from '../lib/format'
import { reasoningLevelLabel } from '../lib/session'
import { PlusIcon } from './icons'

export interface ProviderManagerState {
  document: ProviderSettingsDocument | null
  loading: boolean
}

interface EditableProviderModel {
  profile: string
  id: string
  type: string
  supportsImages: boolean
  developerRole: string
  contextWindow: string
  inputLimit: string
  outputLimit: string
  parametersJSON: string
  reasoningParameter: string
  reasoningDefault: string
  reasoningLevelsJSON: string
}

interface ProviderDraft {
  existingName: string
  name: string
  baseURL: string
  apiKey: string
  keepAPIKey: boolean
  apiKeyConfigured: boolean
  authFile: string
  requestTimeout: string
  models: EditableProviderModel[]
}

export function ProviderManagerDialog(props: {
  state: ProviderManagerState
  onDocument: (document: ProviderSettingsDocument) => void
  onClose: () => void
  onError: (message: string) => void
}) {
  const [draft, setDraft] = useState<ProviderDraft | null>(null)
  const [saving, setSaving] = useState(false)
  const [discovering, setDiscovering] = useState(false)
  const [discoveredModels, setDiscoveredModels] = useState<string[]>([])
  const [codexAuth, setCodexAuth] = useState<CodexAuthStatus | null>(null)
  const document = props.state.document

  const selectProvider = useCallback((provider: ProviderSettings) => {
    setDraft(providerDraft(provider))
    setCodexAuth(provider.codex_auth ?? null)
    setDiscoveredModels([])
  }, [])

  useEffect(() => {
    if (!document || draft) return
    const provider = document.providers.find((item) => item.name === document.default_provider) ?? document.providers[0]
    if (provider) selectProvider(provider)
    else setDraft(emptyProviderDraft())
  }, [document, draft, selectProvider])

  useEffect(() => {
    if (codexAuth?.status !== 'pending' || !draft?.existingName) return
    const timer = window.setInterval(() => {
      void api.codexLoginStatus(draft.existingName)
        .then(setCodexAuth)
        .catch((reason: unknown) => props.onError(errorMessage(reason)))
    }, 1500)
    return () => window.clearInterval(timer)
  }, [codexAuth?.status, draft?.existingName, props.onError])

  const save = async () => {
    if (!draft || saving) return
    setSaving(true)
    try {
      const input = providerInput(draft)
      const updated = draft.existingName
        ? await api.updateProvider(draft.existingName, input)
        : await api.createProvider(input)
      props.onDocument(updated)
      const saved = updated.providers.find((provider) => provider.name === input.name)
      if (saved) selectProvider(saved)
    } catch (reason) {
      props.onError(errorMessage(reason))
    } finally {
      setSaving(false)
    }
  }

  const discoverModels = async () => {
    if (!draft?.existingName || discovering) return
    setDiscovering(true)
    try {
      const result = await api.discoverProviderModels(draft.existingName)
      setDiscoveredModels(result.models)
    } catch (reason) {
      props.onError(errorMessage(reason))
    } finally {
      setDiscovering(false)
    }
  }

  const setDefault = async (profile: string) => {
    if (!draft?.existingName) return
    try {
      props.onDocument(await api.updateProviderDefault(draft.existingName, profile))
    } catch (reason) {
      props.onError(errorMessage(reason))
    }
  }

  const startCodexLogin = async () => {
    if (!draft?.existingName) return
    try {
      setCodexAuth(await api.startCodexLogin(draft.existingName))
    } catch (reason) {
      props.onError(errorMessage(reason))
    }
  }

  const clearCodexLogin = async () => {
    if (!draft?.existingName || !window.confirm('退出当前 Server Root 的 Codex 登录？')) return
    try {
      await api.clearCodexLogin(draft.existingName)
      setCodexAuth({ status: 'signed_out' })
    } catch (reason) {
      props.onError(errorMessage(reason))
    }
  }

  const updateModel = (index: number, patch: Partial<EditableProviderModel>) => {
    setDraft((current) => current ? { ...current, models: current.models.map((model, modelIndex) => modelIndex === index ? { ...model, ...patch } : model) } : current)
  }

  const usesCodex = draft?.models.some((model) => model.type === 'openai-codex') ?? false
  const savedCodexProvider = document?.providers.find((provider) => provider.name === draft?.existingName)?.models.some((model) => model.type === 'openai-codex') ?? false

  return (
    <div className="model-dialog-backdrop provider-dialog-backdrop">
      <section className="provider-dialog" role="dialog" aria-modal="true" aria-labelledby="provider-dialog-title">
        <header className="provider-dialog-header">
          <div>
            <span className="eyebrow">Server Root 配置</span>
            <h2 id="provider-dialog-title">Provider 与模型</h2>
            <p>{document ? `${document.server_root} · ${document.config_path}` : '读取当前 Server Root'}</p>
          </div>
          <button className="model-dialog-close" disabled={saving} onClick={props.onClose} aria-label="关闭">×</button>
        </header>
        {props.state.loading || !document || !draft ? (
          <div className="provider-loading">读取 Server Root 配置…</div>
        ) : (
          <div className="provider-dialog-body">
            <aside className="provider-list">
              {document.providers.map((provider) => (
                <button className={draft.existingName === provider.name ? 'selected' : ''} onClick={() => selectProvider(provider)} key={provider.name}>
                  <strong>{provider.name}</strong>
                  <small>{provider.models.length} 个模型</small>
                </button>
              ))}
              <button className={!draft.existingName ? 'selected add-provider' : 'add-provider'} onClick={() => { setDraft(emptyProviderDraft()); setCodexAuth(null); setDiscoveredModels([]) }}><PlusIcon /> 新增 Provider</button>
            </aside>
            <div className="provider-editor">
              <section className="settings-section">
                <div className="settings-section-title"><h3>连接配置</h3>{draft.existingName && <code>{draft.existingName}.yaml</code>}</div>
                <div className="settings-grid">
                  <label>名称<input value={draft.name} disabled={Boolean(draft.existingName)} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="openai" /></label>
                  <label>请求超时<input value={draft.requestTimeout} onChange={(event) => setDraft({ ...draft, requestTimeout: event.target.value })} placeholder="60s" /></label>
                  <label className="wide">Base URL<input value={draft.baseURL} onChange={(event) => setDraft({ ...draft, baseURL: event.target.value })} placeholder="https://api.openai.com/v1" /></label>
                  <label className="wide">API Key / 环境变量<input value={draft.apiKey} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value, keepAPIKey: false })} placeholder={draft.apiKeyConfigured ? '已配置；留空并保留下方选项不会修改' : '$OPENAI_API_KEY'} /></label>
                  {draft.apiKeyConfigured && <label className="checkbox-field wide"><input type="checkbox" checked={draft.keepAPIKey} onChange={(event) => setDraft({ ...draft, keepAPIKey: event.target.checked })} /> 保留当前 API Key</label>}
                </div>
              </section>

              {usesCodex && (
                <section className="settings-section codex-auth-card">
                  <div className="settings-section-title"><h3>Codex 登录态</h3><span className={`auth-status ${codexAuth?.status ?? 'signed_out'}`}>{codexAuthLabel(codexAuth?.status)}</span></div>
                  {codexAuth?.account_id && <p>账户：<code>{codexAuth.account_id}</code></p>}
                  {codexAuth?.expires_at && <p>过期时间：{new Date(codexAuth.expires_at).toLocaleString('zh-CN')}</p>}
                  {codexAuth?.status === 'pending' && <div className="device-login"><strong>{codexAuth.user_code}</strong><button className="secondary-button" onClick={() => void copyText(codexAuth.user_code ?? '')}>复制验证码</button>{codexAuth.verification_url && <a href={codexAuth.verification_url} target="_blank" rel="noreferrer">打开登录页面</a>}</div>}
                  {codexAuth?.message && <p className="settings-error">{codexAuth.message}</p>}
                  <div className="inline-actions">
                    {codexAuth?.status !== 'pending' && codexAuth?.status !== 'signed_in' && <button className="primary-button" disabled={!savedCodexProvider} onClick={() => void startCodexLogin()}>登录 Codex</button>}
                    {(codexAuth?.status === 'signed_in' || codexAuth?.status === 'expired') && <button className="secondary-button" onClick={() => void clearCodexLogin()}>退出登录</button>}
                    {!savedCodexProvider && <small>请先保存 Codex Provider，再开始登录。</small>}
                  </div>
                </section>
              )}

              <section className="settings-section">
                <div className="settings-section-title">
                  <div><h3>模型列表</h3><p>推理等级使用统一显示名，映射值可以是字符串、数字、布尔值或对象。</p></div>
                  <div className="inline-actions">
                    <button className="secondary-button compact" disabled={!draft.existingName || discovering} onClick={() => void discoverModels()}>{discovering ? '获取中…' : '从 Provider 获取'}</button>
                    <button className="secondary-button compact" onClick={() => setDraft({ ...draft, models: [...draft.models, emptyProviderModel()] })}><PlusIcon /> 添加模型</button>
                  </div>
                </div>
                <div className="provider-models">
                  {draft.models.map((model, index) => {
                    const isDefault = draft.existingName === document.default_provider && model.profile === document.default_model
                    const reasoningLevels = reasoningLevelOptions(model.reasoningLevelsJSON)
                    return <article className="provider-model-card" key={`${index}-${model.profile}`}>
                      <div className="provider-model-heading"><strong>{model.profile || `模型 ${index + 1}`}</strong><div className="inline-actions">{isDefault ? <span className="default-badge">默认</span> : <button className="plain-button" disabled={!draft.existingName || !model.profile} onClick={() => void setDefault(model.profile)}>设为默认</button>}<button className="plain-button danger" disabled={draft.models.length === 1} onClick={() => setDraft({ ...draft, models: draft.models.filter((_, modelIndex) => modelIndex !== index) })}>移除</button></div></div>
                      <div className="settings-grid model-grid">
                        {discoveredModels.length > 0 && <label className="wide model-catalog-select">从模型列表选择<select value={discoveredModels.includes(model.id) ? model.id : ''} onChange={(event) => {
                          const selectedID = event.target.value
                          if (selectedID) updateModel(index, { id: selectedID, profile: !model.profile || model.profile === model.id ? selectedID : model.profile })
                        }}><option value="">请选择模型（已获取 {discoveredModels.length} 个）</option>{discoveredModels.map((modelID) => <option value={modelID} key={modelID}>{modelID}</option>)}</select></label>}
                        <label>配置名<input value={model.profile} onChange={(event) => updateModel(index, { profile: event.target.value })} placeholder="gpt-5.5" /></label>
                        <label>模型 ID<input value={model.id} onChange={(event) => updateModel(index, { id: event.target.value })} placeholder="也可以手动输入" /></label>
                        <label>API 类型<select value={model.type || 'openai-chat'} onChange={(event) => updateModel(index, { type: event.target.value })}><option value="openai-chat">OpenAI Chat</option><option value="openai-responses">OpenAI Responses</option><option value="openai-codex">OpenAI Codex</option><option value="anthropic-messages">Anthropic Messages</option></select></label>
                        <label className="checkbox-field"><input type="checkbox" checked={model.supportsImages} onChange={(event) => updateModel(index, { supportsImages: event.target.checked })} /> 支持图片输入</label>
                        <label>Developer 角色<select value={model.developerRole} onChange={(event) => updateModel(index, { developerRole: event.target.value })}><option value="">保持 developer</option><option value="system">映射为 system</option></select></label>
                        <label>Context Window<input type="number" min="0" value={model.contextWindow} onChange={(event) => updateModel(index, { contextWindow: event.target.value })} placeholder="400000" /></label>
                        <label>Input Limit<input type="number" min="0" value={model.inputLimit} onChange={(event) => updateModel(index, { inputLimit: event.target.value })} placeholder="272000" /></label>
                        <label>Output Limit<input type="number" min="0" value={model.outputLimit} onChange={(event) => updateModel(index, { outputLimit: event.target.value })} placeholder="128000" /></label>
                        <label className="wide">其他请求参数（JSON）<textarea value={model.parametersJSON} onChange={(event) => updateModel(index, { parametersJSON: event.target.value })} rows={3} spellCheck={false} /></label>
                      </div>
                      <details className="reasoning-config" open={Boolean(model.reasoningParameter)}>
                        <summary>Reasoning config {model.reasoningParameter ? <code>{model.reasoningParameter}</code> : <small>留空会写入 Pi 推荐默认</small>}</summary>
                        <div className="settings-grid model-grid">
                          <label>参数路径<input value={model.reasoningParameter} onChange={(event) => updateModel(index, { reasoningParameter: event.target.value })} placeholder="reasoning.effort" /></label>
                          <label>默认等级<select value={reasoningLevels.includes(model.reasoningDefault) ? model.reasoningDefault : ''} disabled={reasoningLevels.length === 0} onChange={(event) => updateModel(index, { reasoningDefault: event.target.value })}><option value="">{reasoningLevels.length === 0 ? '请先填写等级映射' : '不设置默认等级'}</option>{reasoningLevels.map((level) => <option value={level} key={level}>{reasoningLevelLabel(level)} ({level})</option>)}</select></label>
                          <label className="wide">等级映射（JSON）<textarea value={model.reasoningLevelsJSON} onChange={(event) => updateModel(index, { reasoningLevelsJSON: event.target.value })} rows={4} spellCheck={false} placeholder={'{"low":"low","high":"high"}'} /></label>
                        </div>
                      </details>
                    </article>
                  })}
                </div>
              </section>
            </div>
          </div>
        )}
        <footer className="model-dialog-actions"><button className="secondary-button" disabled={saving} onClick={props.onClose}>取消</button><button className="primary-button" disabled={!draft || saving} onClick={() => void save()}>{saving ? '保存中…' : '保存配置'}</button></footer>
      </section>
    </div>
  )
}

function providerDraft(provider: ProviderSettings): ProviderDraft {
  return {
    existingName: provider.name,
    name: provider.name,
    baseURL: provider.base_url,
    apiKey: provider.api_key ?? '',
    keepAPIKey: provider.api_key_configured,
    apiKeyConfigured: provider.api_key_configured,
    authFile: provider.auth_file ?? '',
    requestTimeout: provider.request_timeout ?? '',
    models: provider.models.map(editableProviderModel),
  }
}

function emptyProviderDraft(): ProviderDraft {
  return { existingName: '', name: '', baseURL: '', apiKey: '', keepAPIKey: false, apiKeyConfigured: false, authFile: '', requestTimeout: '60s', models: [emptyProviderModel()] }
}

function editableProviderModel(model: ProviderModelSettings): EditableProviderModel {
  return {
    profile: model.profile,
    id: model.id,
    type: model.type || 'openai-chat',
    supportsImages: model.input?.includes('image') ?? false,
    developerRole: model.developer_role ?? '',
    contextWindow: model.context_window ? String(model.context_window) : '',
    inputLimit: model.input_limit ? String(model.input_limit) : '',
    outputLimit: model.output_limit ? String(model.output_limit) : '',
    parametersJSON: prettyJSON(model.parameters ?? {}),
    reasoningParameter: model.reasoning_config?.parameter ?? '',
    reasoningDefault: model.reasoning_config?.default ?? '',
    reasoningLevelsJSON: prettyJSON(model.reasoning_config?.levels ?? {}),
  }
}

function emptyProviderModel(): EditableProviderModel {
  return { profile: '', id: '', type: 'openai-chat', supportsImages: false, developerRole: '', contextWindow: '', inputLimit: '', outputLimit: '', parametersJSON: '{}', reasoningParameter: '', reasoningDefault: '', reasoningLevelsJSON: '{}' }
}

function providerInput(draft: ProviderDraft): ProviderSettingsInput {
  if (!draft.name.trim()) throw new Error('Provider 名称不能为空')
  if (!draft.baseURL.trim()) throw new Error('Base URL 不能为空')
  if (draft.models.length === 0) throw new Error('至少需要一个模型')
  return {
    name: draft.name.trim(),
    base_url: draft.baseURL.trim(),
    api_key: draft.apiKey.trim(),
    keep_api_key: draft.keepAPIKey,
    auth_file: draft.authFile.trim(),
    request_timeout: draft.requestTimeout.trim(),
    models: draft.models.map((model, index) => {
      if (!model.profile.trim() || !model.id.trim()) throw new Error(`模型 ${index + 1} 的配置名和模型 ID 不能为空`)
      const reasoningLevels = parseJSONRecord(model.reasoningLevelsJSON, `模型 ${model.profile} 的等级映射`)
      if (model.reasoningDefault.trim() && !(model.reasoningDefault.trim() in reasoningLevels)) throw new Error(`模型 ${model.profile} 的默认等级不在等级映射中`)
      return {
        profile: model.profile.trim(),
        id: model.id.trim(),
        type: model.type,
        input: model.supportsImages ? ['text', 'image'] : ['text'],
        developer_role: model.developerRole,
        context_window: model.contextWindow ? Number(model.contextWindow) : 0,
        input_limit: model.inputLimit ? Number(model.inputLimit) : 0,
        output_limit: model.outputLimit ? Number(model.outputLimit) : 0,
        parameters: parseJSONRecord(model.parametersJSON, `模型 ${model.profile} 的请求参数`),
        reasoning_config: {
          parameter: model.reasoningParameter.trim(),
          default: model.reasoningDefault.trim(),
          levels: reasoningLevels,
        },
      }
    }),
  }
}

function reasoningLevelOptions(value: string): string[] {
  try {
    const parsed: unknown = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return []
    const keys = Object.keys(parsed)
    const canonical = ['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']
    return [...canonical.filter((level) => keys.includes(level)), ...keys.filter((level) => !canonical.includes(level)).sort()]
  } catch {
    return []
  }
}

function codexAuthLabel(status?: string): string {
  return { signed_out: '未登录', pending: '等待授权', signed_in: '已登录', expired: '已过期', error: '认证异常' }[status ?? 'signed_out'] ?? status ?? '未登录'
}
