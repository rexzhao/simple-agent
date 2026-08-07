import { useCallback, useEffect, useRef, useState } from 'react'
import type { ProviderUpdateTarget } from '../commands/providerCommands'
import type { JsonObject } from '../domain/json'
import type { CodexUsageDomain, CodexUsageCreditsDomain, CodexUsageWindowDomain, CodexUsageWindowSetDomain } from '../domain/codexUsage'
import type { CodexLoginReadModel } from '../repositories/codexLogin'
import type { ProviderModelDomain, ProviderSettingsDomain, ProviderSettingsReadModel } from '../repositories/providerSettings'
import { copyText, parseJSONRecord, prettyJSON } from '../lib/format'
import { reasoningLevelLabel } from '../lib/session'
import { PlusIcon } from './icons'

interface EditableProviderModel {
  profile: string
  id: string
  type: string
  compatibility: string
  supportsImages: boolean
  developerRole: string
  contextWindow: string
  inputLimit: string
  outputLimit: string
  parametersMode: 'preserve' | 'replace'
  parametersSourceProfile: string
  parametersJSON: string
  reasoningParameter: string
  reasoningDefault: string
  reasoningLevelsJSON: string
  pricingCurrency: string
  inputCacheHitPrice: string
  inputCacheMissPrice: string
  cacheWritePrice: string
  outputPrice: string
  longContextThreshold: string
  longInputCacheHitPrice: string
  longInputCacheMissPrice: string
  longCacheWritePrice: string
  longOutputPrice: string
}

interface ProviderDraft {
  existingName: string
  name: string
  baseURL: string
  originalBaseURL: string
  apiKey: string
  keepAPIKey: boolean
  apiKeyConfigured: boolean
  authFile: string
  originalAuthFile: string
  requestTimeout: string
  maxConcurrentRequests: string
  httpProxy: string
  originalHTTPProxy: string
  httpsProxy: string
  originalHTTPSProxy: string
  models: EditableProviderModel[]
}

class ProviderDraftValidationError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'ProviderDraftValidationError'
  }
}

function localProviderError(reason: unknown, fallback: string): string {
  return reason instanceof ProviderDraftValidationError ? reason.message : fallback
}

