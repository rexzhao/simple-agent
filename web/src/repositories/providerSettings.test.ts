import { describe, expect, it } from 'vitest'
import { ProviderSettingsObservationError, ProviderSettingsRepository } from './providerSettings'
import type { ProviderSettingsReadModel } from './providerSettings'

function read(status: ProviderSettingsReadModel['status'], overrides: Partial<ProviderSettingsReadModel> = {}): ProviderSettingsReadModel {
  return {
    status,
    serverRoot: '/srv',
    configPath: '/srv/sai.yaml',
    defaultProvider: '',
    defaultModel: '',
    providers: [],
    availability: { status },
    ...overrides,
  }
}

describe('ProviderSettingsRepository', () => {
  it('exposes loading/error authority safely and delegates retry without transport details', () => {
    let current = read('loading')
    let retries = 0
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: () => current,
      getProvider: () => undefined,
      getModel: () => undefined,
      retry: () => { retries += 1 },
    }
    const repository = new ProviderSettingsRepository(source)
    expect(repository.getSnapshot().status).toBe('loading')
    current = read('error', { error: { code: 'wire_payload', message: 'https://user:secret@example.test/api-key' }, availability: { status: 'error', error: { code: 'wire_payload', message: 'leak' } } })
    expect(repository.getSnapshot().error).toEqual({ code: 'unavailable', message: 'Provider settings are temporarily unavailable' })
    expect(repository.getSnapshot().availability.error).toEqual({ code: 'unavailable', message: 'Provider settings are temporarily unavailable' })
    repository.retry()
    expect(retries).toBe(1)
    expect(listeners.size).toBe(0)
  })

  it('waits for a newly published ready model, supports cancellation, and bounds missing authority', async () => {
    let current = read('loading')
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: () => current,
      getProvider: () => undefined,
      getModel: () => undefined,
    }
    const repository = new ProviderSettingsRepository(source)
    const observed = repository.waitFor((model) => model.status === 'ready' && model.defaultProvider === 'alpha', { timeoutMS: 100 })
    current = read('ready', { defaultProvider: 'alpha', defaultModel: 'fast', providers: [] })
    for (const listener of [...listeners]) listener()
    await expect(observed).resolves.toMatchObject({ defaultProvider: 'alpha' })

    const controller = new AbortController()
    controller.abort()
    await expect(repository.waitFor(() => false, { signal: controller.signal, timeoutMS: 100 })).rejects.toMatchObject({ code: 'cancelled' } satisfies Partial<ProviderSettingsObservationError>)
    await expect(repository.waitFor(() => false, { timeoutMS: 1 })).rejects.toMatchObject({ code: 'timeout' } satisfies Partial<ProviderSettingsObservationError>)
  })
})
