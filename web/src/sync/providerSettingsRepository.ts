import { ProviderSettingsStore } from './providerSettingsStore'
export { ProviderSettingsStore } from './providerSettingsStore'
export type {
  DomainReadError,
  ProviderModelDomain,
  ProviderPricingDomain,
  ProviderPricingTierDomain,
  ProviderSettingsAvailability,
  ProviderSettingsDomain,
  ProviderSettingsReadModel,
  ProviderSettingsReadState,
  ProviderSettingsSource,
} from '../repositories/providerSettings'

// Naming-compatible sync entry point. The Store remains the single local
// projection; this alias does not create a second cache or reducer.
export class ProviderSettingsRepository extends ProviderSettingsStore {}

export function selectProviders(repository: ProviderSettingsRepository): readonly ReturnType<ProviderSettingsRepository['getSnapshot']>['providers'][number][] { return repository.getSnapshot().providers }
export function selectProvider(repository: ProviderSettingsRepository, name: string) { return repository.getProvider(name) }
export function selectProviderModel(repository: ProviderSettingsRepository, providerName: string, profile: string) { return repository.getModel(providerName, profile) }
