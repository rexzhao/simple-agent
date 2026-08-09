import { encodeMessage } from '../protocol/encode'
import { parseSequence } from '../protocol/sequence'
import type {
  AckMessage,
  ChangeMessage,
  ProtocolMessage,
  ResourceKey,
  ResyncRequiredMessage,
  Sequence,
  SnapshotMessage,
  SubscriptionEventMessage,
  RunCursor,
  SubscribeMessage,
  SubscribedMessage,
  UnsubscribeMessage,
} from '../protocol/types'
import { BlobClient } from './blobClient'
import { CodexLoginAdapter } from './codexLoginAdapter'
import { asSyncReadError, SyncReadError } from './errors'
import { LocalReplica, resourceKeyString } from './localReplica'
import type { ResourceAdapter, StagedReplicaChange, TransientResumeToken } from './localReplica'
import { ProjectIndexAdapter } from './projectIndexAdapter'
import { ProviderSettingsAdapter } from './providerSettingsAdapter'
import { SessionContentAdapter } from './sessionContentAdapter'
import { SessionIndexAdapter } from './sessionIndexAdapter'
import type { TransportCloseEvent, TransportReadyEvent, WebSocketTransport } from './transport'

export interface RuntimeTransport {
  readonly isReady: boolean
  readonly connectionGeneration: number
  readonly serverEpoch?: string
  start(): void
  stop(): void
  send(message: ProtocolMessage): void
  onMessage(listener: (message: ProtocolMessage, connectionGeneration: number) => void): () => void
  onReady(listener: (event: TransportReadyEvent) => void): () => void
  onClose(listener: (event: TransportCloseEvent) => void): () => void
}

export interface SyncRuntimeOptions {
  transport: RuntimeTransport | WebSocketTransport
  replica?: LocalReplica
  blobClient?: BlobClient
  maxSubscriptions?: number
  maxRetainedResources?: number
  maxQueuedChanges?: number
  maxQueuedBytes?: number
  maxResyncAttempts?: number
  adapters?: Partial<Record<ResourceKey['type'], (resource: ResourceKey) => ResourceAdapter<unknown, unknown>>>
}

export type SyncSubscriptionErrorCode = 'invalid_resource' | 'capacity'

export class SyncSubscriptionError extends Error {
  readonly code: SyncSubscriptionErrorCode

  constructor(code: SyncSubscriptionErrorCode, message: string) {
    super(message)
    this.name = 'SyncSubscriptionError'
    this.code = code
  }
}

export interface SyncSubscribeOptions {
  /** Keep the normalized replica value after the final reference releases. */
  retainOnRelease?: boolean
}

interface Subscription {
  resource: ResourceKey
  key: string
  references: number
  generation: number
  subscriptionID: string
  requestID?: string
  socketGeneration?: number
  streamEpoch?: string
  sequence?: Sequence
  subscribedBarrier?: Sequence
  resourceRevision?: string
  resume?: { stream_epoch: string; sequence: Sequence }
  phase: 'waiting' | 'snapshot' | 'resuming' | 'live' | 'error'
  /** The transport, rather than this resource, is the reason it is waiting. */
  transportTerminalError: boolean
  queue: Array<ChangeMessage | SubscriptionEventMessage>
  queueBytes: number
  snapshotBusy: boolean
  snapshotAbort: AbortController | null
  resyncing: boolean
  resyncAttempts: number
  retainOnRelease: boolean
}

function emptyPayloadMessage<T extends ProtocolMessage['type']>(type: T, id: string, payload: unknown): ProtocolMessage {
  return { version: 1, type, id, payload } as ProtocolMessage
}

let runtimeID = 0
function messageID(prefix: string): string {
  runtimeID += 1
  return `${prefix}_${runtimeID}`
}

function resourceMatches(left: ResourceKey, right: ResourceKey): boolean {
  return left.type === right.type && left.id === right.id
}

function sequenceAfter(previous: Sequence, current: Sequence): boolean {
  try { return parseSequence(current) === parseSequence(previous) + 1n } catch { return false }
}

function sameSequence(left: Sequence, right: Sequence): boolean {
  try { return parseSequence(left) === parseSequence(right) } catch { return false }
}

function compareSequences(left: Sequence, right: Sequence): -1 | 0 | 1 {
  try {
    const leftValue = parseSequence(left)
    const rightValue = parseSequence(right)
    return leftValue < rightValue ? -1 : leftValue > rightValue ? 1 : 0
  } catch {
    return 1
  }
}

const resourceTypes = new Set<ResourceKey['type']>([
  'project_index', 'session_index', 'session_content', 'provider_settings',
  'model_catalog', 'codex_login',
])

function wellFormedResourceID(value: string): boolean {
  if (value.length === 0 || value.trim() !== value || /[\u0000-\u001f\u007f]/.test(value)) return false
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false
      index += 1
    } else if (code >= 0xdc00 && code <= 0xdfff) return false
  }
  return true
}

