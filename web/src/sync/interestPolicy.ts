import type { ResourceKey } from '../protocol/types'
import { isProviderName } from '../domain/providerIdentity'
import type {
  CurrentCodexLoginProviderSignal,
  CurrentProjectSignal,
  CurrentSessionSignal,
  MutableCurrentCodexLoginProviderSignal,
  MutableCurrentProjectSignal,
  MutableCurrentSessionSignal,
  MutableProviderSettingsApplicationStateSignal,
  ProviderSettingsApplicationState,
  ProviderSettingsApplicationStateSignal,
} from '../applicationServices'
export type {
  CurrentCodexLoginProviderSignal,
  CurrentProjectSignal,
  CurrentSessionSignal,
  MutableCurrentCodexLoginProviderSignal,
  MutableCurrentProjectSignal,
  MutableCurrentSessionSignal,
  MutableProviderSettingsApplicationStateSignal,
  ProviderSettingsApplicationState,
  ProviderSettingsApplicationStateSignal,
} from '../applicationServices'
import { SyncSubscriptionError, type SyncRuntime } from './runtime'

export interface ProjectIndexInterestPolicyOptions { retainReleased?: boolean }

/**
 * The project index is application navigation state, not page state. It is
 * therefore the one permanent resource interest owned by the composition
 * root rather than by a selected-project signal.
 */
export class ProjectIndexInterestPolicy {
  private readonly runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }
  private readonly retainReleased: boolean
  private started = false
  private releaseCurrent: (() => void) | null = null

  constructor(runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }, options: ProjectIndexInterestPolicyOptions = {}) {
    this.runtime = runtime
    this.retainReleased = options.retainReleased ?? true
  }

  start(): void {
    if (this.started) return
    this.started = true
    try {
      this.releaseCurrent = this.runtime.subscribe(
        { type: 'project_index', id: 'server' },
        { retainOnRelease: this.retainReleased },
      )
    } catch (reason) {
      this.started = false
      throw reason
    }
  }

  stop(): void {
    if (!this.started && !this.releaseCurrent) return
    this.started = false
    this.releaseCurrent?.()
    if (!this.retainReleased) this.runtime.evict?.({ type: 'project_index', id: 'server' })
    this.releaseCurrent = null
  }
}

export interface SessionIndexInterestPolicyOptions {
  /** Keep a removed project replica; runtime bounds retained values. */
  retainReleased?: boolean
}

/**
 * The project index is the navigation scope for Session Index interest.  It
 * deliberately is not the current-project signal: a run in a background
 * project must keep publishing enough state for the navigation tree to show
 * it.
 */
export interface SessionIndexProjectSource {
  getActiveProjectIDs(): readonly string[]
  subscribe(listener: () => void): () => void
}

export function createCurrentProjectSignal(initialProjectID: string | null = null): MutableCurrentProjectSignal {
  let current = initialProjectID
  const listeners = new Set<() => void>()
  return {
    get: () => current,
    set: (projectID) => {
      if (projectID === current) return
      current = projectID
      for (const listener of [...listeners]) listener()
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}

export function createProviderSettingsApplicationStateSignal(initial: Partial<ProviderSettingsApplicationState> = {}): MutableProviderSettingsApplicationStateSignal {
  let state: ProviderSettingsApplicationState = { settingsEnabled: initial.settingsEnabled ?? false, modelSelectionNeeded: initial.modelSelectionNeeded ?? false }
  const listeners = new Set<() => void>()
  return {
    get: () => state,
    set: (next) => {
      const normalized = { settingsEnabled: next.settingsEnabled === true, modelSelectionNeeded: next.modelSelectionNeeded === true }
      if (normalized.settingsEnabled === state.settingsEnabled && normalized.modelSelectionNeeded === state.modelSelectionNeeded) return
      state = normalized
      for (const listener of [...listeners]) listener()
    },
    subscribe: (listener) => { listeners.add(listener); return () => listeners.delete(listener) },
  }
}

export interface ProviderSettingsInterestPolicyOptions { retainReleased?: boolean }

/** Owns the singleton provider-settings desired resource independently from
 * page lifetime. React must update the application-state signal; it never
 * calls runtime.subscribe for this resource directly. */
export class ProviderSettingsInterestPolicy {
  private readonly runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }
  private readonly signal: ProviderSettingsApplicationStateSignal
  private readonly retainReleased: boolean
  private started = false
  private detachSignal: (() => void) | null = null
  private releaseCurrent: (() => void) | null = null
  private desired = false

  constructor(runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }, signal: ProviderSettingsApplicationStateSignal, options: ProviderSettingsInterestPolicyOptions = {}) {
    this.runtime = runtime
    this.signal = signal
    this.retainReleased = options.retainReleased ?? true
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detachSignal = this.signal.subscribe(() => this.reconcile())
    this.reconcile()
  }

  stop(): void {
    if (!this.started && !this.releaseCurrent && !this.detachSignal) return
    this.started = false
    this.detachSignal?.()
    this.detachSignal = null
    this.releaseCurrent?.()
    if (!this.retainReleased && this.desired) this.runtime.evict?.({ type: 'provider_settings', id: 'server' })
    this.releaseCurrent = null
    this.desired = false
  }

  get isDesired(): boolean { return this.desired }

  private reconcile(): void {
    if (!this.started) return
    const state = this.signal.get()
    const next = state.settingsEnabled || state.modelSelectionNeeded
    if (next === this.desired) return
    if (!next) {
      this.releaseCurrent?.()
      if (!this.retainReleased) this.runtime.evict?.({ type: 'provider_settings', id: 'server' })
      this.releaseCurrent = null
      this.desired = false
      return
    }
    try {
      this.releaseCurrent = this.runtime.subscribe({ type: 'provider_settings', id: 'server' }, { retainOnRelease: this.retainReleased })
      this.desired = true
    } catch (reason) {
      this.desired = false
      throw reason
    }
  }
}

