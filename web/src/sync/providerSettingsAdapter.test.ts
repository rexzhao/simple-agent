import { describe, expect, it } from 'vitest'
import type { JsonObject } from '../protocol/types'
import { ProviderSettingsRepository as DomainRepository, selectProviderModel as selectDomainProviderModel } from '../repositories/providerSettings'
import { LocalReplica } from './localReplica'
import { ProviderSettingsAdapter } from './providerSettingsAdapter'
import { ProviderSettingsRepository, selectProvider, selectProviderModel, selectProviders } from './providerSettingsRepository'
import { ProviderSettingsStore } from './providerSettingsStore'

const model = (profile: string, id = `${profile}-id`) => ({
  profile, id, type: '', compatibility: '', input: ['text'], developer_role: '', context_window: 32000,
  input_limit: 0, output_limit: 0,
  reasoning_config: { parameter: 'effort', default: 'medium', levels: [{ name: 'low', value: 'low' }, { name: 'medium', value: 'medium' }] as Array<{ name: string; value: string | number | boolean | null }> },
  pricing: null,
})

const provider = (name: string, profiles = ['fast']) => ({
  name, base_url: 'https://example.test/v1', api_key_configured: true, auth_file: '', request_timeout: '', http_proxy: '', https_proxy: '', max_concurrent_requests: 0,
  models: profiles.map((profile) => model(profile)),
})

const snapshot = (providers = [provider('alpha'), provider('beta')]) => ({ server_root: '/srv', config_path: '/srv/sai.yaml', default_provider: 'alpha', default_model: 'fast', providers })
const resource = { type: 'provider_settings' as const, id: 'server' }