export function ProviderManagerDialog(props: {
  state: ProviderSettingsReadModel
  codexLogin: CodexLoginReadModel | null
  onProviderChange: (provider: string | null) => void
  onSave: (provider: string, target: ProviderUpdateTarget, existing: boolean) => Promise<void>
  onSetDefault: (provider: string, model: string) => Promise<void>
  onDiscoverModels: (provider: string) => Promise<readonly string[]>
  onStartCodexLogin: (provider: string) => Promise<void>
  onClearCodexLogin: (provider: string) => Promise<void>
  onRefreshUsage: (provider: string) => Promise<CodexUsageDomain>
  onRetrySettings: () => void
  onRetryCodexLogin: () => void
  onClose: () => void
  onError: (message: string) => void
}) {
  const [draft, setDraft] = useState<ProviderDraft | null>(null)
  const [selectedProviderName, setSelectedProviderName] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [defaultingProfile, setDefaultingProfile] = useState<string | null>(null)
  const [discovering, setDiscovering] = useState(false)
  const [discoveredModels, setDiscoveredModels] = useState<string[]>([])
  const [codexAction, setCodexAction] = useState<'starting' | 'clearing' | null>(null)
  const [codexUsage, setCodexUsage] = useState<CodexUsageDomain | null>(null)
  const [usageLoading, setUsageLoading] = useState(false)
  const [usageError, setUsageError] = useState<string | null>(null)
  const providerSelectionGeneration = useRef(0)
  const settings = props.state
  const codexAuth = props.codexLogin?.login

  const selectProvider = useCallback((provider: ProviderSettingsDomain) => {
    providerSelectionGeneration.current += 1
    setSelectedProviderName(provider.name)
    setDraft(providerDraft(provider))
    setCodexAction(null)
    setDefaultingProfile(null)
    setCodexUsage(null)
    setUsageLoading(false)
    setUsageError(null)
    setDiscoveredModels([])
    props.onProviderChange(provider.models.some((model) => model.type === 'openai-codex') ? provider.name : null)
  }, [props.onProviderChange])

  useEffect(() => {
    if (draft) return
    const provider = (selectedProviderName && settings.providers.find((item) => item.name === selectedProviderName))
      ?? settings.providers.find((item) => item.name === settings.defaultProvider)
      ?? settings.providers.find((item) => item.models.some((model) => model.type === 'openai-codex'))
      ?? settings.providers[0]
    if (provider) selectProvider(provider)
    else {
      providerSelectionGeneration.current += 1
      setSelectedProviderName(null)
      props.onProviderChange(null)
      setDraft(emptyProviderDraft())
    }
  }, [draft, props.onProviderChange, selectedProviderName, selectProvider, settings.defaultProvider, settings.providers])

  const save = async () => {
    if (!draft || saving) return
    const generation = providerSelectionGeneration.current
    setSaving(true)
    try {
      const input = providerInput(draft)
      const providerName = draft.name.trim()
      await props.onSave(providerName, input, Boolean(draft.existingName))
      if (generation !== providerSelectionGeneration.current) return
      setSelectedProviderName(providerName)
      setDraft(null)
    } catch (reason) {
      props.onError(localProviderError(reason, 'Provider settings could not be saved.'))
    } finally {
      setSaving(false)
    }
  }

  const discoverModels = async () => {
    if (!draft?.existingName || discovering) return
    const generation = providerSelectionGeneration.current
    const provider = draft.existingName
    setDiscovering(true)
    try {
      const models = await props.onDiscoverModels(provider)
      if (generation === providerSelectionGeneration.current) setDiscoveredModels([...models])
    } catch (reason) {
      props.onError('Provider model discovery failed.')
    } finally {
      setDiscovering(false)
    }
  }

  const setDefault = async (profile: string) => {
    if (!draft?.existingName || defaultingProfile !== null) return
    const generation = providerSelectionGeneration.current
    setDefaultingProfile(profile)
    try {
      await props.onSetDefault(draft.existingName, profile)
    } catch (reason) {
      props.onError('The default model could not be changed.')
    } finally {
      if (generation === providerSelectionGeneration.current) setDefaultingProfile(null)
    }
  }

  const startCodexLogin = async () => {
    if (!draft?.existingName) return
    const generation = providerSelectionGeneration.current
    const provider = draft.existingName
    setCodexAction('starting')
    try {
      await props.onStartCodexLogin(provider)
    } catch (reason) {
      props.onError('Codex sign-in could not be started.')
    } finally {
      if (generation === providerSelectionGeneration.current) setCodexAction(null)
    }
  }

  const clearCodexLogin = async () => {
    if (!draft?.existingName || !window.confirm('Sign out of Codex for the current Server Root?')) return
    const generation = providerSelectionGeneration.current
    const provider = draft.existingName
    setCodexAction('clearing')
    try {
      await props.onClearCodexLogin(provider)
      if (generation === providerSelectionGeneration.current) {
        setCodexUsage(null)
        setUsageError(null)
      }
    } catch (reason) {
      props.onError('Codex sign-out could not be completed.')
    } finally {
      if (generation === providerSelectionGeneration.current) setCodexAction(null)
    }
  }

  const refreshUsage = async () => {
    if (!draft?.existingName || usageLoading) return
    const generation = providerSelectionGeneration.current
    const provider = draft.existingName
    setUsageLoading(true)
    setUsageError(null)
    try {
      const usage = await props.onRefreshUsage(provider)
      if (generation === providerSelectionGeneration.current) setCodexUsage(usage)
    } catch (reason) {
      if (generation === providerSelectionGeneration.current) setUsageError('Codex usage is temporarily unavailable.')
    } finally {
      if (generation === providerSelectionGeneration.current) setUsageLoading(false)
    }
  }

  const updateModel = (index: number, patch: Partial<EditableProviderModel>) => {
    setDraft((current) => current ? { ...current, models: current.models.map((model, modelIndex) => modelIndex === index ? { ...model, ...patch } : model) } : current)
  }

  const duplicateModel = (index: number) => {
    setDraft((current) => {
      if (!current) return current
      const source = current.models[index]
      if (!source) return current
      const models = [...current.models]
      models.splice(index + 1, 0, {
        ...source,
        profile: duplicateProfileName(source.profile, current.models),
        parametersMode: 'replace',
        parametersSourceProfile: '',
        parametersJSON: '{}',
      })
      return { ...current, models }
    })
  }

  const savedCodexProvider = settings.providers.find((provider) => provider.name === draft?.existingName)?.models.some((model) => model.type === 'openai-codex') ?? false
  const codexReadState = props.codexLogin?.availability.status
  const codexUnavailable = codexReadState !== 'ready'

  return (
    <div className="model-dialog-backdrop provider-dialog-backdrop">
      <section className="provider-dialog" role="dialog" aria-modal="true" aria-labelledby="provider-dialog-title">
        <header className="provider-dialog-header">
          <div>
            <span className="eyebrow">Server Root settings</span>
            <h2 id="provider-dialog-title">Providers & models</h2>
            <p>{settings.serverRoot ? `${settings.serverRoot} · ${settings.configPath}` : 'Reading current Server Root'}</p>
          </div>
          <button className="model-dialog-close" disabled={saving || defaultingProfile !== null || codexAction !== null} onClick={props.onClose} aria-label="Close">×</button>
        </header>
        {settings.status === 'loading' || !draft ? (
          <div className="provider-loading">Reading Server Root settings…</div>
        ) : settings.status === 'error' && settings.providers.length === 0 ? (
          <div className="provider-error" role="alert"><span>Provider settings are unavailable.</span><button className="secondary-button" onClick={props.onRetrySettings}>Retry</button></div>
        ) : (
          <div className="provider-dialog-body">
            {settings.status === 'stale' && <div className="sync-status" role="status"><span>Provider settings are offline; showing the last synchronized settings.</span><button onClick={props.onRetrySettings}>Retry synchronization</button></div>}
            <aside className="provider-list">
              {settings.providers.map((provider) => (
                <button className={draft.existingName === provider.name ? 'selected' : ''} onClick={() => selectProvider(provider)} key={provider.name}>
                  <strong>{provider.name}</strong>
                  <small>{provider.models.length} {provider.models.length === 1 ? 'model' : 'models'}</small>
                </button>
              ))}
              <button className={!draft.existingName ? 'selected add-provider' : 'add-provider'} onClick={() => { providerSelectionGeneration.current += 1; setSelectedProviderName(null); props.onProviderChange(null); setDraft(emptyProviderDraft()); setCodexAction(null); setDefaultingProfile(null); setCodexUsage(null); setUsageLoading(false); setUsageError(null); setDiscoveredModels([]) }}><PlusIcon /> Add provider</button>
            </aside>
            <div className="provider-editor">
              <section className="settings-section">
                <div className="settings-section-title"><div><h3>Connection</h3><p>Proxy settings apply to every model in this provider.</p></div>{draft.existingName && <code>{draft.existingName}.yaml</code>}</div>
                {draft.existingName && <p className="field-help">Existing endpoint and auth-file details that are hidden or normalized by the safe settings view are preserved unless you change their visible value.</p>}
                <div className="settings-grid">
                  <label>Name<input value={draft.name} disabled={Boolean(draft.existingName)} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="openai" /></label>
                  <label>Request timeout<input value={draft.requestTimeout} onChange={(event) => setDraft({ ...draft, requestTimeout: event.target.value })} placeholder="60s" /></label>
                  <label>Max concurrent requests<input type="number" min="0" value={draft.maxConcurrentRequests} onChange={(event) => setDraft({ ...draft, maxConcurrentRequests: event.target.value })} placeholder="Unlimited" /></label>
                  <label className="wide">Base URL<input value={draft.baseURL} onChange={(event) => setDraft({ ...draft, baseURL: event.target.value })} placeholder={draft.existingName && !draft.baseURL ? 'Current endpoint is hidden; leave unchanged to preserve' : 'https://api.openai.com/v1'} /></label>
                  <label>HTTP proxy<input value={draft.httpProxy} onChange={(event) => setDraft({ ...draft, httpProxy: event.target.value })} placeholder="http://127.0.0.1:7890" /></label>
                  <label>HTTPS proxy<input value={draft.httpsProxy} onChange={(event) => setDraft({ ...draft, httpsProxy: event.target.value })} placeholder="http://127.0.0.1:7890" /></label>
                  <label className="wide">API key / environment variable<input value={draft.apiKey} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value, keepAPIKey: false })} placeholder={draft.apiKeyConfigured ? 'Configured; leave empty and keep the option below to leave unchanged' : '$OPENAI_API_KEY'} /></label>
                  {draft.apiKeyConfigured && <label className="checkbox-field wide"><input type="checkbox" checked={draft.keepAPIKey} onChange={(event) => setDraft({ ...draft, keepAPIKey: event.target.checked })} /> Keep current API key</label>}
                </div>
              </section>

              {savedCodexProvider && (
                <section className="settings-section codex-auth-card">
                  <div className="settings-section-title"><h3>Codex sign-in</h3><span className={`auth-status ${codexAction ?? codexAuth?.status ?? codexReadState ?? 'signed_out'}`}>{codexStatusLabel(codexAuth?.status, codexReadState, codexAction)}</span></div>
                  {codexReadState === 'stale' && <p className="sync-status"><span>Codex sign-in status is offline; showing the last synchronized status.</span><button onClick={props.onRetryCodexLogin}>Retry synchronization</button></p>}
                  {codexReadState === 'error' && <p className="settings-error">Codex sign-in status is unavailable. <button className="plain-button" onClick={props.onRetryCodexLogin}>Retry</button></p>}
                  {codexAuth?.status === 'pending' && <div className="device-login"><strong>{codexAuth.userCode}</strong><button className="secondary-button" onClick={() => void copyText(codexAuth.userCode)}>Copy code</button>{codexAuth.verificationURL && <a href={codexAuth.verificationURL} target="_blank" rel="noreferrer">Open sign-in page</a>}</div>}
                  {codexAuth?.status === 'error' && <p className="settings-error">Codex sign-in failed.</p>}
                  <div className="inline-actions">
                    {codexAuth?.status !== 'pending' && codexAuth?.status !== 'signed_in' && <button className="primary-button" disabled={!savedCodexProvider || codexAction !== null || codexUnavailable} onClick={() => void startCodexLogin()}>{codexAction === 'starting' ? 'Starting…' : 'Sign in to Codex'}</button>}
                    {(codexAuth?.status === 'signed_in' || codexAuth?.status === 'expired') && <button className="secondary-button" disabled={codexAction !== null || codexUnavailable} onClick={() => void clearCodexLogin()}>{codexAction === 'clearing' ? 'Signing out…' : 'Sign out'}</button>}
                    {!savedCodexProvider && <small>Save the Codex provider before signing in.</small>}
                  </div>
                  {(codexAuth?.status === 'signed_in' || codexAuth?.status === 'expired') && (
                    <div className="usage-block">
                      <div className="usage-heading">
                        <strong>Usage</strong>
                        <button className="secondary-button compact" disabled={usageLoading || !savedCodexProvider} onClick={() => void refreshUsage()}>{usageLoading ? 'Fetching…' : 'Refresh usage'}</button>
                      </div>
                      {usageError && <p className="settings-error">{usageError}</p>}
                      {codexUsage && <CodexUsageView usage={codexUsage} />}
                    </div>
                  )}
                </section>
              )}

              <section className="settings-section">
                <div className="settings-section-title">
                  <div><h3>Models</h3><p>Reasoning levels use unified display names; mapped values can be strings, numbers, booleans, or objects.</p></div>
                  <div className="inline-actions">
                    <button className="secondary-button compact" disabled={!draft.existingName || discovering || settings.status !== 'ready'} onClick={() => void discoverModels()}>{discovering ? 'Fetching…' : 'Fetch from provider'}</button>
                    <button className="secondary-button compact" onClick={() => setDraft({ ...draft, models: [...draft.models, emptyProviderModel()] })}><PlusIcon /> Add model</button>
                  </div>
                </div>
                <div className="provider-models">
                  {draft.models.map((model, index) => {
                    const isDefault = draft.existingName === settings.defaultProvider && model.profile === settings.defaultModel
                    const reasoningLevels = reasoningLevelOptions(model.reasoningLevelsJSON)
                    return <article className="provider-model-card" key={`${index}-${model.profile}`}>
                      <div className="provider-model-heading"><strong>{model.profile || `Model ${index + 1}`}</strong><div className="inline-actions">{isDefault ? <span className="default-badge">Default</span> : <button className="plain-button" disabled={!draft.existingName || !model.profile || settings.status !== 'ready' || defaultingProfile !== null} onClick={() => void setDefault(model.profile)}>{defaultingProfile === model.profile ? 'Setting…' : 'Set as default'}</button>}<button className="plain-button" onClick={() => duplicateModel(index)}>Duplicate</button><button className="plain-button danger" disabled={draft.models.length === 1} onClick={() => setDraft({ ...draft, models: draft.models.filter((_, modelIndex) => index !== modelIndex) })}>Remove</button></div></div>
                      <div className="settings-grid model-grid">
                        {discoveredModels.length > 0 && <label className="wide model-catalog-select">Choose from model list<select value={discoveredModels.includes(model.id) ? model.id : ''} onChange={(event) => {
                          const selectedID = event.target.value
                          if (selectedID) updateModel(index, { id: selectedID, profile: !model.profile || model.profile === model.id ? selectedID : model.profile })
                        }}><option value="">Select a model ({discoveredModels.length} fetched)</option>{discoveredModels.map((modelID) => <option value={modelID} key={modelID}>{modelID}</option>)}</select></label>}
                        <label>Profile name<input value={model.profile} onChange={(event) => updateModel(index, { profile: event.target.value })} placeholder="gpt-5.5" /></label>
                        <label>Model ID<input value={model.id} onChange={(event) => updateModel(index, { id: event.target.value })} placeholder="Or enter manually" /></label>
                        <label>API type<select value={model.type || 'openai-chat'} onChange={(event) => {
                          const type = event.target.value
                          updateModel(index, { type, compatibility: type === 'openai-chat' ? model.compatibility : '' })
                        }}><option value="openai-chat">OpenAI Chat</option><option value="openai-responses">OpenAI Responses</option><option value="openai-codex">OpenAI Codex</option><option value="anthropic-messages">Anthropic Messages</option></select></label>
                        <label>Compatibility<select value={model.compatibility} disabled={model.type !== 'openai-chat'} onChange={(event) => updateModel(index, { compatibility: event.target.value })}><option value="">Provider default</option><option value="openai">Standard OpenAI</option><option value="kimi">Kimi</option></select></label>
                        <label className="checkbox-field"><input type="checkbox" checked={model.supportsImages} onChange={(event) => updateModel(index, { supportsImages: event.target.checked })} /> Supports image input</label>
                        <label>Developer role<select value={model.developerRole} onChange={(event) => updateModel(index, { developerRole: event.target.value })}><option value="">Keep developer</option><option value="system">Map to system</option></select></label>
                        <label>Context Window<input type="number" min="0" value={model.contextWindow} onChange={(event) => updateModel(index, { contextWindow: event.target.value })} placeholder="400000" /></label>
                        <label>Input Limit<input type="number" min="0" value={model.inputLimit} onChange={(event) => updateModel(index, { inputLimit: event.target.value })} placeholder="272000" /></label>
                        <label>Output Limit<input type="number" min="0" value={model.outputLimit} onChange={(event) => updateModel(index, { outputLimit: event.target.value })} placeholder="128000" /></label>
                        <label>Currency<input value={model.pricingCurrency} onChange={(event) => updateModel(index, { pricingCurrency: event.target.value.toUpperCase() })} placeholder="USD" maxLength={3} /></label>
                        <label>Input tokens (cache hit)<input type="number" min="0" step="any" value={model.inputCacheHitPrice} onChange={(event) => updateModel(index, { inputCacheHitPrice: event.target.value })} placeholder="per 1M tokens" /></label>
                        <label>Input tokens (cache miss)<input type="number" min="0" step="any" value={model.inputCacheMissPrice} onChange={(event) => updateModel(index, { inputCacheMissPrice: event.target.value })} placeholder="per 1M tokens" /></label>
                        <label>Cache-write tokens<input type="number" min="0" step="any" value={model.cacheWritePrice} onChange={(event) => updateModel(index, { cacheWritePrice: event.target.value })} placeholder="defaults to cache miss" /></label>
                        <label>Output tokens<input type="number" min="0" step="any" value={model.outputPrice} onChange={(event) => updateModel(index, { outputPrice: event.target.value })} placeholder="per 1M tokens" /></label>
                        <label>Long context threshold<input type="number" min="1" step="1" value={model.longContextThreshold} onChange={(event) => updateModel(index, { longContextThreshold: event.target.value })} placeholder="e.g. 272000" /></label>
                        <label>Long input (cache hit)<input type="number" min="0" step="any" value={model.longInputCacheHitPrice} onChange={(event) => updateModel(index, { longInputCacheHitPrice: event.target.value })} placeholder="optional" /></label>
                        <label>Long input (cache miss)<input type="number" min="0" step="any" value={model.longInputCacheMissPrice} onChange={(event) => updateModel(index, { longInputCacheMissPrice: event.target.value })} placeholder="optional" /></label>
                        <label>Long cache-write<input type="number" min="0" step="any" value={model.longCacheWritePrice} onChange={(event) => updateModel(index, { longCacheWritePrice: event.target.value })} placeholder="optional" /></label>
                        <label>Long output tokens<input type="number" min="0" step="any" value={model.longOutputPrice} onChange={(event) => updateModel(index, { longOutputPrice: event.target.value })} placeholder="optional" /></label>
                        <p className="field-help wide">Prices are in the selected currency per 1 million tokens. Top-level prices are short-context prices; long-context prices apply above the threshold. Cache-write can be configured separately.</p>
                        <div className="field-help wide">
                          {model.parametersMode === 'preserve'
                            ? 'Existing request parameters are hidden and will be preserved when saving this provider.'
                            : 'Replacement request parameters are sent explicitly for this model.'}
                        </div>
                        <label className="checkbox-field wide"><input type="checkbox" checked={model.parametersMode === 'replace'} onChange={(event) => updateModel(index, event.target.checked ? { parametersMode: 'replace', parametersJSON: model.parametersJSON || '{}' } : { parametersMode: 'preserve', parametersJSON: '' })} /> Replace hidden request parameters</label>
                        <label className="wide">Extra request parameters (JSON)<textarea value={model.parametersJSON} disabled={model.parametersMode === 'preserve'} placeholder={model.parametersMode === 'preserve' ? 'Hidden; preserved unless replacement is enabled' : '{}'} onChange={(event) => updateModel(index, { parametersMode: 'replace', parametersJSON: event.target.value })} rows={3} spellCheck={false} /></label>
                      </div>
                      <details className="reasoning-config" open={Boolean(model.reasoningParameter)}>
                        <summary>Reasoning config {model.reasoningParameter ? <code>{model.reasoningParameter}</code> : <small>Leave empty to use Pi recommended defaults</small>}</summary>
                        <div className="settings-grid model-grid">
                          <label>Parameter path<input value={model.reasoningParameter} onChange={(event) => updateModel(index, { reasoningParameter: event.target.value })} placeholder="reasoning.effort" /></label>
                          <label>Default level<select value={reasoningLevels.includes(model.reasoningDefault) ? model.reasoningDefault : ''} disabled={reasoningLevels.length === 0} onChange={(event) => updateModel(index, { reasoningDefault: event.target.value })}><option value="">{reasoningLevels.length === 0 ? 'Fill in the level mapping first' : 'No default level'}</option>{reasoningLevels.map((level) => <option value={level} key={level}>{reasoningLevelLabel(level)} ({level})</option>)}</select></label>
                          <label className="wide">Level mapping (JSON)<textarea value={model.reasoningLevelsJSON} onChange={(event) => updateModel(index, { reasoningLevelsJSON: event.target.value })} rows={4} spellCheck={false} placeholder={'{"low":"low","high":"high"}'} /></label>
                        </div>
                      </details>
                    </article>
                  })}
                </div>
              </section>
            </div>
          </div>
        )}
        <footer className="model-dialog-actions"><button className="secondary-button" disabled={saving || defaultingProfile !== null || codexAction !== null} onClick={props.onClose}>Cancel</button><button className="primary-button" disabled={!draft || saving || defaultingProfile !== null || settings.status !== 'ready'} onClick={() => void save()}>{saving ? 'Saving…' : 'Save settings'}</button></footer>
      </section>
    </div>
  )
}