export function createCurrentCodexLoginProviderSignal(initialProvider: string | null = null): MutableCurrentCodexLoginProviderSignal {
  let current = initialProvider
  const listeners = new Set<() => void>()
  return {
    get: () => current,
    set: (provider) => {
      if (provider === current) return
      current = provider
      for (const listener of [...listeners]) listener()
    },
    subscribe: (listener) => { listeners.add(listener); return () => listeners.delete(listener) },
  }
}

export interface CodexLoginInterestPolicyOptions { retainReleased?: boolean }

/** Owns the selected Codex login resource independently of page lifetime.
 * Device capabilities remain in the replica only while application interest
 * exists (unless the caller explicitly requests bounded retention). */
export class CodexLoginInterestPolicy {
  private readonly runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }
  private readonly signal: CurrentCodexLoginProviderSignal
  private readonly retainReleased: boolean
  private started = false
  private detachSignal: (() => void) | null = null
  private releaseCurrent: (() => void) | null = null
  private currentProvider: string | null = null

  constructor(runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }, signal: CurrentCodexLoginProviderSignal, options: CodexLoginInterestPolicyOptions = {}) {
    this.runtime = runtime
    this.signal = signal
    this.retainReleased = options.retainReleased ?? false
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detachSignal = this.signal.subscribe(() => this.reconcile())
    this.reconcile()
  }

  stop(): void {
    if (!this.started && !this.releaseCurrent && !this.detachSignal) return
    this.started = false
    this.detachSignal?.()
    this.detachSignal = null
    const old = this.currentProvider
    this.releaseCurrent?.()
    if (old && !this.retainReleased) this.runtime.evict?.({ type: 'codex_login', id: old })
    this.releaseCurrent = null
    this.currentProvider = null
  }

  get provider(): string | null { return this.currentProvider }

  private reconcile(): void {
    if (!this.started) return
    const next = this.signal.get() || null
    if (next !== null && !isProviderName(next)) throw new SyncSubscriptionError('invalid_resource', 'Codex login provider is invalid')
    if (next === this.currentProvider) return
    const old = this.currentProvider
    this.releaseCurrent?.()
    if (old && !this.retainReleased) this.runtime.evict?.({ type: 'codex_login', id: old })
    this.releaseCurrent = null
    if (!next) {
      this.currentProvider = null
      return
    }
    try {
      this.releaseCurrent = this.runtime.subscribe({ type: 'codex_login', id: next }, { retainOnRelease: this.retainReleased })
      this.currentProvider = next
    } catch (reason) {
      this.currentProvider = null
      throw reason
    }
  }
}

/**
 * Keeps the current project's Session Index desired subscription independent
 * of component lifetime. Switching projects releases the old desired resource
 * and retains exactly one reference to the new one.
 */
