import { describe, expect, it } from 'vitest'
import { ProviderSettingsRepository } from './providerSettings'
import type { ProviderSettingsDomain, ProviderSettingsReadModel } from './providerSettings'

function provider(baseURL: string, requestTimeout = '30s'): ProviderSettingsDomain {
  return {
    name: 'alpha', baseURL, apiKeyConfigured: true, authFile: 'token.json', requestTimeout,
    httpProxy: 'http://proxy.example.test:8080', httpsProxy: '', maxConcurrentRequests: 2,
    models: [
      { profile: 'slow', id: 'slow-id', type: 'openai-chat', compatibility: '', input: ['image', 'text'], developerRole: '', contextWindow: 1000, inputLimit: 900, outputLimit: 100, reasoningConfig: { type: '', parameter: 'effort', default: 'low', levels: [{ name: 'medium', value: 2 }, { name: 'low', value: 'low' }] }, pricing: null },
      { profile: 'fast', id: 'fast-id', type: 'openai-chat', compatibility: '', input: ['text'], developerRole: '', contextWindow: 2000, inputLimit: 1800, outputLimit: 200, reasoningConfig: { type: '', parameter: '', default: '', levels: [] }, pricing: null },
    ],
  }
}

function read(value: ProviderSettingsDomain): ProviderSettingsReadModel {
  return { status: 'ready', serverRoot: '/srv', configPath: '/srv/sai.yaml', defaultProvider: 'alpha', defaultModel: 'fast', providers: [value], availability: { status: 'ready' } }
}

describe('ProviderSettingsRepository authority barrier', () => {
  it('waits for the target provider token, not URL/config canonicalization', async () => {
    let revision = '1'
    let current = read(provider('https://example.test/v1'))
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: () => current,
      getProvider: (name: string) => current.providers.find((item) => item.name === name),
      getModel: (_provider: string, profile: string) => current.providers[0].models.find((item) => item.profile === profile),
      getAuthorityToken: () => ({ epoch: 'epoch-1', generation: 1, revision, providers: { alpha: { epoch: 'epoch-1', generation: 1, revision } } }),
    }
    const repository = new ProviderSettingsRepository(source)
    const previous = repository.captureAuthority()
    const observed = repository.waitForProviderPublication('alpha', previous, { timeoutMS: 100 })
    // The command acknowledgement alone must not settle saving.
    await new Promise((resolve) => setTimeout(resolve, 5))
    let settled = false
    void observed.then(() => { settled = true })
    await new Promise((resolve) => setTimeout(resolve, 5))
    expect(settled).toBe(false)
    revision = '2'
    current = read(provider('https://example.test/v1', '45s'))
    for (const listener of [...listeners]) listener()
    await expect(observed).resolves.toMatchObject({ providers: [{ requestTimeout: '45s' }] })
  })

  it('does not let a no-op barrier be needed, while changed publication advances', async () => {
    let revision = '7'
    let current = read(provider('https://example.test/v1'))
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: () => current,
      getProvider: (name: string) => current.providers.find((item) => item.name === name),
      getModel: (_provider: string, profile: string) => current.providers[0].models.find((item) => item.profile === profile),
      getAuthorityToken: () => ({ epoch: 'epoch-1', generation: 1, revision, providers: { alpha: { epoch: 'epoch-1', generation: 1, revision } } }),
    }
    const repository = new ProviderSettingsRepository(source)
    const previous = repository.captureAuthority()
    const replacement = repository.waitForProviderPublication('alpha', previous, { timeoutMS: 100 })
    revision = '8'
    current = read(provider('https://example.test/v1'))
    for (const listener of [...listeners]) listener()
    await expect(replacement).resolves.toMatchObject({ status: 'ready' })
  })

  it('does not inspect normalized auth paths or endpoints', async () => {
    let revision = '3'
    let current = read(provider('https://example.test/v1'))
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: () => current,
      getProvider: (name: string) => current.providers.find((item) => item.name === name),
      getModel: (_provider: string, profile: string) => current.providers[0].models.find((item) => item.profile === profile),
      getAuthorityToken: () => ({ epoch: 'epoch-1', generation: 1, revision, providers: { alpha: { epoch: 'epoch-1', generation: 1, revision } } }),
    }
    const repository = new ProviderSettingsRepository(source)
    const previous = repository.captureAuthority()
    const observed = repository.waitForProviderPublication('alpha', previous, { timeoutMS: 100 })
    revision = '4'
    current = read({ ...provider('https://example.test/v1'), authFile: 'token.json' })
    for (const listener of [...listeners]) listener()
    await expect(observed).resolves.toMatchObject({ status: 'ready' })
  })

  it('does not let an unrelated provider publication satisfy a hidden replacement barrier', async () => {
    let revision = '5'
    let alphaRevision = '5'
    let current = read(provider('https://example.test/v1'))
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: () => current,
      getProvider: (name: string) => current.providers.find((item) => item.name === name),
      getModel: (_provider: string, profile: string) => current.providers[0].models.find((item) => item.profile === profile),
      getAuthorityToken: () => ({ epoch: 'epoch-1', generation: 1, revision, providers: { alpha: { epoch: 'epoch-1', generation: 1, revision: alphaRevision } } }),
    }
    const repository = new ProviderSettingsRepository(source)
    const previous = repository.captureAuthority()
    const observed = repository.waitForProviderPublication('alpha', previous, { timeoutMS: 100 })
    revision = '6'
    current = read(provider('https://example.test/v1'))
    for (const listener of [...listeners]) listener()
    let settled = false
    void observed.then(() => { settled = true })
    await new Promise((resolve) => setTimeout(resolve, 5))
    expect(settled).toBe(false)
    alphaRevision = '6'
    for (const listener of [...listeners]) listener()
    await expect(observed).resolves.toMatchObject({ status: 'ready' })
  })
})