function providerDraft(provider: ProviderSettingsDomain): ProviderDraft {
  return {
    existingName: provider.name,
    name: provider.name,
    baseURL: provider.baseURL,
    originalBaseURL: provider.baseURL,
    // The resource intentionally never carries the secret. An empty value
    // plus keep_api_key preserves it; only an explicitly entered value
    // replaces it.
    apiKey: '',
    keepAPIKey: provider.apiKeyConfigured,
    apiKeyConfigured: provider.apiKeyConfigured,
    authFile: provider.authFile,
    originalAuthFile: provider.authFile,
    requestTimeout: provider.requestTimeout,
    maxConcurrentRequests: provider.maxConcurrentRequests ? String(provider.maxConcurrentRequests) : '',
    httpProxy: provider.httpProxy,
    originalHTTPProxy: provider.httpProxy,
    httpsProxy: provider.httpsProxy,
    originalHTTPSProxy: provider.httpsProxy,
    models: provider.models.map(editableProviderModel),
  }
}

function emptyProviderDraft(): ProviderDraft {
  return { existingName: '', name: '', baseURL: '', originalBaseURL: '', apiKey: '', keepAPIKey: false, apiKeyConfigured: false, authFile: '', originalAuthFile: '', requestTimeout: '60s', maxConcurrentRequests: '', httpProxy: '', originalHTTPProxy: '', httpsProxy: '', originalHTTPSProxy: '', models: [emptyProviderModel()] }
}

