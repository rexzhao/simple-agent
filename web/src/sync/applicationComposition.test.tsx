// @vitest-environment jsdom
import { StrictMode } from 'react'
import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { ProtocolMessage } from '../protocol/types'
import { SyncApplicationProvider } from '../applicationContext'
import { ProjectIndexAdapter } from './projectIndexAdapter'
import { useSyncApplication } from '../hooks/useSyncApplication'
import { createSyncApplication } from './applicationComposition'
import type { RuntimeTransport } from './runtime'
import type { SocketLike, TransportCloseEvent, TransportReadyEvent } from './transport'

class FakeTransport implements RuntimeTransport {
  isReady = false
  connectionGeneration = 0
  serverEpoch = 'server_1'
  readonly sent: ProtocolMessage[] = []
  starts = 0
  stops = 0
  private readonly messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private readonly ready = new Set<(event: TransportReadyEvent) => void>()
  private readonly closed = new Set<(event: TransportCloseEvent) => void>()

  start(): void {
    this.starts += 1
    this.isReady = true
    this.connectionGeneration += 1
    const event: TransportReadyEvent = {
      generation: this.connectionGeneration,
      serverEpoch: this.serverEpoch,
      connectionID: `connection_${this.connectionGeneration}`,
      heartbeatIntervalMS: 15_000,
      maxMessageBytes: 256 * 1024,
    }
    for (const listener of [...this.ready]) listener(event)
  }

  stop(): void {
    if (!this.isReady) return
    this.stops += 1
    this.isReady = false
    const event: TransportCloseEvent = { generation: this.connectionGeneration, willRetry: false }
    for (const listener of [...this.closed]) listener(event)
  }

  send(message: ProtocolMessage): void { this.sent.push(message) }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.closed.add(listener); return () => this.closed.delete(listener) }

  listenerCount(): number { return this.messages.size + this.ready.size + this.closed.size }
  subscriptions(): ProtocolMessage[] { return this.sent.filter((message) => message.type === 'subscribe') }
  unsubscriptions(): ProtocolMessage[] { return this.sent.filter((message) => message.type === 'unsubscribe') }
}

class FakeSocket implements SocketLike {
  readonly readyState = 0
  onopen: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onerror: (() => void) | null = null
  onclose: (() => void) | null = null
  closeCalls = 0

  send(_data: string): void {}
  close(): void { this.closeCalls += 1 }
}

function resources(transport: FakeTransport): string[] {
  return transport.subscriptions().map((message) => message.type === 'subscribe' ? `${message.payload.resource.type}:${message.payload.resource.id}` : '')
}

