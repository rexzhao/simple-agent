import type { CodexLoginCommands } from './commands/codexLoginCommands'
import type { ProjectCommands } from './commands/projectCommands'
import type { ProviderCommands } from './commands/providerCommands'
import type { RunCommands } from './commands/runCommands'
import type { SessionCommands } from './commands/sessionCommands'
import type { CodexLoginRepository } from './repositories/codexLogin'
import type { ProjectIndexRepository } from './repositories/projectIndex'
import type { ProviderSettingsRepository } from './repositories/providerSettings'
import type { SessionContentRepository } from './repositories/sessionContent'
import type { SessionIndexRepository } from './repositories/sessionIndex'
import type { SessionImageData } from './repositories/sessionContent'
import type { SessionModelOption } from './types'
import type { CodexUsageDomain } from './domain/codexUsage'
import type { RunImageReference } from './commands/runCommands'

/**
 * Pure application state used to express interest without exposing routing or
 * protocol details to React. The implementation may live in infrastructure;
 * this contract deliberately does not.
 */
export interface ApplicationSignal<T> {
  get(): T
  subscribe(listener: () => void): () => void
}

export interface MutableApplicationSignal<T> extends ApplicationSignal<T> {
  set(next: T): void
}

export type CurrentProjectSignal = ApplicationSignal<string | null>
export type MutableCurrentProjectSignal = MutableApplicationSignal<string | null>
export type CurrentSessionSignal = ApplicationSignal<string | null>
export type MutableCurrentSessionSignal = MutableApplicationSignal<string | null>
export type CurrentCodexLoginProviderSignal = ApplicationSignal<string | null>
export type MutableCurrentCodexLoginProviderSignal = MutableApplicationSignal<string | null>

export interface ProviderSettingsApplicationState {
  readonly settingsEnabled: boolean
  readonly modelSelectionNeeded: boolean
}

export type ProviderSettingsApplicationStateSignal = ApplicationSignal<ProviderSettingsApplicationState>
export type MutableProviderSettingsApplicationStateSignal = MutableApplicationSignal<ProviderSettingsApplicationState>

/** The only command surface that a future page should receive. */
export interface ApplicationCommands {
  readonly project: ProjectCommands
  readonly session: SessionCommands
  readonly run: RunCommands
  readonly provider: ProviderCommands
  readonly codexLogin: CodexLoginCommands
}

/** Domain repositories contain no transport, protocol, cursor, or Blob API. */
export interface ApplicationRepositories {
  readonly projectIndex: ProjectIndexRepository
  readonly sessionIndex: SessionIndexRepository
  readonly sessionContent: SessionContentRepository
  readonly providerSettings: ProviderSettingsRepository
  readonly codexLogin: CodexLoginRepository
}

export interface ApplicationSignals {
  readonly currentProject: MutableCurrentProjectSignal
  readonly currentSession: MutableCurrentSessionSignal
  readonly providerSettings: MutableProviderSettingsApplicationStateSignal
  readonly codexLoginProvider: MutableCurrentCodexLoginProviderSignal
}

/** Small application read facades for the remaining non-sync startup/data
 * plane reads. Their wire DTOs are normalized before crossing this boundary. */
export interface ApplicationBootstrap {
  readonly version: string
  readonly cwd: string
  readonly serverRoot: string
  readonly configPath: string
}

export interface SessionModelCatalog {
  readonly models: readonly SessionModelOption[]
  readonly defaultProvider: string
  readonly defaultModel: string
}

/**
 * Pure page contract. Infrastructure objects such as runtime, stores, replica,
 * transport, and Blob client are intentionally absent.
 */
export interface ApplicationPageServices {
  readonly repositories: ApplicationRepositories
  readonly commands: ApplicationCommands
  readonly signals: ApplicationSignals
  readonly loadBootstrap: () => Promise<ApplicationBootstrap>
  readonly sessionModels: (projectID: string) => Promise<SessionModelCatalog>
  readonly codexUsage: (provider: string) => Promise<CodexUsageDomain>
  readonly loadSessionImage: (sessionID: string, hash: string, signal?: AbortSignal) => Promise<SessionImageData>
  /** Bounded binary attachment data plane. */
  readonly uploadSessionImage: (sessionID: string, image: Blob, signal?: AbortSignal) => Promise<RunImageReference>
}

/** Lifecycle bridge consumed by the React Provider, also without sync types. */
export interface ApplicationLifecycle {
  readonly page: ApplicationPageServices
  start(): void
  stop(): void
}