function editableProviderModel(model: ProviderModelDomain): EditableProviderModel {
  return {
    profile: model.profile,
    id: model.id,
    type: model.type || 'openai-chat',
    compatibility: model.compatibility,
    supportsImages: model.input.includes('image'),
    developerRole: model.developerRole,
    contextWindow: model.contextWindow ? String(model.contextWindow) : '',
    inputLimit: model.inputLimit ? String(model.inputLimit) : '',
    outputLimit: model.outputLimit ? String(model.outputLimit) : '',
    // Model parameters are deliberately not part of the safe settings
    // resource. Existing models therefore start in preserve mode; the UI
    // never presents {} as if it were the current durable value.
    parametersMode: 'preserve',
    parametersSourceProfile: model.profile,
    parametersJSON: '',
    reasoningParameter: model.reasoningConfig.parameter,
    reasoningDefault: model.reasoningConfig.default,
    reasoningLevelsJSON: prettyJSON(Object.fromEntries(model.reasoningConfig.levels.map((level) => [level.name, level.value]))),
    pricingCurrency: model.pricing?.currency ?? '',
    inputCacheHitPrice: priceText(model.pricing, 'inputCacheHit'),
    inputCacheMissPrice: priceText(model.pricing, 'inputCacheMiss'),
    cacheWritePrice: priceText(model.pricing, 'cacheWrite'),
    outputPrice: priceText(model.pricing, 'output'),
    longContextThreshold: model.pricing?.longContextThreshold ? String(model.pricing.longContextThreshold) : '',
    longInputCacheHitPrice: tierPriceText(model.pricing?.longContext, 'inputCacheHit'),
    longInputCacheMissPrice: tierPriceText(model.pricing?.longContext, 'inputCacheMiss'),
    longCacheWritePrice: tierPriceText(model.pricing?.longContext, 'cacheWrite'),
    longOutputPrice: tierPriceText(model.pricing?.longContext, 'output'),
  }
}