function validateResourceKey(resource: ResourceKey): void {
  if (!resource || typeof resource !== 'object' || !resourceTypes.has(resource.type) || typeof resource.id !== 'string' || !wellFormedResourceID(resource.id)) {
    throw new SyncSubscriptionError('invalid_resource', 'resource type or id is not canonical')
  }
}

/**
 * Generic subscription manager and resource apply barrier. It is deliberately
 * transport- and React-independent. One subscription record owns one
 * generation; messages from an old socket or retired subscription ID are
 * ignored before they reach the replica.
 */
export class SyncRuntime {
  readonly replica: LocalReplica
  readonly transport: RuntimeTransport
  private readonly blobClient: BlobClient
  private readonly maxQueuedChanges: number
  private readonly maxQueuedBytes: number
  private readonly maxResyncAttempts: number
  private readonly maxSubscriptions: number
  private readonly maxRetainedResources: number
  private readonly adapterFactories: Partial<Record<ResourceKey['type'], (resource: ResourceKey) => ResourceAdapter<unknown, unknown>>>
  private subscriptions = new Map<string, Subscription>()
  private started = false
  private detachTransport: (() => void)[] = []
  private lastServerEpoch?: string
  private readyGeneration?: number
  private reconnectingTransport = false
  private retainedResources = new Map<string, ResourceKey>()

  constructor(options: SyncRuntimeOptions) {
    this.transport = options.transport
    this.replica = options.replica ?? new LocalReplica()
    this.blobClient = options.blobClient ?? new BlobClient()
    this.maxSubscriptions = options.maxSubscriptions ?? 64
    this.maxRetainedResources = options.maxRetainedResources ?? 64
    this.maxQueuedChanges = options.maxQueuedChanges ?? 256
    this.maxQueuedBytes = options.maxQueuedBytes ?? 2 * 1024 * 1024
    this.maxResyncAttempts = options.maxResyncAttempts ?? 8
    this.adapterFactories = options.adapters ?? {}
    if (this.maxSubscriptions <= 0 || this.maxRetainedResources <= 0 || this.maxQueuedChanges <= 0 || this.maxQueuedBytes <= 0 || this.maxResyncAttempts <= 0) throw new Error('sync queue/resync bounds must be positive')
  }

  start(): void {
    if (this.started) return
    this.started = true
    this.detachTransport = [
      this.transport.onMessage((message, generation) => this.handleMessage(message, generation)),
      this.transport.onReady((event) => this.handleReady(event)),
      this.transport.onClose((event) => this.handleClose(event)),
    ]
    this.transport.start()
    if (this.transport.isReady) {
      this.handleReady({
        generation: this.transport.connectionGeneration,
        serverEpoch: this.transport.serverEpoch ?? '',
        connectionID: '',
        heartbeatIntervalMS: 0,
        maxMessageBytes: 0,
      })
    }
  }

  stop(): void {
    if (!this.started) return
    this.started = false
    this.readyGeneration = undefined
    for (const detach of this.detachTransport.splice(0)) detach()
    for (const subscription of this.subscriptions.values()) {
      subscription.snapshotAbort?.abort()
      subscription.snapshotAbort = null
      subscription.snapshotBusy = false
      subscription.queue = []
      subscription.queueBytes = 0
      subscription.socketGeneration = undefined
      this.replica.markStale(subscription.resource, new SyncReadError('runtime_stopped', 'synchronization runtime stopped'), subscription.generation)
    }
    this.transport.stop()
  }

