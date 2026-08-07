// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { frontendProtocolLogger } from './lib/frontendProtocolLogger'
import { SyncApplicationProvider } from './applicationContext'
import { createSyncApplication } from './sync/applicationComposition'
import { ProjectIndexAdapter } from './sync/projectIndexAdapter'
import type { SessionSummary } from './sync/sessionIndexAdapter'
import { SessionIndexAdapter } from './sync/sessionIndexAdapter'
import type { ProjectSummary } from './repositories/projectIndex'
import type { ProtocolMessage, JsonValue } from './protocol/types'
import type { RuntimeTransport } from './sync/runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './sync/transport'

vi.mock('react-virtuoso', async () => {
  const React = await import('react')
  const MockVirtuoso = React.forwardRef<Record<string, unknown>, Record<string, any>>(function MockVirtuoso(props, ref) {
    const scrollerRef = React.useRef<HTMLElement | null>(null)
    const handle = React.useMemo(() => ({
      getState: (callback: (state: unknown) => void) => callback({ ranges: [], scrollTop: 0 }),
      scrollBy: () => {},
      scrollIntoView: () => {},
      scrollTo: () => {},
      scrollToIndex: () => {},
    }), [])
    React.useImperativeHandle(ref, () => handle, [handle])
    React.useLayoutEffect(() => {
      props.scrollerRef?.(scrollerRef.current)
      props.itemsRendered?.((props.data ?? []).map((data: unknown, index: number) => ({ data, index, offset: index * 80, size: 80 })))
      props.rangeChanged?.({ startIndex: props.firstItemIndex })
      props.totalListHeightChanged?.()
      return () => props.scrollerRef?.(null)
    }, [props.data, props.firstItemIndex, props.itemsRendered, props.rangeChanged, props.scrollerRef, props.totalListHeightChanged])

    const Scroller = props.components?.Scroller ?? 'div'
    const Header = props.components?.Header
    const Footer = props.components?.Footer
    const List = props.components?.List ?? 'div'
    return React.createElement(
      Scroller,
      { ref: scrollerRef, 'data-testid': 'mock-app-scroller' },
      Header ? React.createElement(Header) : null,
      React.createElement(List, null, (props.data ?? []).map((row: unknown, index: number) =>
        React.createElement(React.Fragment, { key: props.computeItemKey?.(index, row) ?? index }, props.itemContent?.(index, row)))),
      Footer ? React.createElement(Footer) : null,
    )
  })
  return { Virtuoso: MockVirtuoso }
})