function duplicateProfileName(profile: string, models: EditableProviderModel[]): string {
  const base = profile.trim()
  if (!base) return ''
  const taken = new Set(models.map((model) => model.profile.trim()))
  for (let suffix = 1; ; suffix += 1) {
    const candidate = suffix === 1 ? `${base} copy` : `${base} copy ${suffix}`
    if (!taken.has(candidate)) return candidate
  }
}

function emptyProviderModel(): EditableProviderModel {
  return { profile: '', id: '', type: 'openai-chat', compatibility: '', supportsImages: false, developerRole: '', contextWindow: '', inputLimit: '', outputLimit: '', parametersMode: 'replace', parametersSourceProfile: '', parametersJSON: '{}', reasoningParameter: '', reasoningDefault: '', reasoningLevelsJSON: '{}', pricingCurrency: 'USD', inputCacheHitPrice: '', inputCacheMissPrice: '', cacheWritePrice: '', outputPrice: '', longContextThreshold: '', longInputCacheHitPrice: '', longInputCacheMissPrice: '', longCacheWritePrice: '', longOutputPrice: '' }
}

function providerInput(draft: ProviderDraft): ProviderUpdateTarget {
  if (!draft.name.trim()) throw new ProviderDraftValidationError('Provider name is required.')
  if (!draft.baseURL.trim() && !draft.existingName) throw new ProviderDraftValidationError('Base URL is required for a new provider.')
  if (draft.models.length === 0) throw new ProviderDraftValidationError('At least one model is required.')
  const existing = Boolean(draft.existingName)
  const writeMode = (value: string, original: string): 'preserve' | 'replace' => existing && value.trim() === original.trim() ? 'preserve' : 'replace'
  return {
    base_url_mode: writeMode(draft.baseURL, draft.originalBaseURL),
    base_url: draft.baseURL.trim(),
    api_key: draft.apiKey.trim(),
    keep_api_key: draft.keepAPIKey,
    auth_file_mode: writeMode(draft.authFile, draft.originalAuthFile),
    auth_file: draft.authFile.trim(),
    request_timeout: draft.requestTimeout.trim(),
    http_proxy_mode: writeMode(draft.httpProxy, draft.originalHTTPProxy),
    max_concurrent_requests: draft.maxConcurrentRequests.trim() ? Number(draft.maxConcurrentRequests) : 0,
    http_proxy: draft.httpProxy.trim(),
    https_proxy_mode: writeMode(draft.httpsProxy, draft.originalHTTPSProxy),
    https_proxy: draft.httpsProxy.trim(),
    models: draft.models.map((model, index) => {
      if (!model.profile.trim() || !model.id.trim()) throw new ProviderDraftValidationError(`Model ${index + 1} needs a profile name and model ID.`)
      let reasoningLevels: Record<string, unknown>
      try { reasoningLevels = parseJSONRecord(model.reasoningLevelsJSON, 'Level mapping') } catch { throw new ProviderDraftValidationError('Level mapping must be a JSON object.') }
      if (model.reasoningDefault.trim() && !(model.reasoningDefault.trim() in reasoningLevels)) throw new ProviderDraftValidationError('The default reasoning level must be in the level mapping.')
      const hasLongPricing = model.longInputCacheHitPrice.trim() || model.longInputCacheMissPrice.trim() || model.longCacheWritePrice.trim() || model.longOutputPrice.trim()
      const hasBasePricing = model.inputCacheHitPrice.trim() || model.inputCacheMissPrice.trim() || model.cacheWritePrice.trim() || model.outputPrice.trim()
      const pricing = hasBasePricing || hasLongPricing
        ? {
            input_cache_hit: priceNumber(model.inputCacheHitPrice, 'Cache-hit price'),
            input_cache_miss: priceNumber(model.inputCacheMissPrice, 'Cache-miss price'),
            ...(model.cacheWritePrice.trim() ? { cache_write: priceNumber(model.cacheWritePrice, 'Cache-write price') } : {}),
            output: priceNumber(model.outputPrice, 'Output price'),
            currency: model.pricingCurrency.trim().toUpperCase() || 'USD',
            ...(hasLongPricing ? {
              long_context_threshold: positiveInteger(model.longContextThreshold, 'Long context threshold'),
              long_context: {
                input_cache_hit: priceNumber(model.longInputCacheHitPrice, 'Long cache-hit price'),
                input_cache_miss: priceNumber(model.longInputCacheMissPrice, 'Long cache-miss price'),
                cache_write: priceNumber(model.longCacheWritePrice, 'Long cache-write price'),
                output: priceNumber(model.longOutputPrice, 'Long output price'),
              },
            } : {}),
          }
        : undefined
      if (pricing && !/^[A-Z]{3}$/.test(pricing.currency)) throw new ProviderDraftValidationError('Currency must be a 3-letter code.')
      const target = {
        profile: model.profile.trim(),
        id: model.id.trim(),
        type: model.type,
        compatibility: model.compatibility,
        input: model.supportsImages ? ['text', 'image'] : ['text'],
        developer_role: model.developerRole,
        context_window: model.contextWindow ? Number(model.contextWindow) : 0,
        input_limit: model.inputLimit ? Number(model.inputLimit) : 0,
        output_limit: model.outputLimit ? Number(model.outputLimit) : 0,
        parameters_mode: model.parametersMode,
        reasoning_config: {
          parameter: model.reasoningParameter.trim(),
          default: model.reasoningDefault.trim(),
          levels: reasoningLevels as JsonObject,
        },
        pricing,
      }
      if (model.parametersMode === 'preserve') {
        return { ...target, parameters_source_profile: model.parametersSourceProfile.trim() || model.profile.trim() }
      } else {
        try { return { ...target, parameters: parseJSONRecord(model.parametersJSON || '{}', 'Request parameters') as JsonObject } } catch { throw new ProviderDraftValidationError('Request parameters must be a JSON object.') }
      }
    }),
  }
}

