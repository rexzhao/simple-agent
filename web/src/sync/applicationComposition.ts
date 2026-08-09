import type {
  ApplicationCommands,
  ApplicationPageServices,
  ApplicationRepositories,
  ApplicationSignals,
  ApplicationLifecycle,
  ProviderSettingsApplicationState,
} from '../applicationServices'
import { CodexLoginRepository as DomainCodexLoginRepository } from '../repositories/codexLogin'
import { ProjectIndexRepository as DomainProjectIndexRepository } from '../repositories/projectIndex'
import { ProviderSettingsRepository as DomainProviderSettingsRepository } from '../repositories/providerSettings'
import { SessionContentRepository as DomainSessionContentRepository } from '../repositories/sessionContent'
import { SessionIndexRepository as DomainSessionIndexRepository } from '../repositories/sessionIndex'
import { BlobClient, type BlobClientOptions } from './blobClient'
import { CodexLoginInterestPolicy, createCurrentCodexLoginProviderSignal, createCurrentProjectSignal, createCurrentSessionSignal, createProviderSettingsApplicationStateSignal, ProjectIndexInterestPolicy, ProviderSettingsInterestPolicy, SessionContentInterestPolicy, SessionIndexInterestPolicy } from './interestPolicy'
import { CommandFacade } from './commandFacade'
import { LocalReplica } from './localReplica'
import { ProjectIndexStore } from './projectIndexStore'
import { SessionIndexRepository as SyncSessionIndexRepository } from './sessionIndexRepository'
import { SessionContentRepository as SyncSessionContentRepository } from './sessionContentRepository'
import { ProviderSettingsStore } from './providerSettingsStore'
import { CodexLoginStore } from './codexLoginStore'
import { SyncRuntime, type RuntimeTransport } from './runtime'
import { WebSocketTransport, type WebSocketTransportOptions } from './transport'
import { WebDebugBridge, type WebDebugBridgeOptions } from '../debug/webDebugBridge'
import { sessionHistoryResultToDomain } from './sessionContentHistory'
import type { SessionContentHistoryReadOptions, SessionContentHistoryWindow } from '../domain/sessionContent'
import { api } from '../api'
import { codexUsageDomain } from '../domain/codexUsage'

// Keep the old infrastructure-facing names available while the page contract
// itself remains in the pure application layer.
export type SyncApplicationCommands = ApplicationCommands
export type SyncApplicationSignals = ApplicationSignals

export interface SyncApplicationStores {
  readonly projectIndex: ProjectIndexStore
  readonly sessionIndex: SyncSessionIndexRepository
  readonly sessionContent: SyncSessionContentRepository
  readonly providerSettings: ProviderSettingsStore
  readonly codexLogin: CodexLoginStore
}

export type SyncApplicationRepositories = ApplicationRepositories

export interface SyncApplicationPolicies {
  readonly projectIndex: ProjectIndexInterestPolicy
  readonly sessionIndex: SessionIndexInterestPolicy
  readonly sessionContent: SessionContentInterestPolicy
  readonly providerSettings: ProviderSettingsInterestPolicy
  readonly codexLogin: CodexLoginInterestPolicy
}

export type SyncApplicationPageServices = ApplicationPageServices

/**
 * The only application-level object graph for the client sync system. The
 * public `repositories`, `commands`, and `signals` members are safe for a
 * future page to consume; the transport/runtime/store members are exposed so
 * the composition boundary can be tested and embedded without a real server.
 */
export interface SyncApplication extends ApplicationLifecycle {
  readonly transport: RuntimeTransport
  readonly blobClient: BlobClient
  readonly replica: LocalReplica
  readonly runtime: SyncRuntime
  readonly commandFacade: CommandFacade
  readonly debugBridge: WebDebugBridge
  readonly commands: ApplicationCommands
  readonly stores: SyncApplicationStores
  readonly repositories: ApplicationRepositories
  readonly signals: ApplicationSignals
  readonly policies: SyncApplicationPolicies
  readonly page: ApplicationPageServices
  readonly started: boolean
  start(): void
  stop(): void
  dispose(): void
}