  /**
   * Requests recovery for a resource through the existing transport.
   *
   * A targeted retry is an explicit force-resync seam: it rebuilds that
   * desired subscription even when the resource still looks live and ready.
   * The unscoped form remains conservative and only wakes resources which
   * already report a recovery condition.
   */
  retry(resource?: ResourceKey): void {
    if (!this.started) return
    const forceTarget = resource !== undefined
    const targets = resource
      ? [...this.subscriptions.values()].filter((subscription) => resourceMatches(subscription.resource, resource))
      : [...this.subscriptions.values()]
    let wakeTransport = false
    for (const subscription of targets) {
      const readState = this.replica.get(subscription.resource).metadata.readState
      const replicaNeedsRecovery = readState === 'stale' || readState === 'error'
      if (!forceTarget && subscription.phase !== 'error' && !subscription.transportTerminalError && !replicaNeedsRecovery) continue
      const oldID = subscription.socketGeneration === undefined ? '' : subscription.subscriptionID
      subscription.snapshotAbort?.abort()
      subscription.snapshotAbort = null
      subscription.snapshotBusy = false
      subscription.queue = []
      subscription.queueBytes = 0
      subscription.socketGeneration = undefined
      subscription.requestID = undefined
      subscription.streamEpoch = undefined
      subscription.sequence = undefined
      subscription.subscribedBarrier = undefined
      subscription.resourceRevision = undefined
      subscription.phase = 'waiting'
      subscription.transportTerminalError = false
      subscription.resyncing = false
      subscription.resyncAttempts = 0
      subscription.resume = undefined
      this.replica.markStale(subscription.resource, undefined, subscription.generation)
      // Recovery is scoped to this resource. Never tear down the shared
      // socket: doing so would invalidate unrelated subscriptions and make
      // in-flight command outcomes unknown.
      if (this.transport.isReady) {
        if (oldID) {
          const unsubscribe: UnsubscribeMessage = emptyPayloadMessage('unsubscribe', messageID('unsubscribe'), { subscription_id: oldID }) as UnsubscribeMessage
          try { this.transport.send(unsubscribe) } catch { /* best effort */ }
        }
        this.sendSubscription(subscription, this.transport.connectionGeneration, true)
      } else {
        // A transport which exhausted its reconnect budget is no longer
        // running.  start() is the idempotent wake-up seam for that case and
        // is also a no-op while an ordinary reconnect is already in flight.
        wakeTransport = true
      }
    }
    if (wakeTransport) {
      try {
        this.transport.start()
        // Test/embedded transports may become ready synchronously without
        // emitting a separate ready callback.  Real WebSocketTransport stays
        // connecting here and will call handleReady from its event.
        if (this.transport.isReady) {
          this.handleReady({
            generation: this.transport.connectionGeneration,
            serverEpoch: this.transport.serverEpoch ?? '',
            connectionID: '',
            heartbeatIntervalMS: 0,
            maxMessageBytes: 0,
          })
        }
      } catch (reason) {
        for (const subscription of targets) {
          if (subscription.phase !== 'waiting') continue
          subscription.phase = 'error'
          subscription.transportTerminalError = false
          this.replica.markError(subscription.resource, asSyncReadError(reason, 'transport', subscription.key), subscription.generation)
        }
      }
    }
  }

  subscribe(resource: ResourceKey, options: SyncSubscribeOptions = {}): () => void {
    validateResourceKey(resource)
    const key = resourceKeyString(resource)
    let subscription = this.subscriptions.get(key)
    if (subscription) {
      subscription.references += 1
      subscription.retainOnRelease ||= options.retainOnRelease === true
      return () => this.release(subscription!, key)
    }
    if (this.subscriptions.size >= this.maxSubscriptions) {
      throw new SyncSubscriptionError('capacity', 'maximum active resource subscriptions reached')
    }
    this.retainedResources.delete(key)
    subscription = {
      resource: { ...resource },
      key,
      references: 1,
      generation: 0,
      subscriptionID: '',
      phase: 'waiting',
      transportTerminalError: false,
      queue: [],
      queueBytes: 0,
      snapshotBusy: false,
      snapshotAbort: null,
      resyncing: false,
      resyncAttempts: 0,
      retainOnRelease: options.retainOnRelease === true,
    }
    this.subscriptions.set(key, subscription)
    this.replica.beginLoading(subscription.resource, subscription.generation)
    if (this.started && this.transport.isReady) this.sendSubscription(subscription)
    return () => this.release(subscription!, key)
  }

  private release(subscription: Subscription, key: string): void {
    if (this.subscriptions.get(key) !== subscription) return
    subscription.references -= 1
    if (subscription.references > 0) return
    subscription.snapshotAbort?.abort()
    this.clearTransientOverlay(subscription)
    if (this.transport.isReady && subscription.subscriptionID) {
      const message: UnsubscribeMessage = emptyPayloadMessage('unsubscribe', messageID('unsubscribe'), {
        subscription_id: subscription.subscriptionID,
      }) as UnsubscribeMessage
      try { this.transport.send(message) } catch { /* disconnect owns cleanup */ }
    }
    this.subscriptions.delete(key)
    if (subscription.retainOnRelease) this.retainResource(subscription.resource)
    else this.replica.evict(subscription.resource)
  }

  /** Explicitly drops a released resource value and its metadata. */
  evict(resource: ResourceKey): void {
    validateResourceKey(resource)
    const key = resourceKeyString(resource)
    if (this.subscriptions.has(key)) return
    this.retainedResources.delete(key)
    this.replica.evict(resource)
  }

  private retainResource(resource: ResourceKey): void {
    const key = resourceKeyString(resource)
    this.retainedResources.delete(key)
    this.retainedResources.set(key, { ...resource })
    while (this.retainedResources.size > this.maxRetainedResources) {
      const oldest = this.retainedResources.entries().next().value as [string, ResourceKey] | undefined
      if (!oldest) break
      this.retainedResources.delete(oldest[0])
      this.replica.evict(oldest[1])
    }
  }

