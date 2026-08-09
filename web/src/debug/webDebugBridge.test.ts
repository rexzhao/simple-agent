// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import { encodeMessage } from '../protocol/encode'
import type { DebugExecutionResultPayload, ProtocolMessage } from '../protocol/types'
import { ProjectIndexAdapter } from '../sync/projectIndexAdapter'
import { SessionIndexAdapter } from '../sync/sessionIndexAdapter'
import { SessionContentAdapter } from '../sync/sessionContentAdapter'
import { SyncReadError } from '../sync/errors'
import { createSyncApplication } from '../sync/applicationComposition'
import type { SyncRuntime } from '../sync/runtime'
import type { CommandFacade } from '../sync/commandFacade'
import type { RuntimeTransport } from '../sync/runtime'
import type { TransportCloseEvent, TransportReadyEvent } from '../sync/transport'
import {
  WebDebugBridge,
  WebDebugIdleError,
  WebDebugSelectionError,
  WEB_DEBUG_TARGET_PROJECT_ID,
} from './webDebugBridge'

class FakeTransport implements RuntimeTransport {
  isReady = true
  connectionGeneration = 1
  serverEpoch = 'server_1'
  readonly sent: ProtocolMessage[] = []
  sendAttempts = 0
  sendError?: Error
  private readonly messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private readonly ready = new Set<(event: TransportReadyEvent) => void>()
  private readonly closed = new Set<(event: TransportCloseEvent) => void>()

  start(): void { this.isReady = true }
  stop(): void { this.close(false) }
  send(message: ProtocolMessage): void {
    this.sendAttempts += 1
    if (this.sendError) throw this.sendError
    this.sent.push(decodeMessage(encodeMessage(message)))
  }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.closed.add(listener); return () => this.closed.delete(listener) }

  emit(message: ProtocolMessage, generation = this.connectionGeneration): void {
    const decoded = decodeMessage(encodeMessage(message))
    for (const listener of [...this.messages]) listener(decoded, generation)
  }

  reconnect(): void {
    this.close(true)
    this.connectionGeneration += 1
    this.isReady = true
    const event: TransportReadyEvent = {
      generation: this.connectionGeneration,
      serverEpoch: this.serverEpoch,
      connectionID: `connection_${this.connectionGeneration}`,
      heartbeatIntervalMS: 15_000,
      maxMessageBytes: 256 * 1024,
    }
    for (const listener of [...this.ready]) listener(event)
  }

  close(willRetry: boolean): void {
    this.isReady = false
    const event: TransportCloseEvent = { generation: this.connectionGeneration, willRetry }
    for (const listener of [...this.closed]) listener(event)
  }

  last(type: ProtocolMessage['type']): ProtocolMessage | undefined {
    return [...this.sent].reverse().find((message) => message.type === type)
  }

  count(type: ProtocolMessage['type']): number { return this.sent.filter((message) => message.type === type).length }
}

const targetProject = {
  id: WEB_DEBUG_TARGET_PROJECT_ID,
  root: '/workspace/debug',
  display_name: 'debug',
  archived: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
}

function session(sessionID: string, projectID: string) {
  return {
    session_id: sessionID,
    project_id: projectID,
    parent_session_id: null,
    display_name: sessionID,
    archived: false,
    status: 'idle' as const,
    run_id: null,
    resource_revision: '1',
    updated_at: '2025-01-01T00:00:00Z',
    has_unread_result: false,
  }
}