export interface SyncApplicationOptions {
  /** Inject a fake transport for composition tests or an alternate host. */
  readonly transport?: RuntimeTransport
  /** Inject a fake HTTP Blob data-plane client for composition tests. */
  readonly blobClient?: BlobClient
  /** Used only when `transport` is not supplied; fetcher is the ticket seam. */
  readonly transportOptions?: WebSocketTransportOptions
  /** Used only when `blobClient` is not supplied. */
  readonly blobClientOptions?: BlobClientOptions
  readonly initialProjectID?: string | null
  readonly initialSessionID?: string | null
  readonly initialProviderSettings?: Partial<ProviderSettingsApplicationState>
  readonly initialCodexLoginProvider?: string | null
  /** Test seam for the application history reader; production uses the typed
   * command/blob data plane below. */
  readonly historyReader?: (sessionID: string, options: SessionContentHistoryReadOptions, signal?: AbortSignal) => Promise<SessionContentHistoryWindow>
  readonly debugBridgeOptions?: Pick<WebDebugBridgeOptions, 'pageIDGenerator' | 'pageEpochGenerator' | 'window' | 'document' | 'pollIntervalMS' | 'setTimeout' | 'clearTimeout'>
}

function pageCommands(facade: CommandFacade): ApplicationCommands {
  return {
    project: facade,
    session: facade,
    run: facade,
    provider: facade,
    codexLogin: facade,
  }
}

