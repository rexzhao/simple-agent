// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import type { ComponentProps } from 'react'
import { ProviderManagerDialog } from './ProviderManagerDialog'
import type { CodexLoginReadModel } from '../repositories/codexLogin'
import type { ProviderSettingsReadModel, ProviderSettingsDomain } from '../repositories/providerSettings'

const deferred = <T,>() => {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function model(profile: string, type = 'openai-chat') {
  return {
    profile, id: `${profile}-id`, type, compatibility: '', input: ['text'] as const,
    developerRole: '', contextWindow: 32000, inputLimit: 0, outputLimit: 0,
    reasoningConfig: { type: '', parameter: '', default: '', levels: [] as const }, pricing: null,
  }
}

function provider(name: string, models = [model('fast')]): ProviderSettingsDomain {
  return {
    name, baseURL: 'https://example.test/v1', apiKeyConfigured: true,
    authFile: `${name}.json`, requestTimeout: '30s', httpProxy: 'http://proxy.example.test:8080',
    httpsProxy: '', maxConcurrentRequests: 2, models,
  }
}

function state(status: ProviderSettingsReadModel['status'] = 'ready', providers = [provider('alpha')]): ProviderSettingsReadModel {
  return {
    status, serverRoot: '/srv', configPath: '/srv/sai.yaml', defaultProvider: providers[0]?.name ?? '', defaultModel: providers[0]?.models[0]?.profile ?? '',
    providers, availability: { status },
  }
}

function login(providerName: string, status: 'signed_out' | 'pending' | 'signed_in' | 'expired' | 'error' = 'signed_out'): CodexLoginReadModel {
  return {
    status: 'ready', provider: providerName,
    availability: { status: 'ready' },
    login: { provider: providerName, status, loginID: '', userCode: status === 'pending' ? 'ABCD-EFGH' : '', verificationURL: status === 'pending' ? 'https://auth.example.test/device' : '', refreshable: status === 'signed_in', errorCode: status === 'error' ? 'sign_in_failed' : '', errorMessage: status === 'error' ? 'Codex sign-in failed.' : '' },
  }
}

function renderDialog(overrides: Partial<ComponentProps<typeof ProviderManagerDialog>> = {}) {
  const onError = vi.fn()
  const props: ComponentProps<typeof ProviderManagerDialog> = {
    state: state(), codexLogin: null, onProviderChange: vi.fn(),
    onSave: vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSave']>(async () => {}), onSetDefault: vi.fn(async () => {}), onDiscoverModels: vi.fn(async () => ['alpha-model']), onSearchModelCatalog: vi.fn(async () => []),
    onStartCodexLogin: vi.fn(async () => {}), onClearCodexLogin: vi.fn(async () => {}), onRefreshUsage: vi.fn(async () => ({ planType: 'pro' as const, rateLimit: undefined, additionalRateLimits: [], credits: undefined })),
    onRetrySettings: vi.fn(), onRetryCodexLogin: vi.fn(), onClose: vi.fn(), onError, ...overrides,
  }
  const result = render(<ProviderManagerDialog {...props} />)
  return { ...result, props, onError }
}

afterEach(cleanup)

describe('ProviderManagerDialog application integration', () => {
  it('keeps saving after command acknowledgement and only settles when the callback authority barrier resolves', async () => {
    const pending = deferred<void>()
    const onSave = vi.fn(() => pending.promise)
    renderDialog({ onSave })
    await vi.waitFor(() => expect(screen.getByDisplayValue('30s')).toBeTruthy())
    fireEvent.change(screen.getByLabelText('Request timeout'), { target: { value: '45s' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Saving…' })).toBeTruthy())
    expect(onSave).toHaveBeenCalledTimes(1)
    pending.resolve()
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Save settings' })).toBeTruthy())
  })

  it('uses the same authority barrier for create and keeps the create target complete', async () => {
    const pending = deferred<void>()
    const onSave = vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSave']>(() => pending.promise)
    renderDialog({ onSave })
    fireEvent.click(screen.getByRole('button', { name: 'Add provider' }))
    fireEvent.change(screen.getByRole('textbox', { name: 'Name' }), { target: { value: 'new-provider' } })
    fireEvent.change(screen.getByRole('textbox', { name: 'Base URL' }), { target: { value: 'https://new.example.test/v1' } })
    fireEvent.click(screen.getByRole('button', { name: /Model 1/ }))
    fireEvent.change(screen.getByRole('textbox', { name: 'Profile name' }), { target: { value: 'default' } })
    fireEvent.change(screen.getByRole('textbox', { name: 'Model ID' }), { target: { value: 'new-model' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Saving…' })).toBeTruthy())
    expect(onSave.mock.calls[0]![2]).toBe(false)
    pending.resolve()
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Save settings' })).toBeTruthy())
  })

  it('keeps the default action pending until its authority callback resolves', async () => {
    const pending = deferred<void>()
    const onSetDefault = vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSetDefault']>(() => pending.promise)
    const alpha = provider('alpha', [model('fast'), model('slow')])
    renderDialog({ state: state('ready', [alpha]), onSetDefault })
    fireEvent.click(screen.getByRole('button', { name: /slow/ }))
    await vi.waitFor(() => expect(screen.getByRole('button', { name: /Make default/ })).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: /Make default/ }))
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Setting…' })).toBeTruthy())
    expect(onSetDefault).toHaveBeenCalledWith('alpha', 'slow')
    pending.resolve()
    await vi.waitFor(() => expect(screen.getByRole('button', { name: /Make default/ })).toBeTruthy())
  })

  it('uses preserve intents for safe projections and explicit replace for hidden parameters and API keys', async () => {
    const onSave = vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSave']>(async () => {})
    renderDialog({ onSave })
    await vi.waitFor(() => expect(screen.getByDisplayValue('30s')).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: /fast/ }))
    expect((screen.getByRole('textbox', { name: 'Extra request parameters (JSON)' }) as HTMLTextAreaElement).value).toBe('')
    expect((screen.getByRole('textbox', { name: 'Extra request parameters (JSON)' }) as HTMLTextAreaElement).disabled).toBe(true)
    expect(screen.getByText('Existing request parameters are hidden and will be preserved when saving this provider.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    const preserved = onSave.mock.calls[0]![1]
    expect(preserved.base_url_mode).toBe('preserve')
    expect(preserved.auth_file_mode).toBe('preserve')
    expect(preserved.http_proxy_mode).toBe('preserve')
    expect(preserved.models[0].parameters_mode).toBe('preserve')
    expect('parameters' in preserved.models[0]).toBe(false)
    expect(preserved.api_key).toBe('')
    expect(preserved.keep_api_key).toBe(true)

    // The authority refresh returns to the provider editor. Change the
    // provider secret there, then select the model for its hidden parameters.
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Save settings' })).toBeTruthy())
    fireEvent.change(screen.getByRole('textbox', { name: 'API key / environment variable' }), { target: { value: 'new-secret' } })
    fireEvent.click(screen.getByRole('button', { name: /fast/ }))
    // A user action explicitly opts into replacing the hidden value.
    fireEvent.click(screen.getByRole('checkbox', { name: 'Replace hidden request parameters' }))
    fireEvent.change(screen.getByRole('textbox', { name: 'Extra request parameters (JSON)' }), { target: { value: '{"temperature":0.2}' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(2))
    const replaced = onSave.mock.calls[1]![1]
    expect(replaced.models[0].parameters_mode).toBe('replace')
    expect(replaced.models[0].parameters).toEqual({ temperature: 0.2 })
    expect(replaced.api_key).toBe('new-secret')
    expect(replaced.keep_api_key).toBe(false)
  })

  it('can save other fields when the safe endpoint projection is intentionally empty', async () => {
    const onSave = vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSave']>(async () => {})
    renderDialog({ state: state('ready', [{ ...provider('opaque'), baseURL: '' }]), onSave })
    await vi.waitFor(() => expect(screen.getByDisplayValue('30s')).toBeTruthy())
    fireEvent.change(screen.getByLabelText('Request timeout'), { target: { value: '45s' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    const target = onSave.mock.calls[0]![1]
    expect(target.base_url).toBe('')
    expect(target.base_url_mode).toBe('preserve')
  })

  it('isolates late discover results after provider selection changes', async () => {
    const pending = deferred<readonly string[]>()
    const second = provider('beta')
    const onDiscoverModels = vi.fn(() => pending.promise)
    renderDialog({ state: state('ready', [provider('alpha'), second]), onDiscoverModels })
    await vi.waitFor(() => expect(screen.getByDisplayValue('30s')).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Add model' }))
    fireEvent.click(screen.getByRole('button', { name: 'Fetch models from provider' }))
    fireEvent.click(screen.getByRole('button', { name: /^beta/ }))
    pending.resolve(['late-secret-model'])
    await vi.waitFor(() => expect(screen.queryByText('late-secret-model')).toBeNull())
  })

  it('renders loading/error/stale/retry authority states and does not expose command error text', async () => {
    const retrySettings = vi.fn()
    const onError = vi.fn()
    const rendered = renderDialog({ state: state('loading', []), onRetrySettings: retrySettings, onError })
    const { rerender } = rendered
    expect(screen.getByText('Reading Server Root settings…')).toBeTruthy()
    rerender(<ProviderManagerDialog {...rendered.props} state={{ ...state('error', []), availability: { status: 'error', error: { code: 'server', message: 'https://user:secret@example.test/api-key' } } } as ProviderSettingsReadModel} onRetrySettings={retrySettings} onError={onError} />)
    expect(screen.getByText('Provider settings are unavailable.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(retrySettings).toHaveBeenCalledTimes(1)
    rerender(<ProviderManagerDialog {...rendered.props} state={state('stale')} onError={onError} />)
    expect(screen.getByText(/showing the last synchronized settings/)).toBeTruthy()

    const failing = vi.fn(async () => { throw new Error('server message api_key=attacker-secret https://attacker.example.test/?token=x') })
    cleanup()
    const wrapper = renderDialog({ onDiscoverModels: failing })
    fireEvent.click(screen.getByRole('button', { name: 'Add model' }))
    fireEvent.click(screen.getByRole('button', { name: 'Fetch models from provider' }))
    await vi.waitFor(() => expect(wrapper.onError).toHaveBeenCalledWith('Provider model discovery failed.'))
    expect(wrapper.onError).not.toHaveBeenCalledWith(expect.stringContaining('attacker-secret'))
    expect(document.body.textContent).not.toContain('attacker-secret')
    expect(document.body.textContent).not.toContain('attacker.example.test')
  })

  it('keeps Codex pending and signed-in presentation safe while switching providers', async () => {
    const alpha = provider('alpha', [model('codex', 'openai-codex')])
    const beta = provider('beta', [model('fast')])
    const rendered = renderDialog({ state: state('ready', [alpha, beta]), codexLogin: null })
    const { rerender } = rendered
    await vi.waitFor(() => expect(screen.getByText('Sign in to Codex')).toBeTruthy())
    expect(screen.getByText('Reading status…')).toBeTruthy()
    rerender(<ProviderManagerDialog {...rendered.props} state={state('ready', [alpha, beta])} codexLogin={login('alpha', 'signed_out')} />)
    await vi.waitFor(() => expect(screen.getByText('Sign in to Codex')).toBeTruthy())
    rerender(<ProviderManagerDialog {...rendered.props} state={state('ready', [alpha, beta])} codexLogin={login('alpha', 'pending')} />)
    expect(screen.getByText('ABCD-EFGH')).toBeTruthy()
    rerender(<ProviderManagerDialog {...rendered.props} state={state('ready', [alpha, beta])} codexLogin={login('alpha', 'signed_in')} />)
    expect(screen.getByText('Sign out')).toBeTruthy()
    expect(screen.queryByText(/account|expiry|expires/i)).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: /^beta/ }))
    expect(screen.queryByText('ABCD-EFGH')).toBeNull()
  })

  it('adds a manually entered model even when provider discovery is unavailable', async () => {
    const onSave = vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSave']>(async () => {})
    renderDialog({ onSave, onDiscoverModels: vi.fn(async () => { throw new Error('unsupported') }) })
    fireEvent.click(screen.getByRole('button', { name: 'Add model' }))
    const dialog = screen.getByRole('dialog', { name: 'Add model' })
    fireEvent.change(within(dialog).getByRole('combobox', { name: 'New model ID' }), { target: { value: 'vendor/private-model' } })
    expect((within(dialog).getByRole('textbox', { name: 'New profile name' }) as HTMLInputElement).value).toBe('vendor/private-model')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add model' }))
    expect(screen.getByRole('button', { name: /vendor\/private-model/ })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(onSave.mock.calls[0]![1].models.at(-1)).toMatchObject({ profile: 'vendor/private-model', id: 'vendor/private-model' })
  })

  it('requires a manual online match choice and imports details without replacing local identity', async () => {
    const local = provider('gateway', [{ ...model('deepseek-v4-flash'), id: 'deepseek-v4-flash' }])
    const onSave = vi.fn<ComponentProps<typeof ProviderManagerDialog>['onSave']>(async () => {})
    const onSearchModelCatalog = vi.fn(async () => [
      { id: 'deepseek-v4-flash', name: 'DeepSeek V4 Flash', provider: 'gateway', context_window: 64000, input: ['text'] as const },
      { id: 'deepseek-v4', name: 'DeepSeek V4', provider: 'deepseek', context_window: 128000, output_limit: 16000, input: ['text', 'image'] as const, reasoning: { enabled: true, effort_levels: ['low', 'high'] } },
    ])
    renderDialog({ state: state('ready', [local]), onSave, onSearchModelCatalog })
    fireEvent.click(screen.getByRole('button', { name: /deepseek-v4-flash/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Find configuration online' }))
    const dialog = screen.getByRole('dialog', { name: 'Find configuration online' })
    await vi.waitFor(() => expect(within(dialog).getAllByRole('radio')).toHaveLength(2))
    expect((within(dialog).getByRole('button', { name: 'Use selected configuration' }) as HTMLButtonElement).disabled).toBe(true)
    const developer = within(dialog).getByText('Likely model developer', { exact: false }).closest('label')!
    fireEvent.click(developer)
    fireEvent.click(within(dialog).getByRole('button', { name: 'Use selected configuration' }))
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(onSave.mock.calls[0]![1].models[0]).toMatchObject({ profile: 'deepseek-v4-flash', id: 'deepseek-v4-flash', context_window: 128000, output_limit: 16000, input: ['text', 'image'], parameters_mode: 'preserve' })
  })
})