export class SessionIndexInterestPolicy {
  private readonly runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }
  private readonly projectSource: SessionIndexProjectSource | CurrentProjectSignal
  private readonly options: Required<SessionIndexInterestPolicyOptions>
  private started = false
  private detachSignal: (() => void) | null = null
  private readonly releases = new Map<string, () => void>()
  private currentProjectID: string | null = null

  constructor(runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }, projectSource: SessionIndexProjectSource | CurrentProjectSignal, options: SessionIndexInterestPolicyOptions = {}) {
    this.runtime = runtime
    this.projectSource = projectSource
    this.options = { retainReleased: options.retainReleased ?? false }
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detachSignal = this.projectSource.subscribe(() => this.reconcile())
    this.reconcile()
  }

  stop(): void {
    if (!this.started && this.releases.size === 0 && !this.detachSignal) return
    this.started = false
    this.detachSignal?.()
    this.detachSignal = null
    for (const [projectID, release] of this.releases) {
      release()
      if (!this.options.retainReleased) this.runtime.evict?.({ type: 'session_index', id: projectID })
    }
    this.releases.clear()
    this.currentProjectID = null
  }

  get projectID(): string | null {
    return this.currentProjectID
  }

  private reconcile(): void {
    if (!this.started) return
    const source = this.projectSource as SessionIndexProjectSource
    const legacySignal = !('getActiveProjectIDs' in this.projectSource)
    const signaled = legacySignal ? (this.projectSource as CurrentProjectSignal).get() : null
    const ids = legacySignal
      ? (signaled ? [signaled] : [])
      : [...new Set(source.getActiveProjectIDs())].sort()
    for (const projectID of ids) {
      if (projectID.trim() !== projectID) throw new SyncSubscriptionError('invalid_resource', 'project id is not canonical')
    }
    const wanted = new Set(ids)
    for (const [projectID, release] of this.releases) {
      if (wanted.has(projectID)) continue
      release()
      if (!this.options.retainReleased) this.runtime.evict?.({ type: 'session_index', id: projectID })
      this.releases.delete(projectID)
    }
    for (const projectID of ids) {
      if (this.releases.has(projectID)) continue
      const resource: ResourceKey = { type: 'session_index', id: projectID }
      try {
        this.releases.set(projectID, this.runtime.subscribe(resource, { retainOnRelease: this.options.retainReleased }))
      } catch (reason) {
        // Keep already installed interests intact.  The next project-index
        // publication or an application restart can retry this one.
        throw reason
      }
    }
    this.currentProjectID = legacySignal ? (ids[0] ?? null) : this.currentProjectID
  }
}

export const CurrentProjectInterestPolicy = SessionIndexInterestPolicy

export function createCurrentSessionSignal(initialSessionID: string | null = null): MutableCurrentSessionSignal {
  let current = initialSessionID
  const listeners = new Set<() => void>()
  return {
    get: () => current,
    set: (sessionID) => {
      if (sessionID === current) return
      current = sessionID
      for (const listener of [...listeners]) listener()
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}

export interface SessionContentInterestPolicyOptions {
  /** Durable history may remain in the bounded runtime LRU, but the runtime
   * always clears the transient overlay before releasing the reference. */
  retainReleased?: boolean
}

/** Owns the selected session-content interest independently of page mounts. */
export class SessionContentInterestPolicy {
  private readonly runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }
  private readonly signal: CurrentSessionSignal
  private readonly retainReleased: boolean
  private started = false
  private detachSignal: (() => void) | null = null
  private releaseCurrent: (() => void) | null = null
  private currentSessionID: string | null = null

  constructor(runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }, signal: CurrentSessionSignal, options: SessionContentInterestPolicyOptions = {}) {
    this.runtime = runtime
    this.signal = signal
    this.retainReleased = options.retainReleased ?? true
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detachSignal = this.signal.subscribe(() => this.reconcile())
    this.reconcile()
  }

  stop(): void {
    if (!this.started && !this.releaseCurrent && !this.detachSignal) return
    this.started = false
    this.detachSignal?.()
    this.detachSignal = null
    const old = this.currentSessionID
    this.releaseCurrent?.()
    if (old && !this.retainReleased) this.runtime.evict?.({ type: 'session_content', id: old })
    this.releaseCurrent = null
    this.currentSessionID = null
  }

  get sessionID(): string | null { return this.currentSessionID }

  private reconcile(): void {
    if (!this.started) return
    const next = this.signal.get() || null
    if (next !== null && next.trim() !== next) throw new SyncSubscriptionError('invalid_resource', 'current session id is not canonical')
    if (next === this.currentSessionID) return
    const old = this.currentSessionID
    this.releaseCurrent?.()
    if (old && !this.retainReleased) this.runtime.evict?.({ type: 'session_content', id: old })
    this.releaseCurrent = null
    if (!next) {
      this.currentSessionID = null
      return
    }
    try {
      this.releaseCurrent = this.runtime.subscribe({ type: 'session_content', id: next }, { retainOnRelease: this.retainReleased })
      this.currentSessionID = next
    } catch (reason) {
      this.currentSessionID = null
      throw reason
    }
  }
}
