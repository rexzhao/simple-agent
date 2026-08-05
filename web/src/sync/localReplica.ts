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

export interface ResourceAdapter<T> {
  readonly resourceType: ResourceType
  validateResourceRevision?(revision: string): void
  decodeSnapshot(value: unknown, previous: T | undefined): T
  applyChange(previous: T, operations: readonly ChangeOperation[]): T
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

type ReplicaListener = (resource: ResourceKey) => void

/**
 * Transactional normalized replica. Resource values are only replaced after
 * an adapter has validated the complete incoming payload, so a malformed
 * operation cannot expose a partially applied map to a repository.
 */
export class LocalReplica {
  private records = new Map<string, ResourceRecord>()
  private listeners = new Set<ReplicaListener>()

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
    const key = resourceKeyString(resource)
    const current = this.records.get(key)
    let value: T
    try {
      adapter.validateResourceRevision?.(snapshotMetadata.resourceRevision)
      value = adapter.decodeSnapshot(content, current?.value as T | undefined)
    } catch {
      throw new SyncReadError('invalid_snapshot', 'resource snapshot failed validation', key)
    }

    let finalMetadata = snapshotMetadata
    try {
      for (const change of changes) {
        adapter.validateResourceRevision?.(change.metadata.resourceRevision)
        value = adapter.applyChange(value, change.operations)
        finalMetadata = change.metadata
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
    if (!sameReadModelState(current, next)) this.notify(resource)
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
    try {
      adapter.validateResourceRevision?.(metadata.resourceRevision)
      value = adapter.applyChange(current.value as T, operations)
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

  /** Remove an unowned normalized resource and notify only if its read model existed. */
  evict(resource: ResourceKey): void {
    const key = resourceKeyString(resource)
    const old = this.records.get(key)
    if (!old) return
    this.records.delete(key)
    if (old.initialized) this.notify(resource)
  }

  private notify(resource: ResourceKey): void {
    for (const listener of [...this.listeners]) {
      try { listener(resource) } catch { /* one observer cannot abort the commit */ }
    }
  }
}