  private handleReady(event: TransportReadyEvent): void {
    if (!this.started || event.generation !== this.transport.connectionGeneration) return
    // Some embedders expose an already-open transport and also synchronously
    // emit its ready event from start(). Treat that pair as one lifecycle
    // transition so StrictMode-style start/restart cannot duplicate a
    // subscription on the same socket.
    if (this.readyGeneration === event.generation && this.lastServerEpoch === event.serverEpoch) return
    this.readyGeneration = event.generation
    this.reconnectingTransport = false
    const serverEpochChanged = this.lastServerEpoch !== undefined && this.lastServerEpoch !== event.serverEpoch
    this.lastServerEpoch = event.serverEpoch
    if (serverEpochChanged) {
      for (const subscription of this.subscriptions.values()) {
        subscription.resume = undefined
        this.clearTransientAndMarkStale(subscription, new SyncReadError('stream_epoch_mismatch', 'server epoch changed'))
      }
    }
    for (const subscription of this.subscriptions.values()) {
      if (subscription.phase !== 'error') this.sendSubscription(subscription, event.generation)
    }
  }

  private handleClose(event: TransportCloseEvent): void {
    if (!this.started) return
    const hasMatchingSocket = [...this.subscriptions.values()].some((subscription) => subscription.socketGeneration === event.generation)
    // A retrying close is only meaningful for subscriptions which were bound
    // to that socket.  A terminal close is different: the transport can
    // exhaust its budget before any subscription ever receives a socket
    // generation, so it must still publish a resource error.
    if (event.willRetry && !hasMatchingSocket) return
    if (!event.willRetry && event.generation !== this.transport.connectionGeneration) return
    this.readyGeneration = undefined
    this.reconnectingTransport = false
    for (const subscription of this.subscriptions.values()) {
      const resourceTerminal = subscription.phase === 'error'
      const sharedTransportTerminal = !event.willRetry && !resourceTerminal
      if (subscription.streamEpoch && subscription.sequence) {
        subscription.resume = { stream_epoch: subscription.streamEpoch, sequence: subscription.sequence }
      }
      subscription.snapshotAbort?.abort()
      subscription.snapshotAbort = null
      subscription.snapshotBusy = false
      subscription.queue = []
      subscription.queueBytes = 0
      subscription.socketGeneration = undefined
      subscription.phase = resourceTerminal ? 'error' : 'waiting'
      subscription.transportTerminalError = sharedTransportTerminal
      const reason = new SyncReadError('transport', 'WebSocket disconnected')
      if (!event.willRetry) this.replica.markError(subscription.resource, reason, subscription.generation)
      else this.replica.markStale(subscription.resource, reason, subscription.generation)
    }
  }

  private sendSubscription(subscription: Subscription, socketGeneration = this.transport.connectionGeneration, isolatedRecovery = false): void {
    if (!this.started || !this.transport.isReady) return
    subscription.snapshotAbort?.abort()
    subscription.snapshotAbort = null
    subscription.snapshotBusy = false
    subscription.transportTerminalError = false
    subscription.generation += 1
    const nextSubscriptionID = `${subscription.resource.type}/${subscription.resource.id}:${subscription.generation}`
    this.replica.markStale(subscription.resource, this.replica.get(subscription.resource).metadata.error, subscription.generation)
    if (!this.started || this.subscriptions.get(subscription.key) !== subscription) return
    subscription.subscriptionID = nextSubscriptionID
    subscription.socketGeneration = socketGeneration
    subscription.phase = subscription.resume ? 'resuming' : 'snapshot'
    subscription.streamEpoch = subscription.resume?.stream_epoch
    subscription.sequence = subscription.resume?.sequence
    subscription.subscribedBarrier = undefined
    subscription.resourceRevision = undefined
    subscription.queue = []
    subscription.queueBytes = 0
    subscription.resyncing = false
    const payload: SubscribeMessage['payload'] = {
      subscription_id: subscription.subscriptionID,
      resource: subscription.resource,
      ...(subscription.resume ? { resume: { ...subscription.resume } } : {}),
    }
    const activeRunResume = this.transientResume(subscription)
    if (activeRunResume && subscription.resource.type === 'session_content') {
      payload.active_run_resume = {
        run_epoch: activeRunResume.runEpoch,
        run_id: activeRunResume.runID,
        run_cursor: activeRunResume.runCursor as RunCursor,
      }
    }
    const requestID = messageID('subscribe')
    subscription.requestID = requestID
    const message: SubscribeMessage = emptyPayloadMessage('subscribe', requestID, payload) as SubscribeMessage
    try {
      this.transport.send(message)
    } catch (reason) {
      const failure = new SyncReadError('transport', 'subscription could not be sent', subscription.key)
      if (isolatedRecovery) {
        subscription.phase = 'error'
        subscription.transportTerminalError = false
        this.replica.markError(subscription.resource, asSyncReadError(reason, failure.code, subscription.key), subscription.generation)
      } else {
        this.transportFailure(subscription, failure)
      }
    }
  }

  private handleMessage(message: ProtocolMessage, socketGeneration: number): void {
    if (!this.started) return
    switch (message.type) {
      case 'subscribed': this.handleSubscribed(message as SubscribedMessage, socketGeneration); return
      case 'snapshot': this.handleSnapshot(message as SnapshotMessage, socketGeneration); return
      case 'change': this.handleChange(message as ChangeMessage, socketGeneration); return
      case 'subscription_event': this.handleSubscriptionEvent(message as SubscriptionEventMessage, socketGeneration); return
      case 'resync_required': this.handleResync(message as ResyncRequiredMessage, socketGeneration); return
      case 'error': this.handleSubscriptionError(message, socketGeneration); return
      default: return
    }
  }

