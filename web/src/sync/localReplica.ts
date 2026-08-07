import type { ChangeOperation, ResourceKey, ResourceType, Sequence } from '../protocol/types'
import { asSyncReadError, SyncReadError } from './errors'

export type ReplicaReadState = 'loading' | 'ready' | 'stale' | 'error'

export interface ResourceMetadata {
  readonly streamEpoch?: string
  readonly sequence?: Sequence
  readonly resourceRevision?: string
  readonly generation: number
  readonly readState: ReplicaReadState
  readonly error?: SyncReadError
}

export interface ReplicaApplyContext {
  readonly resource: ResourceKey
  readonly streamEpoch?: string
  readonly resourceRevision: string
  readonly generation: number
}

export interface TransientResumeToken {
  readonly runEpoch: string
  readonly runID: string
  readonly runCursor: string
}

export interface ResourceAdapter<T, TTransient = unknown> {
  readonly resourceType: ResourceType
  validateResourceRevision?(revision: string): void
  decodeSnapshot(value: unknown, previous: T | undefined, context?: ReplicaApplyContext): T
  applyChange(previous: T, operations: readonly ChangeOperation[], context?: ReplicaApplyContext): T
  /**
   * A resource-level authority reset can make repository-retained ranges
   * invalid even when the resource generation is unchanged.  This hook keeps
   * that decision with the resource adapter instead of making LocalReplica
   * understand history/compaction protocol details.
   */
  shouldInvalidateRetainedWindow?(operations: readonly ChangeOperation[]): boolean
  /** A complete snapshot is itself a retained-window barrier for this resource. */
  retainedWindowInvalidatedBySnapshot?(): boolean
  applyTransient?(previous: T, event: TTransient, context?: ReplicaApplyContext): T
  clearTransient?(previous: T): T
  getTransientResume?(previous: T): TransientResumeToken | undefined
}

export interface ReplicaApplyMetadata {
  streamEpoch: string
  sequence: Sequence
  resourceRevision: string
  generation: number
  readState?: ReplicaReadState
}

export interface StagedReplicaChange {
  operations: readonly ChangeOperation[]
  metadata: ReplicaApplyMetadata
}

interface ResourceRecord {
  value: unknown
  initialized: boolean
  metadata: ResourceMetadata
}

function sameError(left?: SyncReadError, right?: SyncReadError): boolean {
  if (left === right) return true
  if (!left || !right) return false
  return left.code === right.code && left.message === right.message && left.resourceKey === right.resourceKey
}

function sameReadModelState(left: ResourceRecord | undefined, right: ResourceRecord): boolean {
  if (!left) return false
  return left.value === right.value &&
    left.initialized === right.initialized &&
    left.metadata.readState === right.metadata.readState &&
    sameError(left.metadata.error, right.metadata.error)
}

export function resourceKeyString(resource: ResourceKey): string {
  return JSON.stringify([resource.type, resource.id])
}

export function resourceKeyFromString(value: string): ResourceKey | undefined {
  try {
    const parsed = JSON.parse(value) as unknown
    if (!Array.isArray(parsed) || parsed.length !== 2 || typeof parsed[0] !== 'string' || typeof parsed[1] !== 'string') return undefined
    return { type: parsed[0] as ResourceKey['type'], id: parsed[1] }
  } catch {
    return undefined
  }
}

export interface ReplicaNotification {
  /** The resource authority replaced a retained window or was evicted. */
  readonly retainedWindowInvalidated?: boolean
}

type ReplicaListener = (resource: ResourceKey, notification?: ReplicaNotification) => void

/**
 * Transactional normalized replica. Resource values are only replaced after
 * an adapter has validated the complete incoming payload, so a malformed
 * operation cannot expose a partially applied map to a repository.
 */
export class LocalReplica {
  private records = new Map<string, ResourceRecord>()
  private listeners = new Set<ReplicaListener>()

  /**
   * Releases the process-local read model. A composition owns one replica;
   * disposing the composition must not leave records or observers reachable
   * through an otherwise dead application graph.
   */
  dispose(): void {
    this.records.clear()
    this.listeners.clear()
  }

  subscribe(listener: ReplicaListener): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  get<T>(resource: ResourceKey): { value: T | undefined; initialized: boolean; metadata: ResourceMetadata } {
    const record = this.records.get(resourceKeyString(resource))
    if (!record) return { value: undefined, initialized: false, metadata: { generation: 0, readState: 'loading' } }
    return record as { value: T | undefined; initialized: boolean; metadata: ResourceMetadata }
  }

  beginLoading(resource: ResourceKey, generation: number): void {
    this.updateMetadata(resource, (current) => ({
      ...current.metadata,
      generation,
      readState: current.initialized ? 'stale' : 'loading',
      error: undefined,
    }))
  }