function priceText(pricing: ProviderModelDomain['pricing'], field: keyof Pick<NonNullable<ProviderModelDomain['pricing']>, 'inputCacheHit' | 'inputCacheMiss' | 'cacheWrite' | 'output'>): string {
  const value = pricing?.[field]
  return typeof value === 'number' ? String(value) : ''
}

function tierPriceText(tier: NonNullable<ProviderModelDomain['pricing']>['longContext'] | undefined, field: 'inputCacheHit' | 'inputCacheMiss' | 'cacheWrite' | 'output'): string {
  const value = tier?.[field]
  return typeof value === 'number' ? String(value) : ''
}

function priceNumber(value: string, label: string): number {
  const parsed = Number(value)
  if (!value.trim() || !Number.isFinite(parsed) || parsed < 0) throw new ProviderDraftValidationError(`${label} must be a non-negative number.`)
  return parsed
}

function positiveInteger(value: string, label: string): number {
  const parsed = Number(value)
  if (!value.trim() || !Number.isInteger(parsed) || parsed <= 0) throw new ProviderDraftValidationError(`${label} must be a positive integer.`)
  return parsed
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
  return { signed_out: 'Signed out', pending: 'Waiting for authorization', signed_in: 'Signed in', expired: 'Expired', error: 'Auth error' }[status ?? 'signed_out'] ?? status ?? 'Signed out'
}