  private lookup(subscriptionID: string, socketGeneration: number): Subscription | undefined {
    const subscription = [...this.subscriptions.values()].find((candidate) => candidate.subscriptionID === subscriptionID)
    if (!subscription || subscription.phase === 'error' || subscription.socketGeneration !== socketGeneration) return undefined
    return subscription
  }

  private handleSubscribed(message: SubscribedMessage, socketGeneration: number): void {
    const subscription = this.lookup(message.payload.subscription_id, socketGeneration)
    if (!subscription || !resourceMatches(subscription.resource, message.payload.resource)) return
    if (subscription.phase !== 'snapshot' && subscription.phase !== 'resuming') {
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'duplicate subscribed frame'))
      return
    }
    if (subscription.subscribedBarrier !== undefined) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'overlapping subscribed frame'))
      return
    }
    subscription.subscribedBarrier = message.payload.sequence
    subscription.streamEpoch = message.payload.stream_epoch
    if (!subscription.resume) return

    if (subscription.resume.stream_epoch !== message.payload.stream_epoch) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('stream_epoch_mismatch', 'subscription stream epoch changed'))
      return
    }
    const resumeRelation = compareSequences(subscription.resume.sequence, message.payload.sequence)
    if (resumeRelation > 0) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('sequence_gap', 'resume sequence is ahead of subscribed barrier'))
      return
    }
    if (!subscription.sequence || !this.replica.get(subscription.resource).initialized) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('invalid_snapshot', 'resume requires an initialized replica'))
      return
    }
    if (resumeRelation === 0) {
      subscription.phase = 'live'
      subscription.resyncAttempts = 0
      this.replica.markReady(subscription.resource, {
        streamEpoch: subscription.streamEpoch,
        sequence: subscription.sequence,
        generation: subscription.generation,
      })
      const acked = this.sendAck(subscription)
      if (!acked) this.transportFailure(subscription, new SyncReadError('transport', 'resume ACK could not be sent'))
      return
    }
    // The client has an initialized stale value, but the server still has to
    // replay changes up to its subscribed barrier before this resource is
    // considered ready.
    subscription.phase = 'resuming'
    this.replica.markStale(subscription.resource, undefined, subscription.generation)
  }

  private handleResync(message: ResyncRequiredMessage, socketGeneration: number): void {
    const subscription = this.lookup(message.payload.subscription_id, socketGeneration)
    if (!subscription || !resourceMatches(subscription.resource, message.payload.resource)) return
    this.requestResync(subscription, socketGeneration, new SyncReadError('resync_required', 'server requested a resource resync'))
  }

  private handleSubscriptionError(message: ProtocolMessage, socketGeneration: number): void {
    if (message.type !== 'error' || !message.payload.request_id) return
    const subscription = [...this.subscriptions.values()].find((candidate) => candidate.requestID === message.payload.request_id)
    if (!subscription || !this.current(subscription, socketGeneration, subscription.generation)) return
    // A subscribe error is not left as a silent waiting state.  It consumes a
    // bounded resync attempt; after the limit requestResync publishes the
    // typed terminal error instead of retrying forever on the same socket.
    this.requestResync(subscription, socketGeneration, new SyncReadError('server', message.payload.message, subscription.key))
  }

  private handleSnapshot(message: SnapshotMessage, socketGeneration: number): void {
    const subscription = this.lookup(message.payload.subscription_id, socketGeneration)
    if (!subscription || !resourceMatches(subscription.resource, message.payload.resource)) return
    if (subscription.phase !== 'snapshot' || subscription.subscribedBarrier === undefined) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'snapshot arrived outside the snapshot phase'))
      return
    }
    if (!subscription.streamEpoch || message.payload.stream_epoch !== subscription.streamEpoch) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('stream_epoch_mismatch', 'snapshot stream epoch does not match subscription'))
      return
    }
    if (!sameSequence(message.payload.sequence, subscription.subscribedBarrier)) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('sequence_gap', 'snapshot sequence does not match subscribed barrier'))
      return
    }
    if (subscription.snapshotBusy) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('invalid_snapshot', 'overlapping resource snapshots'))
      return
    }
    subscription.snapshotBusy = true
    const generation = subscription.generation
    const controller = new AbortController()
    subscription.snapshotAbort = controller
    void this.applySnapshot(subscription, message, socketGeneration, generation, controller.signal)
  }

  private async applySnapshot(subscription: Subscription, message: SnapshotMessage, socketGeneration: number, generation: number, signal: AbortSignal): Promise<void> {
    let content: unknown
    try {
      content = 'inline' in message.payload.content
        ? message.payload.content.inline
        : await this.blobClient.getJSON(message.payload.content.blob, { signal })
    } catch (reason) {
      if (this.current(subscription, socketGeneration, generation)) {
        subscription.snapshotBusy = false
        subscription.snapshotAbort = null
        this.requestResync(subscription, socketGeneration, asSyncReadError(reason, 'invalid_snapshot', subscription.key))
      }
      return
    }
    if (!this.current(subscription, socketGeneration, generation)) return
    const queued = [...subscription.queue]
    const stagedChanges: StagedReplicaChange[] = []
    const stagedTransients: SubscriptionEventMessage['payload']['event'][] = []
    let previousSequence = message.payload.sequence
    let finalResourceRevision = message.payload.resource_revision
    try {
      for (const queuedMessage of queued) {
        if (queuedMessage.type === 'subscription_event') {
          if (!resourceMatches(subscription.resource, queuedMessage.payload.resource)) throw new SyncReadError('invalid_change', 'queued transient resource does not match subscription')
          stagedTransients.push(queuedMessage.payload.event)
          continue
        }
        const change = queuedMessage
        if (
          change.payload.stream_epoch !== message.payload.stream_epoch ||
          !sameSequence(change.payload.previous_sequence, previousSequence) ||
          !sequenceAfter(change.payload.previous_sequence, change.payload.sequence)
        ) {
          throw new SyncReadError('sequence_gap', 'queued resource change sequence is not contiguous')
        }
        stagedChanges.push({
          operations: change.payload.operations,
          metadata: {
            streamEpoch: change.payload.stream_epoch,
            sequence: change.payload.sequence,
            resourceRevision: change.payload.resource_revision,
            generation,
            readState: 'ready',
          },
        })
        previousSequence = change.payload.sequence
        finalResourceRevision = change.payload.resource_revision
      }
    } catch (reason) {
      subscription.snapshotBusy = false
      subscription.snapshotAbort = null
      this.requestResync(subscription, socketGeneration, asSyncReadError(reason, 'sequence_gap', subscription.key))
      return
    }
    try {
      const adapter = this.adapterFor(subscription.resource)
      this.replica.applySnapshotAndChangesAndTransient(
        subscription.resource,
        adapter,
        content,
        {
          streamEpoch: message.payload.stream_epoch,
          sequence: message.payload.sequence,
          resourceRevision: message.payload.resource_revision,
          generation,
          readState: 'ready',
        },
        stagedChanges,
        stagedTransients,
      )
    } catch (reason) {
      subscription.snapshotBusy = false
      subscription.snapshotAbort = null
      this.requestResync(subscription, socketGeneration, asSyncReadError(reason, 'invalid_snapshot', subscription.key))
      return
    }
    if (!this.current(subscription, socketGeneration, generation)) return
    subscription.snapshotBusy = false
    subscription.snapshotAbort = null
    subscription.streamEpoch = message.payload.stream_epoch
    subscription.sequence = previousSequence
    subscription.resourceRevision = finalResourceRevision
    subscription.resume = { stream_epoch: subscription.streamEpoch, sequence: subscription.sequence }
    subscription.phase = 'live'
    subscription.resyncAttempts = 0
    subscription.queue = []
    subscription.queueBytes = 0
    if (!this.sendAck(subscription)) this.transportFailure(subscription, new SyncReadError('transport', 'snapshot ACK could not be sent'))
  }

  private handleChange(message: ChangeMessage, socketGeneration: number): void {
    const subscription = this.lookup(message.payload.subscription_id, socketGeneration)
    if (!subscription || !resourceMatches(subscription.resource, message.payload.resource)) return
    if (subscription.subscribedBarrier === undefined) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'change arrived before subscribed'))
      return
    }
    if (!subscription.streamEpoch || message.payload.stream_epoch !== subscription.streamEpoch) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('stream_epoch_mismatch', 'change stream epoch does not match subscription'))
      return
    }
    if (subscription.snapshotBusy || subscription.phase === 'snapshot') {
      const bytes = this.messageSize(message)
      if (subscription.queue.length >= this.maxQueuedChanges || subscription.queueBytes + bytes > this.maxQueuedBytes) {
        this.requestResync(subscription, socketGeneration, new SyncReadError('sequence_gap', 'resource change buffer overflowed'))
        return
      }
      subscription.queue.push(message)
      subscription.queueBytes += bytes
      return
    }
    if (subscription.phase !== 'resuming' && subscription.phase !== 'live') {
      if (subscription.phase === 'error') return
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'change arrived outside a live phase'))
      return
    }
    this.applyChange(subscription, message, socketGeneration, subscription.generation)
  }

  private handleSubscriptionEvent(message: SubscriptionEventMessage, socketGeneration: number): void {
    const subscription = this.lookup(message.payload.subscription_id, socketGeneration)
    if (!subscription || !resourceMatches(subscription.resource, message.payload.resource)) return
    if (subscription.resource.type !== 'session_content') return
    if (subscription.subscribedBarrier === undefined) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'transient event arrived before subscribed'))
      return
    }
    if (subscription.snapshotBusy || subscription.phase === 'snapshot' || subscription.phase === 'resuming') {
      const bytes = this.messageSize(message)
      if (subscription.queue.length >= this.maxQueuedChanges || subscription.queueBytes + bytes > this.maxQueuedBytes) {
        this.requestResync(subscription, socketGeneration, new SyncReadError('sequence_gap', 'resource transient buffer overflowed'))
        return
      }
      subscription.queue.push(message)
      subscription.queueBytes += bytes
      return
    }
    if (subscription.phase !== 'live') {
      if (subscription.phase === 'error') return
      this.requestResync(subscription, socketGeneration, new SyncReadError('protocol', 'transient event arrived outside a live phase'))
      return
    }
    const adapter = this.adapterFor(subscription.resource)
    if (!adapter.applyTransient) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('invalid_change', 'session content adapter does not support transient events'))
      return
    }
    try {
      this.replica.applyTransient(subscription.resource, adapter, message.payload.event, subscription.generation)
    } catch (reason) {
      this.requestResync(subscription, socketGeneration, asSyncReadError(reason, 'sequence_gap', subscription.key))
    }
  }

  private applyChange(subscription: Subscription, message: ChangeMessage, socketGeneration: number, generation: number): boolean {
    if (!this.current(subscription, socketGeneration, generation)) return false
    if (!subscription.streamEpoch || !subscription.sequence || message.payload.stream_epoch !== subscription.streamEpoch || !sameSequence(message.payload.previous_sequence, subscription.sequence) || !sequenceAfter(message.payload.previous_sequence, message.payload.sequence)) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('sequence_gap', 'resource change sequence is not contiguous'))
      return false
    }
    if (subscription.phase === 'resuming' && subscription.subscribedBarrier !== undefined && compareSequences(message.payload.sequence, subscription.subscribedBarrier) > 0) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('sequence_gap', 'replay change passed subscribed barrier'))
      return false
    }
    const reachesReplayBarrier = subscription.phase === 'resuming' && subscription.subscribedBarrier !== undefined && sameSequence(message.payload.sequence, subscription.subscribedBarrier)
    try {
      const adapter = this.adapterFor(subscription.resource)
      adapter.validateResourceRevision?.(message.payload.resource_revision)
      this.replica.applyChange(subscription.resource, adapter, message.payload.operations, {
        streamEpoch: message.payload.stream_epoch,
        sequence: message.payload.sequence,
        resourceRevision: message.payload.resource_revision,
        generation,
        readState: reachesReplayBarrier ? 'ready' : subscription.phase === 'resuming' ? 'stale' : 'ready',
      })
    } catch (reason) {
      this.requestResync(subscription, socketGeneration, asSyncReadError(reason, 'invalid_change', subscription.key))
      return false
    }
    if (!this.current(subscription, socketGeneration, generation)) return false
    subscription.sequence = message.payload.sequence
    subscription.resourceRevision = message.payload.resource_revision
    subscription.resume = { stream_epoch: subscription.streamEpoch, sequence: subscription.sequence }
    if (reachesReplayBarrier) {
      subscription.phase = 'live'
      subscription.resyncAttempts = 0
      if (!this.drainQueuedTransients(subscription, socketGeneration, generation)) return false
    }
    const acked = this.sendAck(subscription)
    if (!acked) this.transportFailure(subscription, new SyncReadError('transport', 'change ACK could not be sent'))
    return acked
  }

  private sendAck(subscription: Subscription): boolean {
    if (!subscription.streamEpoch || !subscription.sequence || !subscription.subscriptionID) return false
    const message: AckMessage = emptyPayloadMessage('ack', messageID('ack'), {
      subscription_id: subscription.subscriptionID,
      stream_epoch: subscription.streamEpoch,
      sequence: subscription.sequence,
    }) as AckMessage
    try {
      this.transport.send(message)
      return true
    } catch {
      return false
    }
  }

  private drainQueuedTransients(subscription: Subscription, socketGeneration: number, generation: number): boolean {
    const queued = subscription.queue.filter((message): message is SubscriptionEventMessage => message.type === 'subscription_event')
    if (queued.length === 0) return true
    subscription.queue = []
    subscription.queueBytes = 0
    const adapter = this.adapterFor(subscription.resource)
    if (!adapter.applyTransient) {
      this.requestResync(subscription, socketGeneration, new SyncReadError('invalid_change', 'session content adapter does not support transient events'))
      return false
    }
    for (const message of queued) {
      if (!this.current(subscription, socketGeneration, generation)) return false
      if (!resourceMatches(subscription.resource, message.payload.resource)) {
        this.requestResync(subscription, socketGeneration, new SyncReadError('invalid_change', 'queued transient resource does not match subscription'))
        return false
      }
      try {
        this.replica.applyTransient(subscription.resource, adapter, message.payload.event, generation)
      } catch (reason) {
        this.requestResync(subscription, socketGeneration, asSyncReadError(reason, 'sequence_gap', subscription.key))
        return false
      }
    }
    return true
  }

  private transportFailure(subscription: Subscription, reason: SyncReadError): void {
    if (!this.started || this.subscriptions.get(subscription.key) !== subscription) return
    subscription.phase = 'waiting'
    this.replica.markStale(subscription.resource, reason, subscription.generation)
    if (!this.transport.isReady || this.reconnectingTransport) return
    this.reconnectingTransport = true
    this.readyGeneration = undefined
    try {
      this.transport.stop()
      if (this.started) this.transport.start()
    } catch (failure) {
      this.reconnectingTransport = false
      this.replica.markError(subscription.resource, asSyncReadError(failure, 'transport', subscription.key), subscription.generation)
    }
  }

  private requestResync(subscription: Subscription, socketGeneration: number, reason: SyncReadError): void {
    if (!this.current(subscription, socketGeneration, subscription.generation) || subscription.resyncing) return
    subscription.resyncing = true
    subscription.snapshotAbort?.abort()
    subscription.snapshotAbort = null
    subscription.snapshotBusy = false
    subscription.queue = []
    subscription.queueBytes = 0
    subscription.resume = undefined
    subscription.streamEpoch = undefined
    subscription.sequence = undefined
    subscription.resourceRevision = undefined
    subscription.phase = 'waiting'
    this.clearTransientAndMarkStale(subscription, reason)
    if (!this.started || this.subscriptions.get(subscription.key) !== subscription) {
      subscription.resyncing = false
      return
    }
    const oldID = subscription.subscriptionID
    subscription.resyncAttempts += 1
    if (subscription.resyncAttempts > this.maxResyncAttempts) {
      if (this.transport.isReady && oldID) {
        const unsubscribe: UnsubscribeMessage = emptyPayloadMessage('unsubscribe', messageID('unsubscribe'), { subscription_id: oldID }) as UnsubscribeMessage
        try { this.transport.send(unsubscribe) } catch { /* best effort */ }
      }
      subscription.resyncing = false
      subscription.phase = 'error'
      subscription.transportTerminalError = false
      this.replica.markError(subscription.resource, reason, subscription.generation)
      return
    }
    if (this.transport.isReady && oldID) {
      const unsubscribe: UnsubscribeMessage = emptyPayloadMessage('unsubscribe', messageID('unsubscribe'), { subscription_id: oldID }) as UnsubscribeMessage
      try { this.transport.send(unsubscribe) } catch { /* reconnect will retry */ }
      this.sendSubscription(subscription, socketGeneration)
    }
    subscription.resyncing = false
  }

  private current(subscription: Subscription, socketGeneration: number, generation: number): boolean {
    return this.started && this.subscriptions.get(subscription.key) === subscription && subscription.socketGeneration === socketGeneration && subscription.generation === generation
  }

  private adapterFor(resource: ResourceKey): ResourceAdapter<unknown, unknown> {
    const factory = this.adapterFactories[resource.type]
    if (factory) return factory(resource)
    if (resource.type === 'session_index') return new SessionIndexAdapter(resource.id) as ResourceAdapter<unknown, unknown>
    if (resource.type === 'session_content') return new SessionContentAdapter(resource.id) as ResourceAdapter<unknown, unknown>
    if (resource.type === 'project_index') return new ProjectIndexAdapter(resource.id) as ResourceAdapter<unknown, unknown>
    if (resource.type === 'provider_settings') return new ProviderSettingsAdapter(resource.id) as ResourceAdapter<unknown, unknown>
    if (resource.type === 'codex_login') return new CodexLoginAdapter(resource.id) as ResourceAdapter<unknown, unknown>
    throw new SyncReadError('invalid_snapshot', 'resource type is not registered', resourceKeyString(resource))
  }

  private messageSize(message: ProtocolMessage): number {
    try {
      const encoded = encodeMessage(message)
      return typeof TextEncoder === 'undefined' ? encoded.length : new TextEncoder().encode(encoded).byteLength
    } catch { return this.maxQueuedBytes }
  }

  private transientResume(subscription: Subscription): TransientResumeToken | undefined {
    if (subscription.resource.type !== 'session_content') return undefined
    try {
      const adapter = this.adapterFor(subscription.resource)
      const value = this.replica.get<unknown>(subscription.resource).value
      return value === undefined ? undefined : adapter.getTransientResume?.(value)
    } catch {
      return undefined
    }
  }

  private clearTransientOverlay(subscription: Subscription): void {
    try {
      const adapter = this.adapterFor(subscription.resource)
      this.replica.clearTransient(subscription.resource, adapter)
    } catch {
      // A released/invalid resource is allowed to be absent from the replica.
    }
  }

  private clearTransientAndMarkStale(subscription: Subscription, reason: SyncReadError): void {
    try {
      const adapter = this.adapterFor(subscription.resource)
      this.replica.clearTransientAndMarkStale(subscription.resource, adapter, reason, subscription.generation)
    } catch {
      // A released/invalid resource is allowed to be absent from the replica.
      this.replica.markStale(subscription.resource, reason, subscription.generation)
    }
  }
}