  markStale(resource: ResourceKey, error?: SyncReadError, generation?: number): void {
    this.updateMetadata(resource, (current) => ({
      ...current.metadata,
      generation: generation ?? current.metadata.generation,
      readState: current.initialized ? 'stale' : 'loading',
      error,
    }))
  }

  markError(resource: ResourceKey, reason: unknown, generation?: number): void {
    const error = asSyncReadError(reason, 'transport', resourceKeyString(resource))
    this.updateMetadata(resource, (current) => ({
      ...current.metadata,
      generation: generation ?? current.metadata.generation,
      readState: current.initialized ? 'stale' : 'error',
      error,
    }))
  }

  markReady(resource: ResourceKey, metadata: { streamEpoch: string; sequence: Sequence; generation: number }): void {
    const key = resourceKeyString(resource)
    const old = this.records.get(key)
    if (!old) return
    this.records.set(key, {
      ...old,
      metadata: {
        ...old.metadata,
        ...metadata,
        readState: 'ready',
        error: undefined,
      },
    })
    if (!sameReadModelState(old, this.records.get(key)!)) this.notify(resource)
  }

  applySnapshot<T>(
    resource: ResourceKey,
    adapter: ResourceAdapter<T>,
    content: unknown,
    metadata: ReplicaApplyMetadata,
  ): void {
    this.applySnapshotAndChanges(resource, adapter, content, metadata, [])
  }

  /**
   * Stages a complete snapshot barrier without publishing any intermediate
   * value. Adapter validation and every queued operation happen against local
   * temporaries; the record and its single notification are committed only
   * after the whole batch succeeds.
   */
  applySnapshotAndChanges<T>(
    resource: ResourceKey,
    adapter: ResourceAdapter<T>,
    content: unknown,
    snapshotMetadata: ReplicaApplyMetadata,
    changes: readonly StagedReplicaChange[],
  ): void {
    this.applySnapshotAndChangesAndTransient(resource, adapter, content, snapshotMetadata, changes, [])
  }

  /**
   * Applies a snapshot barrier, durable changes, and any transient frames
   * queued while a Blob was downloading as one publication. Transient frames
   * do not alter replica sequence metadata, but they must not be exposed in a
   * half-applied state either.
   */
  applySnapshotAndChangesAndTransient<T, TTransient>(
    resource: ResourceKey,
    adapter: ResourceAdapter<T, TTransient>,
    content: unknown,
    snapshotMetadata: ReplicaApplyMetadata,
    changes: readonly StagedReplicaChange[],
    transients: readonly TTransient[],
  ): void {
    const key = resourceKeyString(resource)
    const current = this.records.get(key)
    let value: T
    try {
      adapter.validateResourceRevision?.(snapshotMetadata.resourceRevision)
      value = adapter.decodeSnapshot(content, current?.value as T | undefined, {
        resource,
        streamEpoch: snapshotMetadata.streamEpoch,
        resourceRevision: snapshotMetadata.resourceRevision,
        generation: snapshotMetadata.generation,
      })
    } catch {
      throw new SyncReadError('invalid_snapshot', 'resource snapshot failed validation', key)
    }

    let finalMetadata = snapshotMetadata
    try {
      for (const change of changes) {
        adapter.validateResourceRevision?.(change.metadata.resourceRevision)
        value = adapter.applyChange(value, change.operations, {
          resource,
          streamEpoch: change.metadata.streamEpoch,
          resourceRevision: change.metadata.resourceRevision,
          generation: change.metadata.generation,
        })
        finalMetadata = change.metadata
      }
      if (adapter.applyTransient) {
        for (const transient of transients) {
          value = adapter.applyTransient(value, transient, {
            resource,
            resourceRevision: finalMetadata.resourceRevision,
            generation: finalMetadata.generation,
          })
        }
      } else if (transients.length > 0) {
        throw new SyncReadError('invalid_change', 'resource does not support transient events', key)
      }
    } catch {
      throw new SyncReadError('invalid_change', 'resource barrier change failed validation', key)
    }

    const next: ResourceRecord = {
      value,
      initialized: true,
      metadata: {
        ...finalMetadata,
        readState: finalMetadata.readState ?? 'ready',
        error: undefined,
      },
    }
    this.records.set(key, next)
    // A resource may declare a complete snapshot to be an authority barrier.
    // Retained page/blob ranges then belong to the previous snapshot even
    // when the server reuses the same resource generation. Even an
    // equal-looking replacement must be observable in that case.
    const retainedWindowInvalidated = adapter.retainedWindowInvalidatedBySnapshot?.() ?? false
    if (!sameReadModelState(current, next) || retainedWindowInvalidated) {
      this.notify(resource, { retainedWindowInvalidated })
    }
  }