describe('ProviderSettingsAdapter', () => {
  it('strictly decodes the safe whitelist and keeps complete entity identity', () => {
    const adapter = new ProviderSettingsAdapter()
    const first = adapter.decodeSnapshot(snapshot([provider('alpha'), provider('space provider')]) as unknown as JsonObject, undefined)
    expect(first.orderedProviderNames).toEqual(['alpha', 'space provider'])
    expect(first.providersByName.alpha.models[0].profile).toBe('fast')
    expect(() => adapter.decodeSnapshot({ ...snapshot(), api_key: 'secret' } as unknown as JsonObject, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ ...snapshot(), providers: [{ ...provider('alpha'), models: [{ ...model('fast'), parameters: { secret: 'x' } }] }] } as unknown as JsonObject, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ ...snapshot(), providers: [provider(' space provider')] } as unknown as JsonObject, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ ...snapshot(), providers: [{ ...provider('alpha'), max_concurrent_requests: 1_000_000_001 }] } as unknown as JsonObject, undefined)).toThrow()
    expect(() => adapter.decodeSnapshot({ ...snapshot(), providers: [{ ...provider('alpha'), models: [{ ...model('fast'), context_window: -1 }] }] } as unknown as JsonObject, undefined)).toThrow()
    const same = adapter.decodeSnapshot(snapshot([provider('alpha'), provider('space provider')]) as unknown as JsonObject, first)
    expect(same).toBe(first)
    expect(same.providersByName.alpha).toBe(first.providersByName.alpha)
    const scalarModel = { ...model('fast'), reasoning_config: { parameter: 'budget_tokens', default: 'max_tokens', levels: [{ name: 'budget_tokens', value: 1234 }, { name: 'max_tokens', value: true }, { name: 'unlimited', value: null }] as Array<{ name: string; value: string | number | boolean | null }> } }
    const scalarSnapshot = snapshot([{ ...provider('alpha'), models: [scalarModel] }])
    const scalar = adapter.decodeSnapshot(scalarSnapshot as unknown as JsonObject, undefined)
    expect(scalar.providersByName.alpha.models[0].reasoning_config.levels.map((level) => level.value)).toEqual([1234, true, null])
    expect(() => adapter.decodeSnapshot({ ...scalarSnapshot, providers: [{ ...provider('alpha'), models: [{ ...model('fast'), reasoning_config: { parameter: 'effort', default: 'low', levels: [{ name: 'bad', value: { secret: true } }] } }] }] } as unknown as JsonObject, undefined)).toThrow()
  })

  it('applies upsert/remove/default operations as one typed collection update', () => {
    const adapter = new ProviderSettingsAdapter()
    const first = adapter.decodeSnapshot(snapshot([provider('alpha'), provider('beta')]) as unknown as JsonObject, undefined)
    const changed = adapter.applyChange(first, [{ op: 'upsert', key: 'beta', value: provider('beta', ['slow']) as unknown as JsonObject }, { op: 'remove', key: 'alpha' }, { op: 'default.replace', key: 'server', value: { provider: 'beta', model: 'slow' } }])
    expect(changed.orderedProviderNames).toEqual(['beta'])
    expect(changed.providersByName.beta.models[0].profile).toBe('slow')
    expect(changed.default_provider).toBe('beta')
    expect(changed.default_model).toBe('slow')
    expect(() => adapter.applyChange(first, [{ op: 'remove', key: 'alpha', value: null }])).toThrow()
    expect(() => adapter.applyChange(first, [{ op: 'default.replace', key: 'wrong', value: { provider: '', model: '' } }])).toThrow()
  })

  it('keeps an authority publication observable when the safe projection is unchanged', () => {
    const adapter = new ProviderSettingsAdapter()
    const first = adapter.decodeSnapshot(snapshot([provider('alpha')]) as unknown as JsonObject, undefined, { resourceRevision: '10' })
    const published = adapter.applyChange(first, [{ op: 'upsert', key: 'alpha', value: provider('alpha') as unknown as JsonObject }], { resourceRevision: '11' })
    expect(published).not.toBe(first)
    expect(published.authorityRevision.revision).toBe('11')
    expect(published.providersByName.alpha).toBe(first.providersByName.alpha)
  })

  it('tracks the affected provider publication separately from unrelated providers', () => {
    const adapter = new ProviderSettingsAdapter()
    const first = adapter.decodeSnapshot(snapshot([provider('alpha'), provider('beta')]) as unknown as JsonObject, undefined, { resourceRevision: '10' })
    const published = adapter.applyChange(first, [{ op: 'upsert', key: 'beta', value: provider('beta') as unknown as JsonObject }], { resourceRevision: '11' })
    expect(published.providerAuthorityRevisions.alpha.revision).toBe('10')
    expect(published.providerAuthorityRevisions.beta.revision).toBe('11')
    expect(published.providerAuthorityRevisions.alpha.epoch).toBe('')
  })

  it('does not reuse an authority token across stream epochs', () => {
    const adapter = new ProviderSettingsAdapter()
    const first = adapter.decodeSnapshot(snapshot([provider('alpha')]) as unknown as JsonObject, undefined, { streamEpoch: 'epoch-1', resourceRevision: '4', generation: 1 })
    const next = adapter.decodeSnapshot(snapshot([provider('alpha')]) as unknown as JsonObject, first, { streamEpoch: 'epoch-2', resourceRevision: '4', generation: 1 })
    expect(next.authorityRevision).not.toEqual(first.authorityRevision)
    expect(next.authorityRevision.revision).toBe(first.authorityRevision.revision)
    expect(next.authorityRevision.epoch).not.toBe(first.authorityRevision.epoch)
  })

  it('keeps saving behind a real LocalReplica publication and accepts publication-before-ack', async () => {
    const replica = new LocalReplica()
    const adapter = new ProviderSettingsAdapter()
    const store = new ProviderSettingsStore(replica)
    const domain = new DomainRepository(store)
    replica.applySnapshot(resource, adapter, snapshot([provider('alpha')]) as unknown as JsonObject, { streamEpoch: 'epoch-1', sequence: '0' as never, resourceRevision: '0', generation: 1 })
    const previous = domain.captureAuthority()
    let settled = false
    const barrier = domain.waitForProviderPublication('alpha', previous, { timeoutMS: 100 }).then(() => { settled = true })
    await Promise.resolve()
    expect(settled).toBe(false)
    replica.applySnapshotAndChanges(resource, adapter, snapshot([provider('alpha')]) as unknown as JsonObject, { streamEpoch: 'epoch-1', sequence: '1' as never, resourceRevision: '1', generation: 1 }, [{ operations: [{ op: 'upsert', key: 'alpha', value: provider('alpha') as unknown as JsonObject }], metadata: { streamEpoch: 'epoch-1', sequence: '2' as never, resourceRevision: '2', generation: 1 } }])
    await expect(barrier).resolves.toBeUndefined()
    // If publication wins the race with the command acknowledgement, capture
    // the pre-command token and start observing only after the publication.
    const beforeAck = domain.captureAuthority()
    replica.applySnapshotAndChanges(resource, adapter, snapshot([provider('alpha')]) as unknown as JsonObject, { streamEpoch: 'epoch-1', sequence: '3' as never, resourceRevision: '3', generation: 1 }, [{ operations: [{ op: 'upsert', key: 'alpha', value: provider('alpha') as unknown as JsonObject }], metadata: { streamEpoch: 'epoch-1', sequence: '4' as never, resourceRevision: '4', generation: 1 } }])
    await expect(domain.waitForProviderPublication('alpha', beforeAck, { timeoutMS: 100 })).resolves.toMatchObject({ status: 'ready' })
    const oldEpoch = domain.captureAuthority()
    replica.applySnapshot(resource, adapter, snapshot([provider('alpha')]) as unknown as JsonObject, { streamEpoch: 'epoch-2', sequence: '0' as never, resourceRevision: '4', generation: 1 })
    await expect(domain.waitForProviderPublication('alpha', oldEpoch, { timeoutMS: 100 })).resolves.toMatchObject({ status: 'ready' })
    store.dispose()
  })

  it('publishes LocalReplica barriers atomically and exposes availability/selectors only through the repository', () => {
    const replica = new LocalReplica()
    const adapter = new ProviderSettingsAdapter()
    const store = new ProviderSettingsStore(replica)
    const repository = new ProviderSettingsRepository(replica)
    const domainRepository = new DomainRepository(repository)
    let notifications = 0
    replica.subscribe((changed) => { if (changed.type === 'provider_settings') notifications += 1 })
    expect(store.getSnapshot().availability.status).toBe('loading')
    replica.applySnapshot(resource, adapter, snapshot() as unknown as JsonObject, { streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1 })
    const before = repository.getSnapshot()
    expect(before.availability.status).toBe('ready')
    expect(selectProviders(repository)).toHaveLength(2)
    expect(selectProvider(repository, 'alpha')?.name).toBe('alpha')
    expect(selectProviderModel(repository, 'alpha', 'fast')?.id).toBe('fast-id')
    expect(selectDomainProviderModel(domainRepository, 'alpha', 'fast')?.developerRole).toBe('')
    expect(() => replica.applySnapshotAndChanges(resource, adapter, snapshot() as unknown as JsonObject, { streamEpoch: 'epoch', sequence: '1' as never, resourceRevision: '1', generation: 1 }, [{ operations: [{ op: 'remove', key: 'alpha', value: null }], metadata: { streamEpoch: 'epoch', sequence: '2' as never, resourceRevision: '2', generation: 1 } }])).toThrow()
    expect(repository.getSnapshot()).toBe(before)
    expect(notifications).toBe(1)
    replica.markStale(resource, undefined, 2)
    expect(repository.getSnapshot().availability.status).toBe('stale')
    store.dispose()
    repository.dispose()
  })
})