const mocks = vi.hoisted(() => {
  const session = {
    id: 'session-1',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    display_name: 'session-1',
    created_by: 'user',
    root_session_id: 'session-1',
    spawn_depth: 0,
    archived: false,
    last_used_at: '2026-01-01T00:00:00Z',
    provider: 'fake',
    model_profile: 'default',
    model_id: 'fake-model',
    project_id: 'project-1',
    created_cwd: '/workspace',
    last_seq: 0,
    full_access: false,
    cwd: '/workspace/src',
    config_path: '/config',
    reasoning_level: 'medium',
  }
  const api = {
    bootstrap: vi.fn().mockResolvedValue({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' }),
    projects: vi.fn().mockResolvedValue({ projects: [{ id: 'project-1', root: '/workspace', display_name: 'project', archived: false, created_at: '', updated_at: '' }] }),
    activeRuns: vi.fn().mockResolvedValue({ runs: [{ run_id: 'run-1', session_id: 'session-1', turn_id: 'turn-1', started_at: '', status: 'running' }] }),
    sessions: vi.fn().mockResolvedValue({ sessions: [session] }),
    session: vi.fn().mockResolvedValue(session),
    snapshot: vi.fn().mockImplementation(() => new Promise(() => {})),
    createSession: vi.fn(),
    startRun: vi.fn(),
    appendRunMessage: vi.fn(),
    cancelRun: vi.fn(),
    continueRun: vi.fn(),
  }
  const streamLifecycle = vi.fn((_onEvent: unknown, options: { signal?: AbortSignal }) => new Promise<void>((resolve) => {
    options.signal?.addEventListener('abort', () => resolve(), { once: true })
  }))
  return { api, session, streamLifecycle, streamRun: vi.fn().mockResolvedValue(undefined) }
})

vi.mock('./api', () => ({
  api: mocks.api,
  streamLifecycle: mocks.streamLifecycle,
  streamRun: mocks.streamRun,
}))

class AppTestTransport implements RuntimeTransport {
  isReady = false
  connectionGeneration = 0
  serverEpoch = 'test-epoch'
  sent: ProtocolMessage[] = []
  startCalls = 0
  onSend?: (message: ProtocolMessage) => void
  private readonly messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private readonly ready = new Set<(event: TransportReadyEvent) => void>()
  private readonly closed = new Set<(event: TransportCloseEvent) => void>()

  start(): void {
    this.startCalls += 1
    this.isReady = true
    this.connectionGeneration += 1
    const event: TransportReadyEvent = {
      generation: this.connectionGeneration,
      serverEpoch: this.serverEpoch,
      connectionID: `test-connection-${this.connectionGeneration}`,
      heartbeatIntervalMS: 15_000,
      maxMessageBytes: 256 * 1024,
    }
    for (const listener of [...this.ready]) listener(event)
  }

  stop(): void {
    if (!this.isReady) return
    this.isReady = false
    const event: TransportCloseEvent = { generation: this.connectionGeneration, willRetry: false }
    for (const listener of [...this.closed]) listener(event)
  }

  send(message: ProtocolMessage): void {
    this.sent.push(message)
    this.onSend?.(message)
  }
  emit(message: ProtocolMessage): void {
    for (const listener of [...this.messages]) listener(message, this.connectionGeneration)
  }
  emitClose(willRetry = true): void {
    this.isReady = false
    for (const listener of [...this.closed]) listener({ generation: this.connectionGeneration, willRetry })
  }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.closed.add(listener); return () => this.closed.delete(listener) }
}

const testApplications = new Set<ReturnType<typeof createSyncApplication>>()

const defaultProject: ProjectSummary = {
  id: 'project-1',
  root: '/workspace',
  display_name: 'project',
  archived: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

function renderApp(projects: readonly ProjectSummary[] = [defaultProject]) {
  const transport = new AppTestTransport()
  const application = createSyncApplication({ transport })
  const adapter = new ProjectIndexAdapter()
  application.replica.applySnapshot(
    { type: 'project_index', id: 'server' },
    adapter,
    { projects: [...projects] },
    { streamEpoch: 'test', sequence: '0' as never, resourceRevision: '0', generation: 0 },
  )
  for (const project of projects) {
    const sessionAdapter = new SessionIndexAdapter(project.id)
    const sessions = project.id === 'project-1' ? [{
      session_id: mocks.session.id,
      project_id: project.id,
      parent_session_id: null,
      display_name: mocks.session.display_name,
      archived: false,
      status: 'idle' as const,
      run_id: null,
      resource_revision: '0',
      updated_at: mocks.session.updated_at,
      has_unread_result: false,
    }] : []
    application.replica.applySnapshot(
      { type: 'session_index', id: project.id },
      sessionAdapter,
      { sessions },
      { streamEpoch: 'test', sequence: '0' as never, resourceRevision: '0', generation: 0 },
    )
  }
  testApplications.add(application)
  const view = render(
    <SyncApplicationProvider application={application}>
      <App />
    </SyncApplicationProvider>,
  )
  return Object.assign(view, { application, transport })
}

function respondToProjectCommand(transport: AppTestTransport, message: ProtocolMessage, result: unknown): void {
  if (message.type !== 'command') return
  transport.emit({
    version: 1,
    type: 'command_result',
    id: `result-${message.payload.request_id}`,
    payload: { request_id: message.payload.request_id, status: 'succeeded', result },
  } as unknown as ProtocolMessage)
}

function failProjectCommand(transport: AppTestTransport, message: ProtocolMessage, code = 'permission_denied'): void {
  if (message.type !== 'command') return
  transport.emit({
    version: 1,
    type: 'command_result',
    id: `result-${message.payload.request_id}`,
    payload: { request_id: message.payload.request_id, status: 'failed', error: { code, message: 'protocol-secret-not-for-ui' } },
  } as unknown as ProtocolMessage)
}

function applySessionIndexAuthority(view: { application: ReturnType<typeof createSyncApplication> }, summary: SessionSummary, sequence = '1'): void {
  const adapter = new SessionIndexAdapter(summary.project_id)
  view.application.replica.applyChange(
    { type: 'session_index', id: summary.project_id },
    adapter,
    [{ op: 'upsert', key: summary.session_id, value: summary as unknown as JsonValue }],
    { streamEpoch: 'test', sequence: sequence as never, resourceRevision: summary.resource_revision, generation: 0 },
  )
}

function applySessionCreateAuthority(view: { application: ReturnType<typeof createSyncApplication> }, sessionID: string): void {
  applySessionIndexAuthority(view, {
    session_id: sessionID,
    project_id: 'project-1',
    parent_session_id: null,
    display_name: 'new root',
    archived: false,
    status: 'idle',
    run_id: null,
    resource_revision: '1',
    updated_at: '2026-01-02T00:00:00Z',
    has_unread_result: false,
  })
}

function respondToSessionCreate(view: { application: ReturnType<typeof createSyncApplication>; transport: AppTestTransport }, message: ProtocolMessage): void {
  if (message.type !== 'command' || message.payload.name !== 'session.create') return
  const sessionID = String(message.payload.arguments.session_id ?? 'session-new')
  view.transport.emit({
    version: 1,
    type: 'command_result',
    id: `result-${message.payload.request_id}`,
    payload: { request_id: message.payload.request_id, status: 'succeeded', result: { session_id: sessionID, project_id: 'project-1' } },
  } as unknown as ProtocolMessage)
  applySessionCreateAuthority(view, sessionID)
}

describe('App lifecycle bootstrap', () => {
  function resetApiMocks() {
    mocks.api.bootstrap.mockReset().mockResolvedValue({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' })
    mocks.api.projects.mockReset().mockResolvedValue({ projects: [{ id: 'project-1', root: '/workspace', display_name: 'project', archived: false, created_at: '', updated_at: '' }] })
    mocks.api.activeRuns.mockReset().mockResolvedValue({ runs: [{ run_id: 'run-1', session_id: 'session-1', turn_id: 'turn-1', started_at: '', status: 'running' }] })
    mocks.api.sessions.mockReset().mockResolvedValue({ sessions: [mocks.session] })
    mocks.api.session.mockReset().mockResolvedValue(mocks.session)
    mocks.api.snapshot.mockReset().mockImplementation(() => new Promise(() => {}))
    mocks.api.createSession.mockReset()
    mocks.api.startRun.mockReset()
    mocks.api.appendRunMessage.mockReset()
    mocks.api.cancelRun.mockReset()
    mocks.api.continueRun.mockReset()
    mocks.streamRun.mockReset().mockResolvedValue(undefined)
  }

  beforeEach(() => {
    resetApiMocks()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
    vi.restoreAllMocks()
    frontendProtocolLogger.resetForTesting()
    for (const application of testApplications) application.dispose()
    testApplications.clear()
  })

  it('does not poll legacy session REST or active runs while a run remains active', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())

    vi.useFakeTimers()
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(mocks.api.activeRuns).toHaveBeenCalledTimes(1)
    expect(mocks.api.sessions).not.toHaveBeenCalled()
    view.unmount()
  })

  it('does not bootstrap or reload active runs when Session Index changes', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    expect(mocks.api.bootstrap).toHaveBeenCalledTimes(1)
    expect(mocks.api.activeRuns).toHaveBeenCalledTimes(1)

    const running: SessionSummary = {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: false, status: 'running', run_id: 'background-run', resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    }
    const completed = { ...running, status: 'completed' as const, resource_revision: '2', has_unread_result: true }
    await act(async () => { applySessionIndexAuthority(view, running, '1') })
    await act(async () => { applySessionIndexAuthority(view, completed, '2') })

    await waitFor(() => expect(screen.getByLabelText('Unread result')).toBeTruthy())
    expect(mocks.api.bootstrap).toHaveBeenCalledTimes(1)
    expect(mocks.api.activeRuns).toHaveBeenCalledTimes(1)
    expect(mocks.api.sessions).not.toHaveBeenCalled()
    view.unmount()
  })

  it('offers a retry for cached project data after terminal transport failure', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('project')).toBeTruthy())
    const subscribeCountBeforeClose = view.transport.sent.filter((message) => message.type === 'subscribe' && message.payload.resource.type === 'project_index').length
    const startCallsBeforeRetry = view.transport.startCalls

    view.transport.emitClose(false)
    await waitFor(() => expect(screen.getByRole('button', { name: 'Retry project synchronization' })).toBeTruthy())
    expect(view.application.repositories.projectIndex.getSnapshot().status).toBe('stale')

    fireEvent.click(screen.getByRole('button', { name: 'Retry project synchronization' }))
    await waitFor(() => expect(view.transport.startCalls).toBe(startCallsBeforeRetry + 1))
    await waitFor(() => expect(view.transport.sent.filter((message) => message.type === 'subscribe' && message.payload.resource.type === 'project_index').length).toBeGreaterThan(subscribeCountBeforeClose))
    view.unmount()
  })

  it('keeps project navigation on the authoritative index and selects the next project after removal', async () => {
    const projects: ProjectSummary[] = [
      defaultProject,
      { ...defaultProject, id: 'project-2', root: '/workspace/project-2', display_name: 'project two', created_at: '2026-01-02T00:00:00Z' },
      { ...defaultProject, id: 'project-3', root: '/workspace/project-3', display_name: 'project three', created_at: '2026-01-03T00:00:00Z' },
    ]
    const view = renderApp(projects)
    await waitFor(() => expect(screen.getByText('project two')).toBeTruthy())

    fireEvent.click(screen.getByText('project two'))
    await waitFor(() => expect(view.application.signals.currentProject.get()).toBe('project-2'))

    const adapter = new ProjectIndexAdapter()
    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...projects[1], display_name: 'project two renamed', updated_at: '2026-01-04T00:00:00Z' } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 1 },
      )
    })
    await waitFor(() => expect(screen.getByText('project two renamed')).toBeTruthy())
    expect(view.application.signals.currentProject.get()).toBe('project-2')

    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'remove', key: 'project-2' }],
        { streamEpoch: 'test', sequence: '2' as never, resourceRevision: '2', generation: 1 },
      )
    })

    await waitFor(() => {
      const selectedProjectHeader = screen.getByText('project three').closest('.project-tree-header')
      expect(selectedProjectHeader?.className).toContain('selected')
      expect(view.application.signals.currentProject.get()).toBe('project-3')
    })
    expect(screen.queryByText('project two')).toBeNull()
    // Project index IDs, not the legacy project endpoint, enumerate project
    // navigation; each project's sessions come from Session Index.
    expect(mocks.api.projects).not.toHaveBeenCalled()
    view.unmount()
  })

  it('shows only active projects and selects deterministically across archive and restore changes', async () => {
    const projects: ProjectSummary[] = [
      defaultProject,
      { ...defaultProject, id: 'project-2', root: '/workspace/project-2', display_name: 'project two', created_at: '2026-01-02T00:00:00Z' },
      { ...defaultProject, id: 'project-3', root: '/workspace/project-3', display_name: 'project three', created_at: '2026-01-03T00:00:00Z' },
    ]
    const view = renderApp(projects)
    await waitFor(() => expect(screen.getByText('project two')).toBeTruthy())
    fireEvent.click(screen.getByText('project two'))
    await waitFor(() => expect(view.application.signals.currentProject.get()).toBe('project-2'))
    const adapter = new ProjectIndexAdapter()

    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...projects[1], archived: true, updated_at: '2026-01-04T00:00:00Z' } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 1 },
      )
    })
    await waitFor(() => {
      expect(screen.queryByText('project two')).toBeNull()
      expect(view.application.signals.currentProject.get()).toBe('project-3')
    })

    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...projects[1], archived: false, updated_at: '2026-01-05T00:00:00Z' } }],
        { streamEpoch: 'test', sequence: '2' as never, resourceRevision: '2', generation: 1 },
      )
    })
    await waitFor(() => expect(screen.getByText('project two')).toBeTruthy())
    expect(view.application.signals.currentProject.get()).toBe('project-3')
    view.unmount()
  })

  it('subscribes the Session Index for a project added by the authoritative active index', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('project')).toBeTruthy())
    const adapter = new ProjectIndexAdapter()
    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...defaultProject, id: 'project-2', root: '/workspace/project-2', display_name: 'arrived project' } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
      )
    })
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === 'subscribe' && message.payload.resource.type === 'session_index' && message.payload.resource.id === 'project-2')).toBe(true))
    expect(mocks.api.sessions).not.toHaveBeenCalled()
    expect(mocks.api.projects).not.toHaveBeenCalled()
    view.unmount()
  })

  it('does not let an empty-state project form hide a project arriving from the authority, or close a manual form', async () => {
    const emptyView = renderApp([])
    await screen.findByText('Connect your first project')
    const adapter = new ProjectIndexAdapter()
    await act(async () => {
      emptyView.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-1', value: { ...defaultProject } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
      )
    })
    await waitFor(() => {
      expect(screen.getByText('project')).toBeTruthy()
      expect(screen.queryByText('Connect your first project')).toBeNull()
    })
    emptyView.unmount()

    const manualView = renderApp()
    await waitFor(() => expect(screen.getByText('project')).toBeTruthy())
    fireEvent.click(screen.getByRole('button', { name: 'Add project' }))
    await screen.findByText('Add another project')
    await act(async () => {
      manualView.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...defaultProject, id: 'project-2', display_name: 'unrelated project' } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
      )
    })
    await waitFor(() => expect(screen.getByText('Add another project')).toBeTruthy())
    manualView.unmount()
  })

  it('keeps create and rename command acknowledgements separate from project authority changes', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('project')).toBeTruthy())
    view.transport.onSend = (message) => {
      if (message.type !== 'command') return
      if (message.payload.name === 'project.create') {
        respondToProjectCommand(view.transport, message, { operation_id: message.payload.arguments.operation_id, project_id: 'project-2', created: true })
      }
    }
    fireEvent.click(screen.getByRole('button', { name: 'Add project' }))
    fireEvent.change(screen.getByLabelText('Project directory'), { target: { value: '/workspace/project-2' } })
    fireEvent.change(screen.getByLabelText(/Display name/), { target: { value: 'new project' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect project' }))
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'project.create')).toBe(true))
    await Promise.resolve()
    expect(screen.queryByText('new project')).toBeNull()
    expect(view.application.repositories.projectIndex.getSnapshot().active.map((project) => project.id)).toEqual(['project-1'])

    const adapter = new ProjectIndexAdapter()
    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...defaultProject, id: 'project-2', root: '/workspace/project-2', display_name: 'new project' } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
      )
    })
    await waitFor(() => expect(screen.getByText('new project')).toBeTruthy())

    vi.spyOn(window, 'prompt').mockReturnValue('renamed project')
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'project.rename') {
        respondToProjectCommand(view.transport, message, { project_id: 'project-2', display_name: 'renamed project' })
      }
    }
    fireEvent.click(screen.getByRole('button', { name: 'Rename new project' }))
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'project.rename')).toBe(true))
    await Promise.resolve()
    expect(screen.getByText('new project')).toBeTruthy()
    // The public repository intentionally has no mutation-through-result API;
    // the underlying replica still has the old authoritative name.
    expect(view.application.repositories.projectIndex.getSnapshot().active.find((project) => project.id === 'project-2')?.display_name).toBe('new project')

    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-2', value: { ...defaultProject, id: 'project-2', root: '/workspace/project-2', display_name: 'renamed project', updated_at: '2026-01-02T00:00:00Z' } }],
        { streamEpoch: 'test', sequence: '2' as never, resourceRevision: '2', generation: 0 },
      )
    })
    await waitFor(() => expect(screen.getByText('renamed project')).toBeTruthy())
    view.unmount()
  })

  it('keeps session command acknowledgements separate from active/archive/restore/remove authority', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    const authority = (overrides: Partial<SessionSummary> = {}): SessionSummary => ({
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: false, status: 'idle', run_id: null, resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: false, ...overrides,
    })
    const commands: string[] = []
    view.transport.onSend = (message) => {
      if (message.type !== 'command') return
      commands.push(message.payload.name)
      if (message.payload.name === 'session.rename') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', display_name: 'renamed session' })
      } else if (message.payload.name === 'session.archive') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', archived: true })
      } else if (message.payload.name === 'session.restore') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', archived: false })
      } else if (message.payload.name === 'session.delete') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', status: 'removed', removed_sessions: 1 })
      }
    }
    vi.spyOn(window, 'prompt').mockReturnValue('renamed session')
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    fireEvent.click(screen.getByRole('button', { name: 'Rename session-1' }))
    await waitFor(() => expect(commands).toContain('session.rename'))
    expect(screen.getByText('session-1')).toBeTruthy()
    await act(async () => { applySessionIndexAuthority(view, authority({ display_name: 'renamed session', resource_revision: '2' }), '2') })
    await waitFor(() => expect(screen.getByText('renamed session')).toBeTruthy())

    fireEvent.click(screen.getByRole('button', { name: 'Archive renamed session' }))
    await waitFor(() => expect(commands).toContain('session.archive'))
    expect(screen.getByRole('button', { name: 'Archive renamed session' })).toBeTruthy()
    await act(async () => { applySessionIndexAuthority(view, authority({ display_name: 'renamed session', archived: true, resource_revision: '3' }), '3') })
    const archivedToggle = await screen.findByRole('button', { name: /Archived \(1\)/ })
    fireEvent.click(archivedToggle)
    fireEvent.click(screen.getByRole('button', { name: 'Restore renamed session' }))
    await waitFor(() => expect(commands).toContain('session.restore'))
    expect(screen.getByRole('button', { name: 'Restore renamed session' })).toBeTruthy()
    await act(async () => { applySessionIndexAuthority(view, authority({ display_name: 'renamed session', resource_revision: '4' }), '4') })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Restore renamed session' })).toBeNull())

    fireEvent.click(screen.getByRole('button', { name: 'Delete renamed session' }))
    await waitFor(() => expect(commands).toEqual(['session.rename', 'session.archive', 'session.restore', 'session.archive']))
    expect(screen.getByRole('button', { name: 'Archive renamed session' })).toBeTruthy()
    expect(commands).not.toContain('session.delete')
    await act(async () => { applySessionIndexAuthority(view, authority({ display_name: 'renamed session', archived: true, resource_revision: '5' }), '5') })
    await waitFor(() => expect(commands).toEqual(['session.rename', 'session.archive', 'session.restore', 'session.archive', 'session.delete']))
    expect(screen.getByText('renamed session')).toBeTruthy()
    await act(async () => {
      view.application.replica.applyChange(
        { type: 'session_index', id: 'project-1' },
        new SessionIndexAdapter('project-1'),
        [{ op: 'remove', key: 'session-1' }],
        { streamEpoch: 'test', sequence: '5' as never, resourceRevision: '5', generation: 0 },
      )
    })
    await waitFor(() => expect(screen.getAllByText('No sessions yet').length).toBeGreaterThan(0))
    expect(mocks.api.sessions).not.toHaveBeenCalled()
    view.unmount()
  })

  it('restores an active session only after an explicit archive-first delete failure', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const commands: string[] = []
    view.transport.onSend = (message) => {
      if (message.type !== 'command') return
      commands.push(message.payload.name)
      if (message.payload.name === 'session.archive') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', archived: true })
      } else if (message.payload.name === 'session.delete') {
        failProjectCommand(view.transport, message, 'permission_denied')
      } else if (message.payload.name === 'session.restore') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', archived: false })
      }
    }

    fireEvent.click(screen.getByRole('button', { name: 'Delete session-1' }))
    await waitFor(() => expect(commands).toEqual(['session.archive']))
    await act(async () => { applySessionIndexAuthority(view, {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: true, status: 'idle', run_id: null, resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    }) })
    await waitFor(() => expect(commands).toEqual(['session.archive', 'session.delete', 'session.restore']))
    fireEvent.click(screen.getByRole('button', { name: 'Archived (1)' }))
    expect(screen.getByRole('button', { name: 'Restore session-1' })).toBeTruthy()
    expect(view.application.repositories.sessionIndex.getProjectReadModel('project-1').summaries).toHaveLength(1)
    await act(async () => { applySessionIndexAuthority(view, {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: false, status: 'idle', run_id: null, resource_revision: '2',
      updated_at: '2026-01-01T00:00:02Z', has_unread_result: false,
    }, '2') })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Restore session-1' })).toBeNull())
    expect(screen.getByRole('alert').textContent).toContain('Session operation failed.')
    view.unmount()
  })

  it('does not restore an active session after an unknown archive-first delete outcome', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const commands: string[] = []
    view.transport.onSend = (message) => {
      if (message.type !== 'command') return
      commands.push(message.payload.name)
      if (message.payload.name === 'session.archive') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', archived: true })
      } else if (message.payload.name === 'session.delete') {
        failProjectCommand(view.transport, message, 'outcome_unknown')
      }
    }

    fireEvent.click(screen.getByRole('button', { name: 'Delete session-1' }))
    await waitFor(() => expect(commands).toEqual(['session.archive']))
    await act(async () => { applySessionIndexAuthority(view, {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: true, status: 'idle', run_id: null, resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    }) })
    await waitFor(() => expect(commands).toEqual(['session.archive', 'session.delete']))
    fireEvent.click(screen.getByRole('button', { name: 'Archived (1)' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Session operation failed.'))
    expect(commands).not.toContain('session.restore')
    expect(screen.getByRole('button', { name: 'Restore session-1' })).toBeTruthy()
    view.unmount()
  })

  it('deletes an already archived session directly without a second archive command', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const commands: string[] = []
    view.transport.onSend = (message) => {
      if (message.type !== 'command') return
      commands.push(message.payload.name)
      if (message.payload.name === 'session.archive') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', archived: true })
      } else if (message.payload.name === 'session.delete') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', status: 'removed', removed_sessions: 1 })
      }
    }

    fireEvent.click(screen.getByRole('button', { name: 'Archive session-1' }))
    await waitFor(() => expect(commands).toEqual(['session.archive']))
    await act(async () => { applySessionIndexAuthority(view, {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: true, status: 'idle', run_id: null, resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    }) })
    fireEvent.click(await screen.findByRole('button', { name: 'Archived (1)' }))
    fireEvent.click(screen.getByRole('button', { name: 'Delete session-1' }))
    await waitFor(() => expect(commands).toEqual(['session.archive', 'session.delete']))
    expect(commands.filter((name) => name === 'session.archive')).toHaveLength(1)
    view.unmount()
  })

  it('clears an unread result only after the Session Index authority update', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'session.mark_read') {
        respondToProjectCommand(view.transport, message, { session_id: 'session-1', run_id: 'result-run', marked_read: true })
      }
    }
    const completed = {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: false, status: 'completed' as const, run_id: 'result-run', resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: true,
    }
    await act(async () => { applySessionIndexAuthority(view, completed, '1') })
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'session.mark_read')).toBe(true))
    expect(screen.getByLabelText('Unread result')).toBeTruthy()
    await act(async () => { applySessionIndexAuthority(view, { ...completed, resource_revision: '2', has_unread_result: false }, '2') })
    await waitFor(() => expect(screen.queryByLabelText('Unread result')).toBeNull())
    expect(mocks.api.sessions).not.toHaveBeenCalled()
    view.unmount()
  })

  it('waits for authoritative archive and delete changes in order, without restoring on unknown outcome', async () => {
    mocks.api.sessions.mockResolvedValue({ sessions: [] })
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('project')).toBeTruthy())
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    view.transport.onSend = (message) => {
      if (message.type !== 'command') return
      if (message.payload.name === 'project.archive') {
        respondToProjectCommand(view.transport, message, { project_id: 'project-1', archived: true })
      } else if (message.payload.name === 'project.delete') {
        failProjectCommand(view.transport, message, 'outcome_unknown')
      }
    }
    fireEvent.click(screen.getByRole('button', { name: 'Delete project' }))
    await waitFor(() => expect(view.transport.sent.filter((message) => message.type === 'command').map((message) => message.payload.name)).toEqual(['project.archive']))
    expect(screen.getAllByText('project').length).toBeGreaterThan(0)

    const adapter = new ProjectIndexAdapter()
    await act(async () => {
      view.application.replica.applyChange(
        { type: 'project_index', id: 'server' },
        adapter,
        [{ op: 'upsert', key: 'project-1', value: { ...defaultProject, archived: true, updated_at: '2026-01-02T00:00:00Z' } }],
        { streamEpoch: 'test', sequence: '1' as never, resourceRevision: '1', generation: 0 },
      )
    })
    await waitFor(() => expect(view.transport.sent.filter((message) => message.type === 'command').map((message) => message.payload.name)).toEqual(['project.archive', 'project.delete']))
    await waitFor(() => expect(screen.getAllByRole('alert').some((element) => element.textContent?.includes('Project operation failed.'))).toBe(true))
    expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'project.restore')).toBe(false)
    expect(view.application.repositories.projectIndex.getSnapshot().summaries.find((project) => project.id === 'project-1')?.archived).toBe(true)
    view.unmount()
  })

  it('does not mutate or restore when authoritative archive observation times out', async () => {
    mocks.api.sessions.mockResolvedValue({ sessions: [] })
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('project')).toBeTruthy())
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'project.archive') {
        respondToProjectCommand(view.transport, message, { project_id: 'project-1', archived: true })
      }
    }
    vi.useFakeTimers()
    fireEvent.click(screen.getByRole('button', { name: 'Delete project' }))
    await act(async () => { await vi.advanceTimersByTimeAsync(5001) })
    expect(view.transport.sent.filter((message) => message.type === 'command').map((message) => message.payload.name)).toEqual(['project.archive'])
    expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'project.restore')).toBe(false)
    expect(view.application.repositories.projectIndex.getSnapshot().active[0]?.id).toBe('project-1')
    expect(screen.getAllByRole('alert').some((element) => element.textContent?.includes('Project change accepted; waiting for synchronization.'))).toBe(true)
    view.unmount()
  })

  it('surfaces project command failure safely and leaves the authoritative name unchanged', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getAllByText('project').length).toBeGreaterThan(0))
    vi.spyOn(window, 'prompt').mockReturnValue('should not apply')
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'project.rename') failProjectCommand(view.transport, message)
    }
    fireEvent.click(screen.getByRole('button', { name: 'Rename project' }))
    await waitFor(() => expect(screen.getAllByRole('alert').some((element) => element.textContent?.includes('Project operation failed.'))).toBe(true))
    expect(screen.getAllByText('project').length).toBeGreaterThan(0)
    expect(screen.queryByText('protocol-secret-not-for-ui')).toBeNull()
    expect(view.application.repositories.projectIndex.getSnapshot().active[0]?.display_name).toBe('project')
    view.unmount()
  })

  it('applies committed item projection events to the shared cached history without refreshing', async () => {
    mocks.api.snapshot.mockResolvedValue({
      session_id: 'session-1',
      revision: '1',
      session: { ...mocks.session, revision: '1', last_seq: 1 },
      history: {
        items: [{
          id: 'initial-item',
          seq: 1,
          turn_id: 'turn-0',
          created_at: '2026-01-01T00:00:00Z',
          kind: 'message',
          visibility: 'visible',
          audience: 'user',
          message: { role: 'user', content: { inline: 'initial' } },
        }],
        oldest_seq: 1,
        newest_seq: 1,
        has_more_before: false,
        has_more_after: false,
      },
    })
    mocks.streamRun.mockImplementation(async (_runID: string, onEvent: (event: unknown) => void | Promise<void>) => {
      await onEvent({
        type: 'item.appended',
        session_id: 'session-1',
        seq: 2,
        revision: '2',
        item_id: 'projected-item',
        item: {
          id: 'projected-item',
          seq: 2,
          turn_id: 'turn-2',
          created_at: '2026-01-01T00:00:01Z',
          kind: 'message',
          visibility: 'visible',
          audience: 'model',
          message: { role: 'assistant', content: { inline: 'projected answer' } },
        },
      })
      // A projection event for a session that has not been snapshotted must
      // not cause App to manufacture history or refresh another page.
      await onEvent({
        type: 'item.appended',
        session_id: 'uncached-session',
        seq: 3,
        revision: '3',
        item_id: 'uncached-item',
        item: {
          id: 'uncached-item',
          seq: 3,
          created_at: '2026-01-01T00:00:02Z',
          kind: 'message',
          visibility: 'visible',
          audience: 'model',
          message: { role: 'assistant', content: { inline: 'must wait for snapshot' } },
        },
      })
    })

    const view = renderApp()
    await waitFor(() => expect(screen.getByText('projected answer')).toBeTruthy())
    expect(screen.queryByText('must wait for snapshot')).toBeNull()
    expect(mocks.streamRun).toHaveBeenCalled()
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(1)
    view.unmount()
  })

  const emptySnapshot = () => ({
    session_id: 'session-1',
    revision: '0',
    session: { ...mocks.session, revision: '0' },
    history: { items: [], oldest_seq: 0, newest_seq: 0, has_more_before: false, has_more_after: false },
  })

  async function renderSubmitReadyApp(
    activeRuns: Array<{ run_id: string; session_id: string; turn_id?: string; started_at: string; status: string }> = [],
    snapshot = emptySnapshot(),
  ) {
    mocks.api.activeRuns.mockResolvedValue({ runs: activeRuns })
    mocks.api.snapshot.mockResolvedValue(snapshot)
    mocks.streamRun.mockReset()
    mocks.streamRun.mockResolvedValue(undefined)
    const view = renderApp()
    view.transport.onSend = (message) => respondToSessionCreate(view, message)
    const composer = await screen.findByRole('textbox')
    return { view, composer }
  }

  function configureCreatedRoot() {
    const created = {
      ...mocks.session,
      id: 'session-new',
      display_name: 'new root',
      root_session_id: 'session-new',
      parent_session_id: undefined,
      spawn_depth: 0,
    }
    mocks.api.createSession.mockResolvedValue(created)
    mocks.api.sessions.mockResolvedValue({ sessions: [mocks.session, created] })
    return created
  }

  it('creates a configured root for exact /new without starting or appending a run', async () => {
    const created = configureCreatedRoot()
    const source = {
      ...mocks.session,
      reasoning_level: 'high',
      full_access: true,
      cwd: '/workspace/src',
      config_path: '/config',
      revision: '0',
    }
    const { view, composer } = await renderSubmitReadyApp([], {
      ...emptySnapshot(),
      session: source,
    })

    fireEvent.change(composer, { target: { value: '  /new  ' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'session.create')).toBe(true))
    const createCommand = view.transport.sent.find((message) => message.type === 'command' && message.payload.name === 'session.create')
    expect(createCommand && createCommand.type === 'command' ? createCommand.payload.arguments : null).toMatchObject({
      project_id: 'project-1', provider: 'fake', model_profile: 'default', reasoning_level: 'high', full_access: true,
      cwd: '/workspace/src', config_path: '/config',
    })
    expect(mocks.api.startRun).not.toHaveBeenCalled()
    expect(mocks.api.appendRunMessage).not.toHaveBeenCalled()
    expect(mocks.api.cancelRun).not.toHaveBeenCalled()
    await waitFor(() => expect(screen.getByText('new root')).toBeTruthy())
    const createdButton = screen.getByText('new root').closest('button')
    expect(createdButton).not.toBeNull()
    expect(createdButton?.parentElement?.className).toContain('selected')
    expect(created.parent_session_id).toBeUndefined()
    expect(created.root_session_id).toBe(created.id)
    expect((composer as HTMLTextAreaElement).value).toBe('')
    expect(mocks.api.sessions).not.toHaveBeenCalled()
    view.unmount()
  })

  it('hydrates missing session creation fields from the authoritative session endpoint', async () => {
    const created = configureCreatedRoot()
    const authoritative = {
      ...mocks.session,
      provider: 'authoritative-provider',
      model_profile: 'authoritative-model',
      reasoning_level: 'low',
      full_access: true,
      cwd: '/workspace/authoritative',
      config_path: '/config/authoritative.yaml',
    }
    const incomplete = {
      ...mocks.session,
      cwd: undefined,
      config_path: undefined,
      reasoning_level: undefined,
      revision: '0',
    } as unknown as ReturnType<typeof emptySnapshot>['session']
    mocks.api.session.mockResolvedValue(authoritative)
    const { view, composer } = await renderSubmitReadyApp([], {
      ...emptySnapshot(),
      session: incomplete,
    })

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(mocks.api.session).toHaveBeenCalledWith('session-1'))
    const createCommand = view.transport.sent.find((message) => message.type === 'command' && message.payload.name === 'session.create')
    expect(createCommand && createCommand.type === 'command' ? createCommand.payload.arguments : null).toMatchObject({
      project_id: 'project-1', provider: 'authoritative-provider', model_profile: 'authoritative-model', reasoning_level: 'low',
      full_access: true, cwd: '/workspace/authoritative', config_path: '/config/authoritative.yaml',
    })
    expect(created.parent_session_id).toBeUndefined()
    view.unmount()
  })

  it('allows /new while the source session has a running run without touching that run', async () => {
    configureCreatedRoot()
    const { view, composer } = await renderSubmitReadyApp([{
      run_id: 'run-1', session_id: 'session-1', turn_id: 'turn-1', started_at: '', status: 'running',
    }])

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Append to current run' }))

    await waitFor(() => expect(view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'session.create')).toBe(true))
    expect(mocks.api.startRun).not.toHaveBeenCalled()
    expect(mocks.api.appendRunMessage).not.toHaveBeenCalled()
    expect(mocks.api.cancelRun).not.toHaveBeenCalled()
    view.unmount()
  })

  it('sends /new as normal run input when an image is attached', async () => {
    mocks.api.startRun.mockResolvedValue(undefined)
    const { view, composer } = await renderSubmitReadyApp()
    const file = new File(['image'], 'image.png', { type: 'image/png' })
    const clipboardItem = { kind: 'file', type: 'image/png', getAsFile: () => file }

    fireEvent.paste(composer, {
      clipboardData: { items: [clipboardItem], getData: () => '' },
    })
    await waitFor(() => expect(screen.getByAltText('Image to send #1')).toBeTruthy())
    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(mocks.api.startRun).toHaveBeenCalledTimes(1))
    expect(mocks.api.createSession).not.toHaveBeenCalled()
    view.unmount()
  })

  it('does not treat /new extra as a session command', async () => {
    mocks.api.startRun.mockResolvedValue(undefined)
    const { view, composer } = await renderSubmitReadyApp()

    fireEvent.change(composer, { target: { value: '/new extra' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(mocks.api.startRun).toHaveBeenCalledTimes(1))
    expect(mocks.api.createSession).not.toHaveBeenCalled()
    view.unmount()
  })

  it('keeps /new in the composer when root creation fails', async () => {
    const { view, composer } = await renderSubmitReadyApp()
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'session.create') failProjectCommand(view.transport, message, 'session_unavailable')
    }

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Session operation failed.'))
    expect((composer as HTMLTextAreaElement).value).toBe('/new')
    expect(mocks.api.startRun).not.toHaveBeenCalled()
    view.unmount()
  })

  it('does not create two roots when /new is submitted twice before the first response', async () => {
    const created = configureCreatedRoot()
    const { view, composer } = await renderSubmitReadyApp()
    let createCommand: ProtocolMessage | undefined
    view.transport.onSend = (message) => { if (message.type === 'command' && message.payload.name === 'session.create') createCommand = message }

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(createCommand).toBeTruthy())

    respondToSessionCreate(view, createCommand!)
    await waitFor(() => expect(screen.getByText('new root')).toBeTruthy())
    expect(view.transport.sent.filter((message) => message.type === 'command' && message.payload.name === 'session.create')).toHaveLength(1)
    view.unmount()
  })

  it('keeps the submitted source configuration when selection changes during creation', async () => {
    const created = configureCreatedRoot()
    const other = { ...mocks.session, id: 'session-2', display_name: 'other session' }
    const source = {
      ...mocks.session,
      provider: 'source-provider',
      model_profile: 'source-model',
      reasoning_level: 'source-level',
      full_access: true,
      cwd: '/workspace/source',
      config_path: '/config/source.yaml',
      revision: '0',
    }
    const { view, composer } = await renderSubmitReadyApp([], { ...emptySnapshot(), session: source })
    let createCommand: ProtocolMessage | undefined
    view.transport.onSend = (message) => { if (message.type === 'command' && message.payload.name === 'session.create') createCommand = message }

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(createCommand).toBeTruthy())

    const sessionAdapter = new SessionIndexAdapter('project-1')
    await act(async () => {
      view.application.replica.applyChange(
        { type: 'session_index', id: 'project-1' }, sessionAdapter,
        [{ op: 'upsert', key: 'session-2', value: {
          session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'other session',
          archived: false, status: 'idle', run_id: null, resource_revision: '2', updated_at: '2026-01-02T00:00:00Z', has_unread_result: false,
        } }],
        { streamEpoch: 'test', sequence: '2' as never, resourceRevision: '2', generation: 0 },
      )
    })

    mocks.api.snapshot.mockResolvedValue({
      ...emptySnapshot(),
      session_id: 'session-2',
      session: { ...other, revision: '0' },
    })
    const otherButton = screen.getByText('other session').closest('button')
    expect(otherButton).not.toBeNull()
    fireEvent.click(otherButton!)
    await waitFor(() => expect(mocks.api.snapshot).toHaveBeenCalledWith('session-2'))

    expect(createCommand && createCommand.type === 'command' ? createCommand.payload.arguments : null).toMatchObject({
      project_id: 'project-1', provider: 'source-provider', model_profile: 'source-model', reasoning_level: 'source-level',
      full_access: true, cwd: '/workspace/source', config_path: '/config/source.yaml',
    })
    respondToSessionCreate(view, createCommand!)
    await waitFor(() => expect(screen.getByText('new root')).toBeTruthy())
    const createdButton = screen.getByText('new root').closest('button')
    expect(createdButton?.parentElement?.className).toContain('selected')
    view.unmount()
  })

  async function renderSettlementApp(snapshots: Array<ReturnType<typeof emptySnapshot> & { revision: string }> = [emptySnapshot()]) {
    mocks.api.activeRuns.mockResolvedValue({ runs: [{ run_id: 'settlement-run', session_id: 'session-1', turn_id: 'turn-1', started_at: '', status: 'running' }] })
    mocks.api.snapshot.mockReset()
    for (const response of snapshots) mocks.api.snapshot.mockResolvedValueOnce(response)
    mocks.streamRun.mockReset()
    let onEvent: ((event: unknown) => void | Promise<void>) | undefined
    mocks.streamRun.mockImplementation(async (_runID: string, handler: (event: unknown) => void | Promise<void>) => {
      onEvent = handler
    })
    const view = renderApp()
    await screen.findByRole('textbox')
    await waitFor(() => expect(onEvent).toBeDefined())
    return { view, onEvent: onEvent! }
  }

  it('records one accepted or ignored decision for each settled event', async () => {
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.setEnabled('session-1', true)
    const { view, onEvent } = await renderSettlementApp()
    const settled = { type: 'run.settled' as const, run_id: 'settlement-run', status: 'committed', committed_revision: '0' }

    await act(async () => {
      await onEvent(settled)
      await onEvent(settled)
    })

    const decisions = frontendProtocolLogger.getSnapshot('session-1').records
      .filter((record) => record.source === 'app.event_gate' && record.event_type === 'run.settled')
      .map((record) => record.kind)
    expect(decisions).toEqual(['accepted', 'ignored'])
    view.unmount()
  })

  const projection = (revision: string, text = 'durable answer') => ({
    type: 'item.appended',
    session_id: 'session-1',
    run_id: 'settlement-run',
    seq: 2,
    revision,
    item_id: `item-${revision}`,
    item: {
      id: `item-${revision}`,
      seq: 2,
      turn_id: 'turn-1',
      created_at: '',
      kind: 'message',
      visibility: 'visible',
      audience: 'model',
      message: { role: 'assistant', content: { inline: text } },
    },
  })

  it('waits for admitted run_id before connecting and renders only the committed user item once', async () => {
    let resolveAdmission!: (value: { run_id: string; session_id: string; status: string }) => void
    mocks.api.startRun.mockImplementation(() => new Promise((resolve) => { resolveAdmission = resolve }))
    const { view, composer } = await renderSubmitReadyApp()

    fireEvent.change(composer, { target: { value: 'submitted text' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(mocks.api.startRun).toHaveBeenCalledTimes(1))
    const lifecycleHandler = mocks.streamLifecycle.mock.calls[0][0] as (event: unknown) => Promise<void>
    await act(async () => {
      await lifecycleHandler({ type: 'run.started', session_id: 'session-1', run_id: 'authoritative-run', turn_id: 'turn-1' })
    })
    expect(mocks.streamRun).not.toHaveBeenCalled()
    expect((composer as HTMLTextAreaElement).disabled).toBe(true)
    expect(view.container.querySelectorAll('.message.user')).toHaveLength(0)

    resolveAdmission({ run_id: 'authoritative-run', session_id: 'session-1', status: 'running' })
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(1))
    expect(mocks.streamRun.mock.calls[0][0]).toBe('authoritative-run')
    expect(mocks.streamRun.mock.calls[0][2]).toEqual(expect.objectContaining({ signal: expect.any(AbortSignal) }))
    // The successful admission clears only the composer draft. It does not
    // manufacture a conversation row while the stream is still quiet.
    expect((composer as HTMLTextAreaElement).value).toBe('')
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull()
    expect(view.container.querySelectorAll('.cursor')).toHaveLength(0)
    expect((composer as HTMLTextAreaElement).disabled).toBe(true)
    const blockedSend = screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement
    expect(blockedSend.disabled).toBe(true)
    fireEvent.click(blockedSend)
    expect(mocks.api.startRun).toHaveBeenCalledTimes(1)
    expect(view.container.querySelectorAll('.message.user')).toHaveLength(0)

    const onEvent = mocks.streamRun.mock.calls[0][1] as (event: unknown) => Promise<void> | void
    const committedUserEvent = {
      type: 'item.appended',
      session_id: 'session-1',
      run_id: 'authoritative-run',
      seq: 1,
      revision: '1',
      item_id: 'backend-user-id',
      item: {
        id: 'backend-user-id',
        seq: 1,
        turn_id: 'turn-1',
        created_at: '2026-01-01T00:00:00Z',
        kind: 'message',
        visibility: 'visible',
        audience: 'user',
        message: { role: 'user', content: { inline: 'submitted text' } },
      },
    }
    await act(async () => { await onEvent(committedUserEvent) })
    expect(view.container.querySelectorAll('.message.user')).toHaveLength(1)
    expect(screen.getByText('submitted text')).toBeTruthy()

    // The replay, rather than the admission response, creates the transient
    // run container. Subsequent deltas now have a place to accumulate.
    await act(async () => {
      await onEvent({ type: 'run.started', run_id: 'authoritative-run', session_id: 'session-1', turn_id: 'turn-1' })
    })
    expect(screen.getByRole('button', { name: 'Stop' })).toBeTruthy()
    expect(view.container.querySelectorAll('.cursor')).toHaveLength(1)
    expect(view.container.querySelectorAll('.reasoning-step')).toHaveLength(0)
    expect((composer as HTMLTextAreaElement).disabled).toBe(false)
    expect(screen.getByRole('button', { name: 'Append to current run' })).toBeTruthy()
    await act(async () => {
      await onEvent({ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'assistant delta' })
    })
    await waitFor(() => expect(screen.getByText('assistant delta')).toBeTruthy())

    // Replay/duplicate delivery is an upsert by the backend item id, not by
    // message text or turn, and therefore remains one rendered item.
    await act(async () => { await onEvent(committedUserEvent) })
    expect(view.container.querySelectorAll('.message.user')).toHaveLength(1)
    view.unmount()
  })

  it('tears down a covered settlement without an extra snapshot', async () => {
    const { view, onEvent } = await renderSettlementApp()
    await act(async () => {
      await onEvent(projection('7'))
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'committed', committed_revision: '7' })
      const lifecycleHandler = mocks.streamLifecycle.mock.calls[0][0] as (event: unknown) => Promise<void>
      await lifecycleHandler({ type: 'run.settled', session_id: 'session-1', run_id: 'settlement-run', status: 'committed', committed_revision: '7' })
    })
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull())
    expect(screen.getByText('durable answer')).toBeTruthy()
    view.unmount()
  })

  it('applies covered settlement metadata to the shared store when the sidebar is stale', async () => {
    const initial = {
      ...emptySnapshot(),
      session: {
        ...mocks.session,
        revision: '0',
        last_seq: 0,
        status: 'running',
        current_run_id: 'settlement-run',
        running_run_id: 'settlement-run',
        running_turn_id: 'turn-1',
      },
    }
    const { view, onEvent } = await renderSettlementApp([initial])
    await act(async () => {
      // This advances the shared projection/store revision, while the
      // sidebar DTO remains at its older bootstrap revision.
      await onEvent(projection('7'))
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'committed', committed_revision: '7' })
    })
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(screen.getByLabelText('Session status: idle')).toBeTruthy())
    expect(screen.queryByLabelText('Session status: running')).toBeNull()
    view.unmount()
  })

  it('refreshes a lagging settlement and removes the run only after coverage', async () => {
    const initial = emptySnapshot()
    const repaired = {
      ...emptySnapshot(),
      revision: '2',
      session: { ...mocks.session, revision: '2', last_seq: 2 },
      history: {
        items: [projection('2').item] as never[],
        oldest_seq: 2,
        newest_seq: 2,
        has_more_before: false,
        has_more_after: false,
      },
    }
    const { view, onEvent } = await renderSettlementApp([initial, repaired])
    await act(async () => {
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'committed', committed_revision: '2' })
    })
    await waitFor(() => expect(mocks.api.snapshot).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull())
    expect(screen.getByText('durable answer')).toBeTruthy()
    view.unmount()
  })

  it('uses precision-safe comparison and keeps covered failed partial items', async () => {
    const localRevision = '90071992547409929'
    const committedRevision = '90071992547409930'
    const initial = {
      ...emptySnapshot(),
      revision: localRevision,
      session: { ...mocks.session, revision: localRevision, last_seq: 0 },
    }
    const { view, onEvent } = await renderSettlementApp([initial])
    await act(async () => {
      await onEvent(projection(committedRevision, 'partial answer'))
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'failed', committed_revision: committedRevision, message: 'failed after partial commit' })
    })
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(1)
    expect(screen.getByText('partial answer')).toBeTruthy()
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull())
    view.unmount()
  })

  it('keeps covered cancelled partial items without a snapshot', async () => {
    const { view, onEvent } = await renderSettlementApp([emptySnapshot()])
    await act(async () => {
      await onEvent(projection('4', 'cancelled partial'))
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'cancelled', committed_revision: '4' })
    })
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(1)
    expect(screen.getByText('cancelled partial')).toBeTruthy()
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull())
    view.unmount()
  })

  it('conservatively refreshes an invalid settlement watermark and handles duplicate settlement', async () => {
    const { view, onEvent } = await renderSettlementApp([emptySnapshot(), { ...emptySnapshot(), revision: '0' }])
    await act(async () => {
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'committed', committed_revision: 'invalid' })
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'committed', committed_revision: 'invalid' })
    })
    await waitFor(() => expect(mocks.api.snapshot).toHaveBeenCalledTimes(2))
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull()
    view.unmount()
  })

  it('receives an early item from the run replay without an optimistic active-run user row', async () => {
    mocks.api.startRun.mockResolvedValue({ run_id: 'fast-run', session_id: 'session-1', status: 'running' })
    const { view, composer } = await renderSubmitReadyApp()
    mocks.streamRun.mockImplementation(async (_runID: string, onEvent: (event: unknown) => void | Promise<void>) => {
      await onEvent({
        type: 'item.appended', session_id: 'session-1', run_id: 'fast-run', seq: 1, revision: '1', item_id: 'fast-user-id',
        item: {
          id: 'fast-user-id', seq: 1, turn_id: 'turn-fast', created_at: '', kind: 'message', visibility: 'visible', audience: 'user',
          message: { role: 'user', content: { inline: 'fast replay user' } },
        },
      })
    })
    fireEvent.change(composer, { target: { value: 'fast replay user' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(screen.getByText('fast replay user')).toBeTruthy())
    expect(mocks.streamRun).toHaveBeenCalledTimes(1)
    expect(mocks.streamRun.mock.calls[0][0]).toBe('fast-run')
    expect(view.container.querySelectorAll('.message.user')).toHaveLength(1)
    view.unmount()
  })

  it('keeps the draft and does not connect or create a user row when admission fails', async () => {
    mocks.api.startRun.mockRejectedValue(new Error('admission failed'))
    const { view, composer } = await renderSubmitReadyApp()
    fireEvent.change(composer, { target: { value: 'retry me' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('admission failed'))
    expect(mocks.streamRun).not.toHaveBeenCalled()
    expect(view.container.querySelectorAll('.message.user')).toHaveLength(0)
    expect((composer as HTMLTextAreaElement).value).toBe('retry me')
    expect((composer as HTMLTextAreaElement).disabled).toBe(false)
    view.unmount()
  })

  it('does not delete an existing run when a new admission fails during a lifecycle race', async () => {
    const { view, composer } = await renderSubmitReadyApp([{
      run_id: 'existing-run', session_id: 'session-1', turn_id: 'turn-existing', started_at: '', status: 'running',
    }])
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(1))
    const existingOnEvent = mocks.streamRun.mock.calls[0][1] as (event: unknown) => Promise<void> | void
    await act(async () => {
      await existingOnEvent({ type: 'turn.failed', turn_id: 'turn-existing', code: 'failed', message: 'existing run failed' })
    })
    await waitFor(() => expect(view.container.querySelector('.turn-error')).not.toBeNull())
    expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)

    let rejectAdmission!: (reason: unknown) => void
    mocks.api.startRun.mockImplementation(() => new Promise((_resolve, reject) => { rejectAdmission = reject }))
    fireEvent.change(composer, { target: { value: 'new attempt' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(mocks.api.startRun).toHaveBeenCalledTimes(1))

    const lifecycleHandler = mocks.streamLifecycle.mock.calls[0][0] as (event: unknown) => Promise<void>
    await act(async () => {
      await lifecycleHandler({ type: 'run.started', session_id: 'session-1', run_id: 'different-background-run', turn_id: 'turn-background' })
    })
    rejectAdmission(new Error('new admission failed'))
    await waitFor(() => expect(view.container.querySelector('.error-banner')?.textContent).toContain('new admission failed'))

    // Admission never owned existing-run, so its terminal error remains after
    // the rejected attempt, without leaving an empty transient row.
    expect(view.container.querySelector('.turn-error')).not.toBeNull()
    expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    view.unmount()
  })

  it('does not let a superseded run stream error pollute the current session', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [{ run_id: 'old-run', session_id: 'session-1', turn_id: 'turn-old', started_at: '', status: 'running' }] })
    mocks.api.snapshot.mockResolvedValue({
      ...emptySnapshot(),
      session: { ...mocks.session, status: 'running', current_run_id: 'old-run', running_run_id: 'old-run', running_turn_id: 'turn-old' },
    })
    mocks.api.startRun.mockResolvedValue({ run_id: 'new-run', session_id: 'session-1', status: 'running' })
    mocks.streamRun.mockReset()
    let rejectOld!: (reason: unknown) => void
    mocks.streamRun.mockImplementation(async (runID: string, _handler: (event: unknown) => void | Promise<void>) => {
      if (runID === 'old-run') return await new Promise<void>((_resolve, reject) => { rejectOld = reject })
      return await new Promise<void>(() => {})
    })

    const view = renderApp()
    applySessionIndexAuthority(view, {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: false, status: 'running', run_id: 'old-run', resource_revision: '1', updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    })
    const composer = await screen.findByRole('textbox')
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(1))
    const oldHandler = mocks.streamRun.mock.calls[0][1] as (event: unknown) => Promise<void> | void
    await act(async () => { await oldHandler({ type: 'turn.failed', turn_id: 'turn-old', code: 'failed', message: 'old failure' }) })
    await waitFor(() => expect(view.container.querySelector('.turn-error')).not.toBeNull())
    expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)

    fireEvent.change(composer, { target: { value: 'new attempt' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(2))
    const newHandler = mocks.streamRun.mock.calls[1][1] as (event: unknown) => Promise<void> | void
    await act(async () => { await newHandler({ type: 'run.started', run_id: 'new-run', session_id: 'session-1', turn_id: 'turn-new' }) })

    // A stale reader can still deliver events after the newer run has taken
    // the session slot.  Transient terminal events are ignored, while a
    // committed projection from this old stream remains valid durable data.
    await act(async () => {
      await oldHandler({
        type: 'item.appended', session_id: 'session-1', run_id: 'old-run', seq: 1, revision: '1', item_id: 'old-durable-item',
        item: {
          id: 'old-durable-item', seq: 1, turn_id: 'turn-old', created_at: '', kind: 'message', visibility: 'visible', audience: 'model',
          message: { role: 'assistant', content: { inline: 'durable old output' } },
        },
      })
      await oldHandler({ type: 'turn.failed', turn_id: 'turn-old', code: 'failed', message: 'late old failure' })
      await oldHandler({ type: 'run.settled', run_id: 'old-run', status: 'committed', committed_revision: '9', message: 'late old settlement' })
    })
    expect(screen.getByText('durable old output')).toBeTruthy()
    expect(screen.queryByText('late old failure')).toBeNull()
    expect(view.container.querySelector('.turn-error')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getByRole('button', { name: 'Stop' })).toBeTruthy()
    expect(screen.getByLabelText('Session status: running')).toBeTruthy()

    // The old reader rejects after the new authoritative run has taken over.
    // It may clean up its own connection, but must not create a global error
    // banner or alter the new run's transient state.
    await act(async () => { rejectOld(new Error('old stream disconnected')) })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Stop' })).toBeTruthy())
    expect(screen.queryByRole('alert')).toBeNull()
    view.unmount()
  })

  it('keeps a pending new admission gated when an old terminal replay arrives', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [{ run_id: 'old-run', session_id: 'session-1', turn_id: 'turn-old', started_at: '', status: 'running' }] })
    mocks.api.snapshot.mockResolvedValue({
      ...emptySnapshot(),
      session: { ...mocks.session, status: 'running', current_run_id: 'old-run', running_run_id: 'old-run', running_turn_id: 'turn-old' },
    })
    let resolveAdmission!: (value: { run_id: string; session_id: string; status: string }) => void
    mocks.api.startRun.mockImplementation(() => new Promise((resolve) => { resolveAdmission = resolve }))
    mocks.streamRun.mockReset()
    let rejectOld!: (reason: unknown) => void
    mocks.streamRun.mockImplementation(async (runID: string, _handler: (event: unknown) => void | Promise<void>) => {
      if (runID === 'old-run') return await new Promise<void>((_resolve, reject) => { rejectOld = reject })
      return await new Promise<void>(() => {})
    })

    const view = renderApp()
    applySessionIndexAuthority(view, {
      session_id: 'session-1', project_id: 'project-1', parent_session_id: null, display_name: 'session-1',
      archived: false, status: 'running', run_id: 'old-run', resource_revision: '1', updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    })
    const composer = await screen.findByRole('textbox')
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(1))
    const oldHandler = mocks.streamRun.mock.calls[0][1] as (event: unknown) => Promise<void> | void
    await act(async () => { await oldHandler({ type: 'turn.failed', turn_id: 'turn-old', code: 'failed', message: 'old failure' }) })

    fireEvent.change(composer, { target: { value: 'new admission' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(mocks.api.startRun).toHaveBeenCalledTimes(1))
    expect((composer as HTMLTextAreaElement).disabled).toBe(true)

    // There is no ActiveRun for the new run yet.  The old
    // terminal replay must not clear awaitingRunStarted or mark the sidebar
    // idle, and a second click must not submit another POST.
    await act(async () => { await oldHandler({ type: 'run.settled', run_id: 'old-run', status: 'committed', committed_revision: '9' }) })
    expect((composer as HTMLTextAreaElement).disabled).toBe(true)
    const lifecycleHandler = mocks.streamLifecycle.mock.calls[0][0] as (event: unknown) => Promise<void>
    await act(async () => {
      await lifecycleHandler({
        type: 'run.settled', session_id: 'session-1', run_id: 'old-run', status: 'committed', committed_revision: '9',
        session: { ...mocks.session, status: 'idle', current_run_id: undefined, running_run_id: undefined },
      })
    })
    expect(screen.getByLabelText('Session status: running')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(mocks.api.startRun).toHaveBeenCalledTimes(1)

    resolveAdmission({ run_id: 'new-run', session_id: 'session-1', status: 'running' })
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(2))
    expect(mocks.streamRun.mock.calls[1][0]).toBe('new-run')
    const newHandler = mocks.streamRun.mock.calls[1][1] as (event: unknown) => Promise<void> | void
    await act(async () => { await newHandler({ type: 'run.started', run_id: 'new-run', session_id: 'session-1', turn_id: 'turn-new' }) })
    expect((composer as HTMLTextAreaElement).disabled).toBe(false)

    await act(async () => { rejectOld(new Error('old stream closed')) })
    expect(screen.queryByRole('alert')).toBeNull()
    view.unmount()
  })

  it('keeps a lagging run through bounded refresh failures', async () => {
    const { view, onEvent } = await renderSettlementApp([emptySnapshot()])
    vi.useFakeTimers()
    await act(async () => {
      await onEvent({ type: 'run.settled', run_id: 'settlement-run', status: 'committed', committed_revision: '9' })
    })
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(2)
    expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)

    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    // Initial refresh plus only the bounded retry budget. The overlay is
    // still present; only the terminal timeout/manual action may clear it.
    expect(mocks.api.snapshot.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(mocks.api.snapshot.mock.calls.length).toBeLessThanOrEqual(4)
    expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(0)
    view.unmount()
  })

  it('shows a background completion from Session Index without a lifecycle or run settled event', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    const view = renderApp()
    await screen.findByRole('textbox')
    const lifecycleCalls = mocks.streamLifecycle.mock.calls.length
    const runCalls = mocks.streamRun.mock.calls.length
    await act(async () => { applySessionIndexAuthority(view, {
      session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'background-session',
      archived: false, status: 'running', run_id: 'background-run', resource_revision: '1',
      updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
    }) })
    await screen.findByText('background-session')
    await act(async () => { applySessionIndexAuthority(view, {
      session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'background-session',
      archived: false, status: 'completed', run_id: 'background-run', resource_revision: '2',
      updated_at: '2026-01-01T00:00:02Z', has_unread_result: true,
    }, '2') })
    await waitFor(() => expect(screen.getByText('background-session completed in the background.')).toBeTruthy())
    expect(mocks.streamLifecycle).toHaveBeenCalledTimes(lifecycleCalls)
    expect(mocks.streamRun).toHaveBeenCalledTimes(runCalls)
    view.unmount()
  })

  it('does not refresh the selected session for a covered background settlement', async () => {
    const background = { ...mocks.session, id: 'session-2', display_name: 'background-session', revision: '5', last_seq: 5 }
    let activeRunsCalls = 0
    mocks.api.activeRuns.mockImplementation(async () => {
      activeRunsCalls += 1
      return activeRunsCalls === 1
        ? { runs: [] }
        : { runs: [{ run_id: 'background-run', session_id: 'session-2', turn_id: 'turn-2', started_at: '', status: 'running' }] }
    })
    // Coverage is only established by a snapshot. Prime the background
    // session's empty history, then return to the selected session before its
    // run settles; a pre-snapshot projection queue must not make this test
    // accidentally prove coverage.
    mocks.api.snapshot.mockReset().mockImplementation(async (sessionID: string) => sessionID === 'session-2'
      ? {
        session_id: 'session-2',
        revision: '5',
        session: background,
        history: { items: [], oldest_seq: 0, newest_seq: 0, has_more_before: false, has_more_after: false },
      }
      : emptySnapshot())
    mocks.streamRun.mockReset().mockImplementation(async (_runID: string, onEvent: (event: unknown) => void | Promise<void>) => {
      await onEvent({
        type: 'item.appended', session_id: 'session-2', seq: 1, revision: '5', item_id: 'background-item',
        item: { id: 'background-item', seq: 1, kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'background answer' } } },
      })
      await onEvent({ type: 'run.settled', run_id: 'background-run', status: 'committed', committed_revision: '5' })
    })
    const view = renderApp()
    await screen.findByRole('textbox')
    applySessionIndexAuthority(view, {
      session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'background-session',
      archived: false, status: 'running', run_id: 'background-run', resource_revision: '5', updated_at: '2026-01-01T00:00:05Z', has_unread_result: false,
    })
    await screen.findByText('background-session')
    const snapshotsBeforeBackground = mocks.api.snapshot.mock.calls.length
    fireEvent.click(screen.getByText('background-session'))
    await waitFor(() => expect(mocks.api.snapshot.mock.calls.length).toBeGreaterThan(snapshotsBeforeBackground))
    const snapshotsBeforeReturn = mocks.api.snapshot.mock.calls.length
    fireEvent.click(screen.getByText('session-1'))
    await waitFor(() => expect(mocks.api.snapshot.mock.calls.length).toBeGreaterThan(snapshotsBeforeReturn))
    const snapshotsBeforeSettlement = mocks.api.snapshot.mock.calls.length
    const lifecycleHandler = mocks.streamLifecycle.mock.calls[0][0] as (event: unknown) => Promise<void>
    await act(async () => {
      await lifecycleHandler({
        type: 'run.started', session_id: 'session-2', run_id: 'background-run', status: 'running', session: background,
      })
    })
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(1))
    applySessionIndexAuthority(view, {
      session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'background-session',
      archived: false, status: 'completed', run_id: 'background-run', resource_revision: '6', updated_at: '2026-01-01T00:00:06Z', has_unread_result: true,
    }, '2')
    await waitFor(() => expect(screen.getByText('background-session')).toBeTruthy())
    expect(screen.getByLabelText('Unread result')).toBeTruthy()
    expect(screen.getAllByText(/completed/).length).toBeGreaterThan(0)
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(snapshotsBeforeSettlement)
    view.unmount()
  })
})

