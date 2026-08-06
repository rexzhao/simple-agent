import type { ResourceKey } from '../protocol/types'
import { SyncSubscriptionError, type SyncRuntime } from './runtime'

/**
 * Application-owned navigation signal. It is intentionally not a React hook:
 * the policy must remain alive when the last page subscriber unmounts.
 */
export interface CurrentProjectSignal {
  get(): string | null
  subscribe(listener: () => void): () => void
}

export interface MutableCurrentProjectSignal extends CurrentProjectSignal {
  set(projectID: string | null): void
}

export interface SessionIndexInterestPolicyOptions {
  /** Keep the old project replica when switching; runtime bounds retained values. */
  retainReleased?: boolean
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

/**
 * Minimal application-level input for the provider-settings interest policy.
 * This is intentionally not a route or React signal: settings can be needed
 * by model selection before the settings page mounts.
 */
export interface ProviderSettingsApplicationState {
  readonly settingsEnabled: boolean
  readonly modelSelectionNeeded: boolean
}

export interface ProviderSettingsApplicationStateSignal {
  get(): ProviderSettingsApplicationState
  subscribe(listener: () => void): () => void
}

export interface MutableProviderSettingsApplicationStateSignal extends ProviderSettingsApplicationStateSignal {
  set(next: ProviderSettingsApplicationState): void
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

/**
 * Keeps the current project's Session Index desired subscription independent
 * of component lifetime. Switching projects releases the old desired resource
 * and retains exactly one reference to the new one.
 */
export class SessionIndexInterestPolicy {
  private readonly runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }
  private readonly projectSignal: CurrentProjectSignal
  private readonly options: Required<SessionIndexInterestPolicyOptions>
  private started = false
  private detachSignal: (() => void) | null = null
  private releaseCurrent: (() => void) | null = null
  private currentProjectID: string | null = null

  constructor(runtime: Pick<SyncRuntime, 'subscribe' | 'evict'> | { subscribe: SyncRuntime['subscribe']; evict?: SyncRuntime['evict'] }, projectSignal: CurrentProjectSignal, options: SessionIndexInterestPolicyOptions = {}) {
    this.runtime = runtime
    this.projectSignal = projectSignal
    this.options = { retainReleased: options.retainReleased ?? false }
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detachSignal = this.projectSignal.subscribe(() => this.reconcile())
    this.reconcile()
  }

  stop(): void {
    if (!this.started && !this.releaseCurrent && !this.detachSignal) return
    this.started = false
    this.detachSignal?.()
    this.detachSignal = null
    const oldProjectID = this.currentProjectID
    this.releaseCurrent?.()
    if (oldProjectID && !this.options.retainReleased) this.runtime.evict?.({ type: 'session_index', id: oldProjectID })
    this.releaseCurrent = null
    this.currentProjectID = null
  }

  get projectID(): string | null {
    return this.currentProjectID
  }

  private reconcile(): void {
    if (!this.started) return
    const signaled = this.projectSignal.get()
    const next = signaled || null
    if (next !== null && next.trim() !== next) throw new SyncSubscriptionError('invalid_resource', 'current project id is not canonical')
    if (next === this.currentProjectID) return
    const oldProjectID = this.currentProjectID
    this.releaseCurrent?.()
    if (oldProjectID && !this.options.retainReleased) this.runtime.evict?.({ type: 'session_index', id: oldProjectID })
    this.releaseCurrent = null
    if (!next) {
      this.currentProjectID = null
      return
    }
    const resource: ResourceKey = { type: 'session_index', id: next }
    try {
      this.releaseCurrent = this.runtime.subscribe(resource, { retainOnRelease: this.options.retainReleased })
      this.currentProjectID = next
    } catch (reason) {
      this.currentProjectID = null
      throw reason
    }
  }
}

export const CurrentProjectInterestPolicy = SessionIndexInterestPolicy

export interface CurrentSessionSignal {
  get(): string | null
  subscribe(listener: () => void): () => void
}

export interface MutableCurrentSessionSignal extends CurrentSessionSignal {
  set(sessionID: string | null): void
}

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