function seedAuthority(application: ReturnType<typeof createSyncApplication>, projects = [targetProject], sessions = [session('session_a', WEB_DEBUG_TARGET_PROJECT_ID)]): void {
  application.replica.applySnapshot(
    { type: 'project_index', id: 'server' },
    new ProjectIndexAdapter(),
    { projects },
    { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
  )
  application.replica.applySnapshot(
    { type: 'session_index', id: WEB_DEBUG_TARGET_PROJECT_ID },
    new SessionIndexAdapter(WEB_DEBUG_TARGET_PROJECT_ID),
    { sessions },
    { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
  )
}

function seedSessionContent(application: ReturnType<typeof createSyncApplication>, sessionID = 'session_a'): void {
  application.replica.applySnapshot(
    { type: 'session_content', id: sessionID },
    new SessionContentAdapter(sessionID),
    {
      schema_version: 1,
      session: {
        id: sessionID,
        version: 2,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
        archived: false,
        last_used_at: '2025-01-01T00:00:00Z',
        has_unread_result: false,
        status: 'idle',
        show_reasoning: false,
        full_access: false,
        debug: { request_bodies: false },
        context: {},
        save_tool_results: false,
      },
      history: { items: [], descriptor: { limit: 20, align_turn: false, visible_only: true, has_more_before: false, has_more_after: false } },
      active_run: null,
      compaction: { checkpoints: [], truncated: false },
    },
    { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
  )
}

function createBridge(options: { focused?: boolean; busy?: () => boolean; pending?: () => number; contentReady?: boolean } = {}) {
  const transport = new FakeTransport()
  const application = createSyncApplication({ transport })
  seedAuthority(application)
  if (options.contentReady !== false) seedSessionContent(application)
  application.signals.currentProject.set(WEB_DEBUG_TARGET_PROJECT_ID)
  application.signals.currentSession.set('session_a')
  let focused = options.focused ?? false
  const documentRef = { hasFocus: () => focused } as unknown as Document
  const runtime = {
    getDebugSnapshot: () => ({ started: true, transportReady: true, activeSubscriptions: 1, busySubscriptions: options.busy?.() ? 1 : 0, busy: options.busy?.() ?? false }),
  } as unknown as SyncRuntime
  const commandFacade = { getDebugSnapshot: () => ({ started: true, pendingCount: options.pending?.() ?? 0 }) } as unknown as CommandFacade
  const bridge = new WebDebugBridge({
    transport,
    runtime,
    replica: application.replica,
    repositories: application.repositories,
    commandFacade,
    signals: application.signals,
    window,
    document: documentRef,
    pageIDGenerator: () => 'page-id',
    pageEpochGenerator: () => 'page-epoch',
    pollIntervalMS: 5,
  })
  return {
    application,
    bridge,
    transport,
    setFocus(value: boolean) { focused = value },
  }
}

function acknowledge(transport: FakeTransport, sessionID = 'session_a', generation = transport.connectionGeneration): void {
  const register = transport.last('debug_register')
  expect(register?.type).toBe('debug_register')
  if (register?.type !== 'debug_register') throw new Error('debug register was not sent')
  transport.emit({
    version: 1,
    type: 'debug_registered',
    id: 'registered',
    payload: { ...register.payload, session_id: sessionID },
  }, generation)
}

function cleanUp(bridge: WebDebugBridge, application: ReturnType<typeof createSyncApplication>): void {
  bridge.dispose()
  application.dispose()
  delete window.__SAI_DEBUG__
}

afterEach(() => {
  delete window.__SAI_DEBUG__
  vi.unstubAllGlobals()
})

describe('browser web debug bridge', () => {
  it('uses authoritative local eligibility and exposes only after a matching server ack', () => {
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport })
    const bridge = new WebDebugBridge({
      transport,
      runtime: {} as SyncRuntime,
      replica: application.replica,
      repositories: application.repositories,
      commandFacade: {} as CommandFacade,
      signals: application.signals,
      window,
      pageIDGenerator: () => 'page-id',
      pageEpochGenerator: () => 'page-epoch',
    })
    application.signals.currentProject.set(WEB_DEBUG_TARGET_PROJECT_ID)
    application.signals.currentSession.set('session_a')
    bridge.start()
    expect(transport.count('debug_register')).toBe(0)
    expect(window.__SAI_DEBUG__).toBeUndefined()

    seedAuthority(application)
    expect(transport.count('debug_register')).toBe(1)
    expect(window.__SAI_DEBUG__).toBeUndefined()
    acknowledge(transport)
    expect(window.__SAI_DEBUG__).toBeDefined()
    expect(window.__SAI_DEBUG__?.replica).toBe(application.replica)
    expect(window.__SAI_DEBUG__?.runtime).toBeDefined()
    expect(Object.keys(window).filter((key) => key.startsWith('__SAI_DEBUG'))).toEqual(['__SAI_DEBUG__'])
    cleanUp(bridge, application)
  })

  it('does not make ordinary application startup depend on browser crypto', () => {
    vi.stubGlobal('crypto', undefined)
    const transport = new FakeTransport()
    const ordinary = createSyncApplication({ transport })
    expect(() => ordinary.start()).not.toThrow()
    ordinary.stop()
    ordinary.dispose()

    const eligible = createSyncApplication({ transport: new FakeTransport() })
    seedAuthority(eligible)
    eligible.signals.currentProject.set(WEB_DEBUG_TARGET_PROJECT_ID)
    eligible.signals.currentSession.set('session_a')
    expect(() => eligible.start()).not.toThrow()
    expect(eligible.debugBridge.registered).toBe(false)
    eligible.dispose()
  })

  it('uses a secure getRandomValues UUID fallback and still rejects invalid injected identity', () => {
    const randomValues = (bytes: Uint8Array) => {
      for (let index = 0; index < bytes.length; index += 1) bytes[index] = index
      return bytes
    }
    vi.stubGlobal('crypto', { getRandomValues: randomValues })
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport })
    expect(application.debugBridge.pageID).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    expect(application.debugBridge.pageEpoch).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    application.dispose()

    const infrastructure = createSyncApplication()
    expect(() => new WebDebugBridge({
      transport: new FakeTransport(),
      runtime: {} as SyncRuntime,
      replica: infrastructure.replica,
      repositories: infrastructure.repositories,
      commandFacade: {} as CommandFacade,
      signals: infrastructure.signals,
      pageIDGenerator: () => '',
      pageEpochGenerator: () => 'epoch',
    })).toThrow()
    infrastructure.dispose()
  })

  it('blocks disabled errors without retrying until authority or connection changes', () => {
    const { bridge, application, transport } = createBridge()
    bridge.start()
    expect(transport.count('debug_register')).toBe(1)
    const register = transport.last('debug_register')
    expect(register?.type).toBe('debug_register')
    transport.emit({ version: 1, type: 'error', id: 'unrelated-error', payload: { code: 'web_debug_disabled', message: 'unrelated', request_id: 'not-this-bridge' } })
    expect(transport.count('debug_register')).toBe(1)
    transport.emit({ version: 1, type: 'error', id: 'error', payload: { code: 'web_debug_disabled', message: 'disabled', request_id: register?.id } })
    expect(window.__SAI_DEBUG__).toBeUndefined()
    for (let sequence = 2; sequence <= 5; sequence += 1) {
      application.replica.applySnapshot(
        { type: 'session_index', id: WEB_DEBUG_TARGET_PROJECT_ID },
        new SessionIndexAdapter(WEB_DEBUG_TARGET_PROJECT_ID),
        { sessions: [session('session_a', WEB_DEBUG_TARGET_PROJECT_ID)] },
        { streamEpoch: 'test', sequence: String(sequence) as never, resourceRevision: String(sequence), generation: 0 },
      )
    }
    expect(transport.count('debug_register')).toBe(1)
    window.dispatchEvent(new Event('focus'))
    transport.emit({ version: 1, type: 'debug_registered', id: 'stale', payload: { page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a', focused: false } })
    expect(window.__SAI_DEBUG__).toBeUndefined()
    expect(transport.count('debug_register')).toBe(1)

    transport.reconnect()
    expect(transport.count('debug_register')).toBe(2)
    acknowledge(transport, 'session_a', transport.connectionGeneration)
    expect(window.__SAI_DEBUG__).toBeDefined()
    cleanUp(bridge, application)
  })

  it('reconciles pending focus and unregisters on selection, summary invalidation, stop and dispose', () => {
    const { bridge, application, transport, setFocus } = createBridge()
    bridge.start()
    setFocus(true)
    window.dispatchEvent(new Event('focus'))
    expect(transport.count('debug_focus')).toBe(0)
    acknowledge(transport)
    expect(transport.last('debug_focus')?.type).toBe('debug_focus')
    const focusMessage = transport.last('debug_focus')
    expect(focusMessage?.type === 'debug_focus' && focusMessage.payload.focused).toBe(true)
    const oldFocusOperationID = focusMessage?.id
    expect(window.__SAI_DEBUG__).toBeDefined()

    const oldSurface = window.__SAI_DEBUG__!
    application.signals.currentProject.set('other-project')
    expect(window.__SAI_DEBUG__).toBeUndefined()
    expect(transport.count('debug_unregister')).toBe(1)
    const oldUnregisterOperationID = transport.last('debug_unregister')?.id
    expect(oldSurface.appState.currentProject).toBe('other-project')

    application.signals.currentProject.set(WEB_DEBUG_TARGET_PROJECT_ID)
    application.signals.currentSession.set('session_a')
    expect(transport.count('debug_register')).toBe(2)
    transport.emit({ version: 1, type: 'error', id: 'old-unregister-error', payload: { code: 'web_debug_page_not_registered', message: 'old unregister', request_id: oldUnregisterOperationID } })
    acknowledge(transport)
    transport.emit({ version: 1, type: 'error', id: 'old-focus-error', payload: { code: 'web_debug_page_not_registered', message: 'old focus', request_id: oldFocusOperationID } })
    expect(window.__SAI_DEBUG__).toBeDefined()
    application.replica.applySnapshot(
      { type: 'session_index', id: WEB_DEBUG_TARGET_PROJECT_ID },
      new SessionIndexAdapter(WEB_DEBUG_TARGET_PROJECT_ID),
      { sessions: [] },
      { streamEpoch: 'test', sequence: '2' as never, resourceRevision: '2', generation: 0 },
    )
    expect(window.__SAI_DEBUG__).toBeUndefined()
    expect(transport.count('debug_unregister')).toBe(2)

    application.signals.currentProject.set(WEB_DEBUG_TARGET_PROJECT_ID)
    application.signals.currentSession.set('session_a')
    seedAuthority(application)
    expect(transport.count('debug_register')).toBe(3)
    bridge.stop()
    expect(window.__SAI_DEBUG__).toBeUndefined()
    expect(transport.count('debug_unregister')).toBe(3)
    bridge.start()
    expect(transport.count('debug_register')).toBe(4)
    bridge.dispose()
    expect(window.__SAI_DEBUG__).toBeUndefined()
    application.dispose()
  })

  it('rejects stale acknowledgements and re-registers once per reconnect generation', () => {
    const { bridge, application, transport } = createBridge()
    bridge.start()
    transport.emit({ version: 1, type: 'debug_registered', id: 'stale', payload: { page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'other-session', focused: false } })
    expect(window.__SAI_DEBUG__).toBeUndefined()
    expect(transport.count('debug_unregister')).toBe(1)
    transport.emit({ version: 1, type: 'error', id: 'stale-unregister-error', payload: { code: 'web_debug_page_not_registered', message: 'not registered' } })
    acknowledge(transport)
    expect(window.__SAI_DEBUG__).toBeDefined()
    transport.reconnect()
    expect(transport.count('debug_register')).toBe(2)
    acknowledge(transport)
    const surface = window.__SAI_DEBUG__
    expect(surface).toBeDefined()
    transport.reconnect()
    expect(window.__SAI_DEBUG__).toBeUndefined()
    expect(transport.count('debug_register')).toBe(3)
    acknowledge(transport)
    expect(window.__SAI_DEBUG__).toBe(surface)
    cleanUp(bridge, application)
  })

  it('selects only authoritative projects/sessions and keeps old surface references usable', async () => {
    const otherProject = { ...targetProject, id: 'other-project', root: '/workspace/other' }
    const application = createSyncApplication({ transport: new FakeTransport() })
    seedAuthority(application, [targetProject, otherProject], [session('session_a', WEB_DEBUG_TARGET_PROJECT_ID)])
    application.replica.applySnapshot(
      { type: 'session_index', id: otherProject.id },
      new SessionIndexAdapter(otherProject.id),
      { sessions: [session('session_b', otherProject.id)] },
      { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
    )
    application.signals.currentProject.set(WEB_DEBUG_TARGET_PROJECT_ID)
    application.signals.currentSession.set('session_a')
    const transport = new FakeTransport()
    const bridge = new WebDebugBridge({
      transport,
      runtime: {} as SyncRuntime,
      replica: application.replica,
      repositories: application.repositories,
      commandFacade: {} as CommandFacade,
      signals: application.signals,
      window,
      pageIDGenerator: () => 'page-id',
      pageEpochGenerator: () => 'page-epoch',
    })
    bridge.start()
    acknowledge(transport)
    const surface = window.__SAI_DEBUG__!
    await expect(surface.selectProject('missing')).rejects.toMatchObject({ code: 'project_not_found' })
    await expect(surface.selectSession('missing')).rejects.toMatchObject({ code: 'session_not_found' })
    await surface.selectProject(otherProject.id)
    expect(application.signals.currentProject.get()).toBe(otherProject.id)
    expect(application.signals.currentSession.get()).toBeNull()
    expect(window.__SAI_DEBUG__).toBeUndefined()
    await surface.selectSession('session_b')
    expect(application.signals.currentProject.get()).toBe(otherProject.id)
    expect(application.signals.currentSession.get()).toBe('session_b')
    expect(surface.appState.currentSession).toBe('session_b')
    expect(WebDebugSelectionError).toBeDefined()
    cleanUp(bridge, application)
  })

  it('waitIdle observes command/runtime/repository state, supports cancellation and is bounded', async () => {
    let busy = true
    let pending = 1
    const setup = createBridge({ busy: () => busy, pending: () => pending, contentReady: false })
    setup.bridge.start()
    acknowledge(setup.transport)
    const surface = window.__SAI_DEBUG__!
    const waiting = surface.waitIdle({ timeoutMS: 200 })
    await new Promise((resolve) => setTimeout(resolve, 15))
    busy = false
    pending = 0
    let settled = false
    void waiting.then(() => { settled = true })
    await new Promise((resolve) => setTimeout(resolve, 15))
    expect(settled).toBe(false)
    seedSessionContent(setup.application)
    await expect(waiting).resolves.toBeUndefined()
    setup.application.replica.markStale(
      { type: 'session_content', id: 'session_a' },
      new SyncReadError('transport', 'stale diagnostic'),
    )
    await expect(surface.waitIdle({ timeoutMS: 100 })).resolves.toBeUndefined()

    busy = true
    await expect(surface.waitIdle({ timeoutMS: 20 })).rejects.toMatchObject({ code: 'timeout' })
    const controller = new AbortController()
    const cancelled = surface.waitIdle({ timeoutMS: 100, signal: controller.signal })
    controller.abort()
    await expect(cancelled).rejects.toBeInstanceOf(WebDebugIdleError)
    const stopped = surface.waitIdle({ timeoutMS: 500 })
    setup.bridge.stop()
    await expect(stopped).rejects.toMatchObject({ code: 'stopped' })
    setup.bridge.start()
    acknowledge(setup.transport)
    const disposed = window.__SAI_DEBUG__!.waitIdle({ timeoutMS: 500 })
    setup.bridge.dispose()
    await expect(disposed).rejects.toMatchObject({ code: 'disposed' })
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('treats initialized stale and terminal error session content as observable idle states', async () => {
    const setup = createBridge({ contentReady: false })
    setup.bridge.start()
    acknowledge(setup.transport)
    setup.application.replica.markError(
      { type: 'session_content', id: 'session_a' },
      new Error('content unavailable'),
    )
    await expect(window.__SAI_DEBUG__!.waitIdle({ timeoutMS: 100 })).resolves.toBeUndefined()
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('removes every listener on stop and does not duplicate messages across start/stop', () => {
    const { bridge, application, transport } = createBridge()
    bridge.start()
    expect(transport.count('debug_register')).toBe(1)
    bridge.stop()
    bridge.stop()
    bridge.start()
    bridge.start()
    expect(transport.count('debug_register')).toBe(2)
    acknowledge(transport)
    bridge.stop()
    expect(transport.count('debug_unregister')).toBe(2)
    cleanUp(bridge, application)
  })

  it('keeps debug control errors out of runtime and command handling', () => {
    const transport = new FakeTransport()
    const application = createSyncApplication({ transport })
    application.start()
    const subscribe = transport.last('subscribe')
    expect(subscribe?.type).toBe('subscribe')
    if (subscribe?.type !== 'subscribe') throw new Error('subscription was not sent')
    transport.emit({
      version: 1,
      type: 'error',
      id: 'debug-error',
      payload: { code: 'web_debug_disabled', message: 'disabled', request_id: subscribe.id },
    })
    expect(application.replica.get({ type: 'project_index', id: 'server' }).metadata.error).toBeUndefined()
    expect(application.runtime.getDebugSnapshot()).toMatchObject({ activeSubscriptions: 1 })
    expect(application.commandFacade.getDebugSnapshot()).toMatchObject({ started: true, pendingCount: 0 })
    expect('runtime' in application.page).toBe(false)
    expect('replica' in application.page).toBe(false)
    expect('transport' in application.page).toBe(false)
    expect('debugBridge' in application.page).toBe(false)
    application.dispose()
  })

  it('executes expression and async completion values with a typed inline result', async () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-1',
      payload: {
        execution_id: 'execution-1', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'await Promise.resolve({ answer: 42 })', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.last('debug_execution_result')).toBeDefined())
    const result = setup.transport.last('debug_execution_result')
    expect(result?.type).toBe('debug_execution_result')
    if (result?.type !== 'debug_execution_result') throw new Error('execution result was not sent')
    expect(result.payload).toMatchObject({ execution_id: 'execution-1', status: 'succeeded', value: { answer: 42 } })

    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-2',
      payload: {
        execution_id: 'execution-2', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'const value = 6; return value * 7', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(2))
    const second = setup.transport.last('debug_execution_result')
    if (second?.type !== 'debug_execution_result') throw new Error('second execution result was not sent')
    expect(second.payload.value).toBe(42)
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('captures console output, restores console, and returns typed throws', async () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    const originalLog = window.console.log
    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-console',
      payload: {
        execution_id: 'execution-console', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'console.log("hello", 7); return 1', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(1))
    const success = setup.transport.last('debug_execution_result')
    if (success?.type !== 'debug_execution_result') throw new Error('console execution result was not sent')
    expect(success.payload.console?.[0]).toMatchObject({ level: 'log', arguments: ['hello', 7] })
    expect(success.payload.value).toBe(1)
    expect(window.console.log).toBe(originalLog)

    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-overwrite-success',
      payload: {
        execution_id: 'execution-overwrite-success', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'console.log = () => {}; return 2', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(2))
    expect(window.console.log).toBe(originalLog)

    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-throw',
      payload: {
        execution_id: 'execution-throw', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'throw new Error("boom")', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(3))
    const failure = setup.transport.last('debug_execution_result')
    if (failure?.type !== 'debug_execution_result') throw new Error('throw result was not sent')
    expect(failure.payload).toMatchObject({ status: 'failed', error: { code: 'web_debug_execution_error', message: 'boom' } })
    expect(window.console.log).toBe(originalLog)

    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-overwrite-throw',
      payload: {
        execution_id: 'execution-overwrite-throw', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'console.log = () => {}; throw new Error("overwrite boom")', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(4))
    expect(window.console.log).toBe(originalLog)

    const longMessage = '🦄'.repeat(5_000)
    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-long-error',
      payload: {
        execution_id: 'execution-long-error', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: `throw new Error(${JSON.stringify(longMessage)})`, timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(5))
    const longFailure = setup.transport.last('debug_execution_result')
    if (longFailure?.type !== 'debug_execution_result') throw new Error('long error result was not sent')
    expect(longFailure.payload.status).toBe('failed')
    expect(longFailure.payload.error).toBeDefined()
    expect(new TextEncoder().encode(longFailure.payload.error?.message ?? '').byteLength).toBeLessThanOrEqual(4096)
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('returns a browser timeout for async completion and restores console hooks', async () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    const originalWarn = window.console.warn
    const late = window as unknown as { __saiDebugResolveLate?: () => void }
    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-timeout',
      payload: {
        execution_id: 'execution-timeout', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'console.warn("before timeout"); await new Promise(resolve => { window.__saiDebugResolveLate = resolve })', timeout_ms: 100,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(1), { timeout: 1000 })
    const result = setup.transport.last('debug_execution_result')
    if (result?.type !== 'debug_execution_result') throw new Error('timeout result was not sent')
    expect(result.payload).toMatchObject({
      execution_id: 'execution-timeout', status: 'failed', error: { code: 'web_debug_timeout' },
    })
    expect(window.console.warn).toBe(originalWarn)
    expect(setup.transport.count('debug_execution_result')).toBe(1)

    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-after-timeout',
      payload: {
        execution_id: 'execution-after-timeout', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'return 7', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(2))
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { execution_id: 'execution-after-timeout', status: 'succeeded', value: 7 },
    })
    late.__saiDebugResolveLate?.()
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(setup.transport.count('debug_execution_result')).toBe(2)
    delete late.__saiDebugResolveLate
    setup.bridge.dispose()
    expect(setup.transport.count('debug_execution_result')).toBe(2)
    setup.application.dispose()
  })

  it('returns a typed stale result for an identity that is not the current registration', async () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-stale',
      payload: {
        execution_id: 'execution-stale', page_id: 'other-page', page_epoch: 'page-epoch', session_id: 'session_a',
        code: '1 + 1', timeout_ms: 500,
      },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(1))
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { execution_id: 'execution-stale', status: 'failed', error: { code: 'web_debug_executor_stale' } },
    })
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('validates result frames locally, uses one minimal fallback, and never retries transport failure', () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    const sendResult = (setup.bridge as unknown as {
      sendExecutionResult: (request: {
        execution_id: string
        page_id: string
        page_epoch: string
        session_id: string
      }, generation: number, result: DebugExecutionResultPayload) => void
    }).sendExecutionResult.bind(setup.bridge)
    const request = { execution_id: 'execution-local', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a' }
    const beforeSuccess = setup.transport.sendAttempts
    sendResult(request, setup.transport.connectionGeneration, {
      ...request,
      status: 'succeeded',
      value: 'x'.repeat(70 * 1024),
    })
    expect(setup.transport.sendAttempts - beforeSuccess).toBe(1)
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { status: 'succeeded', value: { __sai_debug: 'summary', reason: 'budget' } },
    })

    const beforeInvalidSuccess = setup.transport.sendAttempts
    sendResult(request, setup.transport.connectionGeneration, {
      ...request,
      status: 'succeeded',
    } as unknown as DebugExecutionResultPayload)
    expect(setup.transport.sendAttempts - beforeInvalidSuccess).toBe(1)
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { status: 'succeeded', value: { __sai_debug: 'summary' } },
    })

    const beforeFailed = setup.transport.sendAttempts
    sendResult(request, setup.transport.connectionGeneration, {
      ...request,
      status: 'failed',
      value: null,
    } as unknown as DebugExecutionResultPayload)
    expect(setup.transport.sendAttempts - beforeFailed).toBe(1)
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { status: 'failed', error: { code: 'web_debug_serializer_error' } },
    })

    const beforeFailedBudget = setup.transport.sendAttempts
    sendResult(request, setup.transport.connectionGeneration, {
      ...request,
      status: 'failed',
      error: { code: 'web_debug_execution_error', message: 'x'.repeat(70 * 1024) },
    })
    expect(setup.transport.sendAttempts - beforeFailedBudget).toBe(1)
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { status: 'failed', error: { code: 'web_debug_serializer_error' } },
    })

    setup.transport.sendError = new Error('socket write failed')
    const beforeTransportFailure = setup.transport.sendAttempts
    sendResult(request, setup.transport.connectionGeneration, {
      ...request,
      status: 'succeeded',
      value: 1,
    })
    expect(setup.transport.sendAttempts - beforeTransportFailure).toBe(1)
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('invalidates an active execution on transport close without a late replay', async () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    setup.transport.emit({
      version: 1,
      type: 'debug_execute',
      id: 'execute-close',
      payload: {
        execution_id: 'execution-close', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'await new Promise(() => {})', timeout_ms: 1000,
      },
    })
    setup.transport.close(false)
    await new Promise((resolve) => setTimeout(resolve, 20))
    // The socket is already unavailable, so the bridge cannot safely send the
    // disconnected result; it must still suppress the late promise.
    expect(setup.transport.count('debug_execution_result')).toBe(0)
    setup.bridge.dispose()
    setup.application.dispose()
  })

  it('enforces one browser execution, async timeout, identity checks, and no late replay', async () => {
    const setup = createBridge()
    setup.bridge.start()
    acknowledge(setup.transport)
    const never = {
      version: 1 as const,
      type: 'debug_execute' as const,
      id: 'execute-never',
      payload: {
        execution_id: 'execution-never', page_id: 'page-id', page_epoch: 'page-epoch', session_id: 'session_a',
        code: 'await new Promise(() => {})', timeout_ms: 1000,
      },
    }
    setup.transport.emit(never)
    setup.transport.emit({
      ...never,
      id: 'execute-busy',
      payload: { ...never.payload, execution_id: 'execution-busy' },
    })
    await vi.waitFor(() => expect(setup.transport.count('debug_execution_result')).toBe(1))
    const busy = setup.transport.last('debug_execution_result')
    if (busy?.type !== 'debug_execution_result') throw new Error('busy result was not sent')
    expect(busy.payload).toMatchObject({ execution_id: 'execution-busy', status: 'failed', error: { code: 'web_debug_busy' } })

    const resultCountBeforeStop = setup.transport.count('debug_execution_result')
    setup.bridge.stop()
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(setup.transport.count('debug_execution_result')).toBe(resultCountBeforeStop + 1)
    // The only post-stop result is the one best-effort stale/disconnected
    // response; the unresolved promise cannot send a second result.
    expect(setup.transport.last('debug_execution_result')).toMatchObject({
      type: 'debug_execution_result',
      payload: { execution_id: 'execution-never', status: 'failed', error: { code: 'web_debug_disconnected' } },
    })
    setup.application.dispose()
  })
})