  applyChange<T>(
    resource: ResourceKey,
    adapter: ResourceAdapter<T>,
    operations: readonly ChangeOperation[],
    metadata: ReplicaApplyMetadata,
  ): void {
    const key = resourceKeyString(resource)
    const current = this.records.get(key)
    if (!current?.initialized) throw new SyncReadError('invalid_change', 'change arrived before a resource snapshot', key)
    let value: T
    let retainedWindowInvalidated = false
    try {
      adapter.validateResourceRevision?.(metadata.resourceRevision)
      retainedWindowInvalidated = adapter.shouldInvalidateRetainedWindow?.(operations) ?? false
      value = adapter.applyChange(current.value as T, operations, {
        resource,
        resourceRevision: metadata.resourceRevision,
        generation: metadata.generation,
      })
    } catch {
      throw new SyncReadError('invalid_change', 'resource change failed validation', key)
    }
    const next: ResourceRecord = {
      value,
      initialized: true,
      metadata: {
        ...metadata,
        readState: metadata.readState ?? 'ready',
        error: undefined,
      },
    }
    this.records.set(key, next)
    if (!sameReadModelState(current, next) || retainedWindowInvalidated) this.notify(resource, { retainedWindowInvalidated })
  }

  /** Apply a transient subscription event without changing durable sequence metadata. */
  applyTransient<T, TTransient>(
    resource: ResourceKey,
    adapter: ResourceAdapter<T, TTransient>,
    event: TTransient,
    generation?: number,
  ): void {
    const key = resourceKeyString(resource)
    const current = this.records.get(key)
    if (!current?.initialized) throw new SyncReadError('invalid_change', 'transient event arrived before a resource snapshot', key)
    if (!adapter.applyTransient) throw new SyncReadError('invalid_change', 'resource does not support transient events', key)
    let value: T
    try {
      value = adapter.applyTransient(current.value as T, event, {
        resource,
        resourceRevision: current.metadata.resourceRevision ?? '',
        generation: generation ?? current.metadata.generation,
      })
    } catch {
      throw new SyncReadError('invalid_change', 'resource transient event failed validation', key)
    }
    const next: ResourceRecord = { ...current, value }
    this.records.set(key, next)
    if (!sameReadModelState(current, next)) this.notify(resource)
  }

  /** Clear only a resource's transient overlay while retaining durable data. */
  clearTransient<T>(resource: ResourceKey, adapter: ResourceAdapter<T>): void {
    const key = resourceKeyString(resource)
    const current = this.records.get(key)
    if (!current?.initialized || !adapter.clearTransient) return
    let value: T
    try { value = adapter.clearTransient(current.value as T) } catch { return }
    const next: ResourceRecord = { ...current, value }
    this.records.set(key, next)
    if (!sameReadModelState(current, next)) this.notify(resource)
  }

  /**
   * Atomically discard a transient overlay and expose the recovery state.
   * Recovery must not publish a ready resource with only half of the old
   * overlay removed; observers receive one replica transaction instead.
   */
  clearTransientAndMarkStale<T>(resource: ResourceKey, adapter: ResourceAdapter<T>, error?: SyncReadError, generation?: number): void {
    const key = resourceKeyString(resource)
    const current = this.records.get(key)
    if (!current) {
      this.markStale(resource, error, generation)
      return
    }
    let value = current.value as T | undefined
    if (current.initialized && adapter.clearTransient) {
      try { value = adapter.clearTransient(current.value as T) } catch { /* retain durable value and still publish stale */ }
    }
    const next: ResourceRecord = {
      ...current,
      value,
      metadata: {
        ...current.metadata,
        generation: generation ?? current.metadata.generation,
        readState: current.initialized ? 'stale' : 'loading',
        error,
      },
    }
    this.records.set(key, next)
    if (!sameReadModelState(current, next)) this.notify(resource)
  }

  private updateMetadata(resource: ResourceKey, update: (current: ResourceRecord) => ResourceMetadata): void {
    const key = resourceKeyString(resource)
    const existing = this.records.get(key)
    const old = existing ?? { value: undefined, initialized: false, metadata: { generation: 0, readState: 'loading' as const } }
    const metadata = update(old)
    const next: ResourceRecord = { ...old, metadata }
    this.records.set(key, next)
    // Generation/sequence changes alone are internal metadata and must not
    // invalidate selectors. A first loading record is observable on the next
    // read but needs no notification because no value existed to invalidate.
    if (existing && !sameReadModelState(existing, next)) this.notify(resource)
  }

  /**
   * Remove an unowned normalized resource.  Loading/error records are also
   * observable state: a repository may retain a page operation independently
   * of an initialized snapshot, and eviction must invalidate that operation's
   * cached view rather than silently allowing it to reappear.
   */
  evict(resource: ResourceKey): void {
    const key = resourceKeyString(resource)
    const old = this.records.get(key)
    if (!old) return
    this.records.delete(key)
    this.notify(resource, { retainedWindowInvalidated: true })
  }

  private notify(resource: ResourceKey, notification?: ReplicaNotification): void {
    for (const listener of [...this.listeners]) {
      try { listener(resource, notification) } catch { /* one observer cannot abort the commit */ }
    }
  }
}