function codexStatusLabel(status: string | undefined, readState: string | undefined, action: 'starting' | 'clearing' | null): string {
  if (action === 'starting') return 'Starting sign-in…'
  if (action === 'clearing') return 'Signing out…'
  if (!readState) return 'Reading status…'
  if (readState === 'loading') return 'Reading status…'
  if (readState === 'error') return 'Status unavailable'
  if (readState === 'stale') return 'Offline'
  return codexAuthLabel(status)
}

export function windowDurationLabel(seconds: number): string {
  if (seconds <= 0) return 'usage'
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`
  if (seconds < 86400) return `${Math.round(seconds / 3600)} h`
  return `${Math.round(seconds / 86400)} d`
}

export function creditsLabel(credits: CodexUsageCreditsDomain): string {
  if (credits.unlimited) return 'Unlimited'
  if (!credits.hasCredits) return 'no separate credits'
  return `balance ${credits.balance}`
}

export function usageWindowRows(set?: CodexUsageWindowSetDomain): { label: string; window?: CodexUsageWindowDomain }[] {
  if (!set) return []
  const rows: { label: string; window?: CodexUsageWindowDomain }[] = []
  if (set.primaryWindow) rows.push({ label: `Window · ${windowDurationLabel(set.primaryWindow.limitWindowSeconds)}`, window: set.primaryWindow })
  if (set.secondaryWindow) rows.push({ label: `Window · ${windowDurationLabel(set.secondaryWindow.limitWindowSeconds)}`, window: set.secondaryWindow })
  return rows
}

function CodexUsageView({ usage }: { usage: CodexUsageDomain }) {
  const windows = usageWindowRows(usage.rateLimit)
  const limited = usage.rateLimit?.limitReached === true
  return (
    <div className="usage-view">
      <p className="usage-plan">Plan: <strong>{capitalize(usage.planType)}</strong></p>
      {usage.rateLimit && !usage.rateLimit.allowed && <p className="settings-error">Usage is currently restricted.</p>}
      {windows.map((window) => (
        <UsageWindowRow key={window.label} window={window.window} label={window.label} limited={limited} />
      ))}
      {!usage.rateLimit && <p className="usage-muted">No rate limit data available.</p>}
      {usage.credits && <p className="usage-credits">Credits: {creditsLabel(usage.credits)}</p>}
      {(usage.additionalRateLimits ?? []).map((additional) => (
        <UsageWindowRow
          key={additional.limitName}
          window={additional.rateLimit?.primaryWindow}
          label={additional.limitName}
          limited={additional.rateLimit?.limitReached === true}
        />
      ))}
    </div>
  )
}

function UsageWindowRow({ window, label, limited }: { window?: CodexUsageWindowDomain; label: string; limited: boolean }) {
  if (!window) return null
  const percent = Math.max(0, Math.min(100, window.usedPercent))
  const danger = limited || percent >= 90
  return (
    <div className="usage-row">
      <div className="usage-row-label">
        <span>{label}</span>
        <span className="usage-percent">{percent}%</span>
        {danger && <span className="usage-badge">Limited</span>}
      </div>
      <div className={`usage-meter${danger ? ' danger' : ''}`}><span style={{ width: `${percent}%` }} /></div>
      {window.resetAt > 0 && <small className="usage-reset">Resets {new Date(window.resetAt * 1000).toLocaleString()}</small>}
    </div>
  )
}

function capitalize(value: string): string {
  if (!value) return value
  return value.charAt(0).toUpperCase() + value.slice(1)
}