function seedProjectIndex(application: ReturnType<typeof createSyncApplication>, projectID = 'project_a'): void {
  application.replica.applySnapshot(
    { type: 'project_index', id: 'server' },
    new ProjectIndexAdapter(),
    { projects: [{
      id: projectID,
      root: `/workspace/${projectID}`,
      display_name: projectID,
      archived: false,
      created_at: '2025-01-01T00:00:00Z',
      updated_at: '2025-01-01T00:00:00Z',
    }] },
    { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
  )
}

describe('application sync composition root', () => {
  it('creates one shared replica/transport graph and exposes typed command groups', () => {
    const transport = new FakeTransport()
    const blobClient = { getJSON: async () => ({}) } as never
    const application = createSyncApplication({ transport, blobClient })

    expect(application.transport).toBe(transport)
    expect(application.blobClient).toBe(blobClient)
    expect(application.runtime.transport).toBe(transport)
    expect(application.runtime.replica).toBe(application.replica)
    expect(application.stores.projectIndex.replica).toBe(application.replica)
    expect(application.stores.sessionIndex.replica).toBe(application.replica)
    expect(application.stores.sessionContent.replica).toBe(application.replica)
    expect(application.stores.providerSettings.replica).toBe(application.replica)
    expect(application.stores.codexLogin.replica).toBe(application.replica)
    expect(application.commands.project).toBe(application.commandFacade)
    expect(application.commands.session).toBe(application.commandFacade)
    expect(application.commands.run).toBe(application.commandFacade)
    expect(application.commands.provider).toBe(application.commandFacade)
    expect(application.commands.codexLogin).toBe(application.commandFacade)
    application.dispose()
  })

  it('starts and stops idempotently, with signals driving only desired resource interests', () => {
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport })
    seedProjectIndex(application)

    application.start()
    application.start()
    expect(application.started).toBe(true)
    expect(transport.starts).toBe(1)
    expect(resources(transport)).toEqual(['project_index:server', 'session_index:project_a'])

    application.signals.currentProject.set('project_a')
    application.signals.currentSession.set('session_a')
    application.signals.providerSettings.set({ settingsEnabled: true, modelSelectionNeeded: false })
    application.signals.codexLoginProvider.set('codex')
    expect(resources(transport)).toEqual([
      'project_index:server',
      'session_index:project_a',
      'session_content:session_a',
      'provider_settings:server',
      'codex_login:codex',
    ])

    application.stop()
    application.stop()
    expect(application.started).toBe(false)
    expect(transport.stops).toBe(1)
    expect(transport.listenerCount()).toBe(0)
    expect(transport.unsubscriptions()).toHaveLength(5)
    application.signals.currentProject.set('project_after_stop')
    application.signals.currentSession.set('session_after_stop')
    expect(transport.subscriptions()).toHaveLength(5)
    application.dispose()
  })

  it('keeps ticket injection at the existing transport boundary without opening a second client', async () => {
    let ticketRequests = 0
    let socket: FakeSocket | undefined
    const application = createSyncApplication({
      transportOptions: {
        capabilityToken: () => 'test-capability',
        fetcher: async () => {
          ticketRequests += 1
          return { ok: true, json: async () => ({ ticket: 'test-ticket' }) } as Response
        },
        websocketFactory: () => {
          socket = new FakeSocket()
          return socket
        },
      },
    })

    application.start()
    await Promise.resolve()
    await Promise.resolve()
    await new Promise<void>((resolve) => globalThis.setTimeout(resolve, 0))
    expect(ticketRequests).toBe(1)
    expect(socket).toBeDefined()
    application.stop()
    expect(socket?.closeCalls).toBe(1)
    application.dispose()
  })

  it('is safe across a StrictMode-like stop/remount and does not duplicate live interests', () => {
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport, initialProjectID: 'project_a', initialSessionID: 'session_a' })
    seedProjectIndex(application)

    application.start()
    application.stop()
    application.start()
    application.start()

    expect(transport.starts).toBe(2)
    expect(application.started).toBe(true)
    expect(resources(transport).filter((resource) => resource === 'project_index:server')).toHaveLength(2)
    expect(resources(transport).filter((resource) => resource === 'session_index:project_a')).toHaveLength(2)
    expect(resources(transport).filter((resource) => resource === 'session_content:session_a')).toHaveLength(2)
    // The first lifecycle released all five references before the second
    // start; only one current policy reference exists after remount.
    expect(transport.unsubscriptions()).toHaveLength(3)
    application.dispose()
  })

  it('owns the lifecycle through the real StrictMode Provider effect', () => {
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport })
    const graph = { transport: application.transport, runtime: application.runtime, replica: application.replica, commandFacade: application.commandFacade }

    const view = render(
      <StrictMode>
        <SyncApplicationProvider application={application}>
          <div data-testid="page" />
        </SyncApplicationProvider>
      </StrictMode>,
    )

    // React StrictMode runs the effect setup, cleanup, and setup again. It
    // must reuse this graph and leave exactly one live policy reference.
    expect(application.transport).toBe(graph.transport)
    expect(application.runtime).toBe(graph.runtime)
    expect(application.replica).toBe(graph.replica)
    expect(application.commandFacade).toBe(graph.commandFacade)
    expect(transport.starts).toBe(2)
    expect(transport.listenerCount()).toBe(5)
    expect(resources(transport)).toHaveLength(2)
    expect(transport.unsubscriptions()).toHaveLength(1)

    view.rerender(
      <StrictMode>
        <SyncApplicationProvider application={application}>
          <div data-testid="page" />
        </SyncApplicationProvider>
      </StrictMode>,
    )
    expect(transport.starts).toBe(2)
    expect(transport.unsubscriptions()).toHaveLength(1)
    expect(transport.listenerCount()).toBe(5)

    view.unmount()
    expect(application.started).toBe(false)
    expect(transport.listenerCount()).toBe(0)
    expect(transport.stops).toBe(2)
    expect(transport.unsubscriptions()).toHaveLength(2)
    application.dispose()
  })

  it('does not start the graph when the Provider start policy is disabled', () => {
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport })
    const view = render(
      <SyncApplicationProvider application={application} startOnMount={false}>
        <div />
      </SyncApplicationProvider>,
    )

    expect(application.started).toBe(false)
    expect(transport.starts).toBe(0)
    expect(transport.listenerCount()).toBe(0)
    view.unmount()
    expect(transport.stops).toBe(0)
    application.dispose()
  })

  it('disposes replica and store observers while keeping the disposed graph unusable', () => {
    const application = createSyncApplication()
    const resource = { type: 'project_index' as const, id: 'server' }
    const adapter = new ProjectIndexAdapter()
    const metadata = { streamEpoch: 'epoch', sequence: '0' as never, resourceRevision: '0', generation: 1 }
    let replicaNotifications = 0
    let storeNotifications = 0
    let repositoryNotifications = 0
    application.replica.subscribe(() => { replicaNotifications += 1 })
    application.stores.projectIndex.subscribe(() => { storeNotifications += 1 })
    application.repositories.projectIndex.subscribe(() => { repositoryNotifications += 1 })

    application.replica.applySnapshot(resource, adapter, { projects: [] }, metadata)
    expect(replicaNotifications).toBe(1)
    expect(storeNotifications).toBe(1)
    expect(repositoryNotifications).toBe(1)

    application.dispose()
    application.dispose()
    expect(() => application.start()).toThrow('disposed')

    // LocalReplica keeps its compatible mutation API after disposal, but all
    // application/store/repository observers have been detached and cannot
    // receive mutations through the dead composition.
    application.replica.applySnapshot(resource, adapter, { projects: [] }, { ...metadata, sequence: '1' as never })
    application.replica.markStale(resource)
    expect(replicaNotifications).toBe(1)
    expect(storeNotifications).toBe(1)
    expect(repositoryNotifications).toBe(1)
  })

})

// Keep the hook import in this test's module graph so the page-facing entry
// point is type-checked together with the composition root. The function is
// intentionally not invoked outside a Provider.
void useSyncApplication