export function createSyncApplication(options: SyncApplicationOptions = {}): SyncApplication {
  const transport = options.transport ?? new WebSocketTransport(options.transportOptions)
  const blobClient = options.blobClient ?? new BlobClient(options.blobClientOptions)
  const replica = new LocalReplica()
  const runtime = new SyncRuntime({ transport, replica, blobClient })
  const commandFacade = new CommandFacade({ transport, blobClient })

  const stores: SyncApplicationStores = {
    projectIndex: new ProjectIndexStore(replica, () => runtime.retry({ type: 'project_index', id: 'server' })),
    sessionIndex: new SyncSessionIndexRepository(replica, { retry: (projectID) => runtime.retry({ type: 'session_index', id: projectID }) }),
    sessionContent: new SyncSessionContentRepository(replica, {
      historyReader: options.historyReader ?? (async (sessionID, historyOptions, signal) => {
          const result = await commandFacade.historyRead(sessionID, historyOptions, { signal })
          return sessionHistoryResultToDomain(result, historyOptions)
        }),
      retry: (sessionID) => runtime.retry({ type: 'session_content', id: sessionID }),
    }),
    providerSettings: new ProviderSettingsStore(replica, () => runtime.retry({ type: 'provider_settings', id: 'server' })),
    codexLogin: new CodexLoginStore(replica, (provider) => runtime.retry({ type: 'codex_login', id: provider })),
  }
  const repositories: SyncApplicationRepositories = {
    projectIndex: new DomainProjectIndexRepository(stores.projectIndex),
    sessionIndex: new DomainSessionIndexRepository(stores.sessionIndex),
    sessionContent: new DomainSessionContentRepository(stores.sessionContent),
    providerSettings: new DomainProviderSettingsRepository(stores.providerSettings),
    codexLogin: new DomainCodexLoginRepository(stores.codexLogin),
  }

  const signals: SyncApplicationSignals = {
    currentProject: createCurrentProjectSignal(options.initialProjectID ?? null),
    currentSession: createCurrentSessionSignal(options.initialSessionID ?? null),
    providerSettings: createProviderSettingsApplicationStateSignal(options.initialProviderSettings),
    codexLoginProvider: createCurrentCodexLoginProviderSignal(options.initialCodexLoginProvider ?? null),
  }
  const policies: SyncApplicationPolicies = {
    projectIndex: new ProjectIndexInterestPolicy(runtime),
    // Session Index is navigation state.  Keep every active project's index
    // desired, rather than tying background completion visibility to the
    // selected project signal.
    sessionIndex: new SessionIndexInterestPolicy(runtime, stores.projectIndex),
    sessionContent: new SessionContentInterestPolicy(runtime, signals.currentSession),
    providerSettings: new ProviderSettingsInterestPolicy(runtime, signals.providerSettings),
    codexLogin: new CodexLoginInterestPolicy(runtime, signals.codexLoginProvider),
  }
  const commands = pageCommands(commandFacade)
  const page: SyncApplicationPageServices = {
    repositories,
    commands,
    signals,
    loadBootstrap: async () => {
      const value = await api.bootstrap()
      return { version: value.version, cwd: value.cwd, serverRoot: value.server_root, configPath: value.config_path }
    },
    sessionModels: async (projectID) => {
      const value = await commandFacade.readModels(projectID)
      return { models: value.models, defaultProvider: value.default_provider, defaultModel: value.default_model }
    },
    codexUsage: async (provider) => codexUsageDomain((await commandFacade.readCodexUsage(provider)).usage),
    loadSessionImage: async (sessionID, hash, signal) => {
      const image = await api.sessionImage(sessionID, hash, signal)
      return { bytes: await image.arrayBuffer(), contentType: image.type }
    },
  }

  const debugBridge = new WebDebugBridge({
    ...options.debugBridgeOptions,
    transport,
    runtime,
    replica,
    repositories,
    commandFacade,
    signals,
  })

  let lifecycle: 'stopped' | 'started' | 'disposed' = 'stopped'

  const stop = (): void => {
    if (lifecycle === 'disposed') return
    if (lifecycle === 'stopped') return
    lifecycle = 'stopped'

    // The debug bridge unregisters while the transport is still alive, then
    // the normal sync graph releases subscriptions and closes the socket.
    debugBridge.stop()
    policies.codexLogin.stop()
    policies.providerSettings.stop()
    policies.sessionContent.stop()
    policies.sessionIndex.stop()
    policies.projectIndex.stop()
    commandFacade.stop()
    runtime.stop()
  }

  const start = (): void => {
    if (lifecycle === 'disposed') throw new Error('sync application has been disposed')
    if (lifecycle === 'started') return
    try {
      // Desired resource references are installed before the runtime starts;
      // the runtime then sends them exactly once on the first ready socket.
      commandFacade.start()
      policies.projectIndex.start()
      policies.sessionIndex.start()
      policies.sessionContent.start()
      policies.providerSettings.start()
      policies.codexLogin.start()
      debugBridge.start()
      runtime.start()
      lifecycle = 'started'
    } catch (reason) {
      // A partially started graph must be as safe to retry as a normal stop.
      policies.codexLogin.stop()
      policies.providerSettings.stop()
      policies.sessionContent.stop()
      policies.sessionIndex.stop()
      policies.projectIndex.stop()
      debugBridge.stop()
      commandFacade.stop()
      runtime.stop()
      throw reason
    }
  }

  const dispose = (): void => {
    if (lifecycle === 'disposed') return
    stop()
    stores.codexLogin.dispose()
    stores.providerSettings.dispose()
    stores.sessionContent.dispose()
    stores.sessionIndex.dispose()
    stores.projectIndex.dispose()
    debugBridge.dispose()
    replica.dispose()
    lifecycle = 'disposed'
  }

  const application: SyncApplication = {
    transport,
    blobClient,
    replica,
    runtime,
    commandFacade,
    debugBridge,
    commands,
    stores,
    repositories,
    signals,
    policies,
    page,
    get started() { return lifecycle === 'started' },
    start,
    stop,
    dispose,
  }
  return application
}
