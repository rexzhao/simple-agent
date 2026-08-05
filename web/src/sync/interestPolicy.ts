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
