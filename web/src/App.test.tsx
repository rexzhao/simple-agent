// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { frontendProtocolLogger } from './lib/frontendProtocolLogger'
import { decodeMessage } from './protocol/decode'
import { SyncApplicationProvider } from './applicationContext'
import { createSyncApplication } from './sync/applicationComposition'
import { ProjectIndexAdapter } from './sync/projectIndexAdapter'
import type { SessionSummary } from './sync/sessionIndexAdapter'
import { SessionIndexAdapter } from './sync/sessionIndexAdapter'
import { SessionContentAdapter } from './sync/sessionContentAdapter'
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
    sessionImage: vi.fn(),
  }
  return { api, session }
})

vi.mock('./api', () => ({
  api: mocks.api,
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

function applySessionContentAuthorityFor(view: { application: ReturnType<typeof createSyncApplication> }, sessionID: string, overrides: Record<string, unknown> = {}, sequence = '1', historyItems?: unknown[], activeRun: unknown = null): void {
  const session = {
    id: sessionID, version: 2, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    archived: false, last_used_at: '2026-01-01T00:00:00Z', has_unread_result: false, status: 'idle',
    show_reasoning: false, full_access: false, debug: { request_bodies: false }, context: {}, save_tool_results: false,
    provider: 'fake', model_profile: 'default', model_id: 'fake-model', project_id: 'project-1',
    cwd: '/workspace/src', created_cwd: '/workspace', config_path: '/config', reasoning_level: 'medium',
    ...overrides,
  }
  view.application.replica.applySnapshot(
    { type: 'session_content', id: sessionID },
    new SessionContentAdapter(sessionID),
    {
      schema_version: 1,
      session,
      history: {
        items: historyItems ?? [{
          key: { turn_id: 'turn-1', agent_iteration: 1, item_id: 'item-1' }, seq: 1,
          created_at: '2026-01-01T00:00:01Z', kind: 'message', visibility: 'visible', audience: 'user',
          message: { role: 'user', content: { inline: 'from session content' } },
        }],
        descriptor: { limit: 20, oldest_item_seq: '1', newest_item_seq: '1', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
      },
      active_run: activeRun,
      compaction: { checkpoints: [], truncated: false },
    } as unknown as JsonValue,
    { streamEpoch: 'test', sequence: sequence as never, resourceRevision: sequence, generation: 1 },
  )
}

function sessionContentSubscribedMessage(subscriptionID: string, sessionID: string, sequence = '1'): ProtocolMessage {
  return decodeMessage(JSON.stringify({
    version: 1, type: 'subscribed', id: `subscribed-${subscriptionID}`,
    payload: { subscription_id: subscriptionID, resource: { type: 'session_content', id: sessionID }, stream_epoch: 'test-epoch', sequence },
  }))
}

function sessionContentSnapshotMessage(subscriptionID: string, sessionID: string, runID: string, sequence = '1'): ProtocolMessage {
  return decodeMessage(JSON.stringify({
    version: 1, type: 'snapshot', id: `snapshot-${subscriptionID}`,
    payload: {
      subscription_id: subscriptionID,
      resource: { type: 'session_content', id: sessionID },
      stream_epoch: 'test-epoch', sequence, resource_revision: sequence,
      content: { inline: {
        schema_version: 1,
        session: {
          id: sessionID, version: 2, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
          archived: false, last_used_at: '2026-01-01T00:00:00Z', has_unread_result: false, status: 'idle',
          last_run_id: runID, latest_run_id: runID,
          show_reasoning: false, full_access: false, debug: { request_bodies: false }, context: {}, save_tool_results: false,
          provider: 'fake', model_profile: 'default', model_id: 'fake-model', project_id: 'project-1',
          cwd: '/workspace/src', created_cwd: '/workspace', config_path: '/config', reasoning_level: 'medium',
        },
        history: {
          items: [{
            key: { turn_id: 'turn-1', agent_iteration: 1, item_id: 'item-1' }, seq: 1,
            created_at: '2026-01-01T00:00:01Z', kind: 'message', visibility: 'visible', audience: 'user',
            message: { role: 'user', content: { inline: 'from resynchronized session content' } },
          }],
          descriptor: { limit: 20, oldest_item_seq: '1', newest_item_seq: '1', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
        },
        active_run: null,
        compaction: { checkpoints: [], truncated: false },
      } },
    },
  }))
}

function applySessionContentAuthority(view: { application: ReturnType<typeof createSyncApplication> }, overrides: Record<string, unknown> = {}, sequence = '1'): void {
  applySessionContentAuthorityFor(view, 'session-1', overrides, sequence)
}

function respondToSessionCreate(view: { application: ReturnType<typeof createSyncApplication>; transport: AppTestTransport }, message: ProtocolMessage, sessionID?: string): void {
  if (message.type !== 'command' || message.payload.name !== 'session.create') return
  const admittedSessionID = sessionID ?? String(message.payload.arguments.session_id ?? '')
  view.transport.emit({
    version: 1, type: 'command_result', id: `result-${message.payload.request_id}`,
    payload: { request_id: message.payload.request_id, status: 'succeeded', result: { session_id: admittedSessionID, project_id: 'project-1' } },
  } as unknown as ProtocolMessage)
  applySessionIndexAuthority(view, {
    session_id: admittedSessionID, project_id: 'project-1', parent_session_id: null, display_name: 'new root', archived: false,
    status: 'idle', run_id: null, resource_revision: '1', updated_at: '2026-01-01T00:00:01Z', has_unread_result: false,
  })
}

describe('App lifecycle bootstrap', () => {
  function resetApiMocks() {
    mocks.api.bootstrap.mockReset().mockResolvedValue({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' })
  }

  beforeEach(() => {
    resetApiMocks()
  })

  async function renderContentReadyApp() {
    const view = renderApp()
    await screen.findByRole('textbox')
    await act(async () => { applySessionContentAuthority(view) })
    await waitFor(() => expect(screen.getByText('from session content')).toBeTruthy())
    return { view, composer: screen.getByRole('textbox') as HTMLTextAreaElement }
  }

  function respondToRunStart(view: { transport: AppTestTransport }, runID: string) {
    view.transport.onSend = (message) => {
      if (message.type !== 'command' || message.payload.name !== 'run.start') return
      view.transport.emit({
        version: 1, type: 'command_result', id: `result-${message.payload.request_id}`,
        payload: { request_id: message.payload.request_id, status: 'succeeded', result: { session_id: 'session-1', run_id: runID, status: 'running' } },
      } as unknown as ProtocolMessage)
    }
  }

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
    view.unmount()
  })

  it('renders the opened detail and history from Session Content, not legacy session reads', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    await act(async () => { applySessionContentAuthority(view) })
    await waitFor(() => expect(screen.getByText('from session content')).toBeTruthy())
    view.unmount()
  })

  it('does not bootstrap or reload active runs when Session Index changes', async () => {
    const view = renderApp()
    await waitFor(() => expect(screen.getByText('session-1')).toBeTruthy())
    expect(mocks.api.bootstrap).toHaveBeenCalledTimes(1)

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
    view.unmount()
  })

  it('restores an active session only after an explicit archive-first delete failure', async () => {
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
    view.unmount()
  })

  it('waits for authoritative archive and delete changes in order, without restoring on unknown outcome', async () => {
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

  it('parses exact /new, keeps the draft on failure, and uses the typed create command', async () => {
    const { view, composer } = await renderContentReadyApp()
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'session.create') failProjectCommand(view.transport, message, 'session_unavailable')
    }
    fireEvent.change(composer, { target: { value: '  /new  ' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Session operation failed.'))
    expect(composer.value).toBe('  /new  ')
    expect(view.transport.sent.filter((message) => message.type === 'command').map((message) => message.payload.name)).toContain('session.create')
    view.unmount()
  })

  it('does not create two roots for a double submit and treats /new extra as normal input', async () => {
    const first = await renderContentReadyApp()
    let createRequest: ProtocolMessage | undefined
    first.view.transport.onSend = (message) => { if (message.type === 'command' && message.payload.name === 'session.create') createRequest = message }
    fireEvent.change(first.composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(createRequest).toBeTruthy())
    expect(first.view.transport.sent.filter((message) => message.type === 'command' && message.payload.name === 'session.create')).toHaveLength(1)
    first.view.unmount()

    const second = await renderContentReadyApp()
    fireEvent.change(second.composer, { target: { value: '/new extra' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(second.view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'run.start')).toBe(true))
    expect(second.view.transport.sent.some((message) => message.type === 'command' && message.payload.name === 'session.create')).toBe(false)
    second.view.unmount()
  })

  it('captures source configuration across a selection race while /new is admitted', async () => {
    const { view, composer } = await renderContentReadyApp()
    const other: SessionSummary = {
      session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'other session', archived: false,
      status: 'idle', run_id: null, resource_revision: '2', updated_at: '2026-01-02T00:00:00Z', has_unread_result: false,
    }
    await act(async () => { applySessionIndexAuthority(view, other, '2'); applySessionContentAuthorityFor(view, 'session-2', { display_name: 'other session' }, '1') })
    await screen.findByText('other session')
    let createRequest: ProtocolMessage | undefined
    view.transport.onSend = (message) => { if (message.type === 'command' && message.payload.name === 'session.create') createRequest = message }
    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(createRequest).toBeTruthy())
    const createdSessionID = createRequest?.type === 'command' ? String(createRequest.payload.arguments.session_id) : ''
    fireEvent.click(screen.getByText('other session'))
    await waitFor(() => expect(view.application.signals.currentSession.get()).toBe('session-2'))
    respondToSessionCreate(view, createRequest!)
    await waitFor(() => expect(view.application.signals.currentSession.get()).toBe(createdSessionID))
    expect(createRequest?.type === 'command' ? createRequest.payload.arguments : null).toMatchObject({
      provider: 'fake', model_profile: 'default', reasoning_level: 'medium', full_access: false,
      cwd: '/workspace/src', config_path: '/config',
    })
    view.unmount()
  })

  it('allows /new with optional Session Content fields absent and omits empty create arguments', async () => {
    const { view, composer } = await renderContentReadyApp()
    // This is a legal legacy/default-config projection. It deliberately has
    // no cwd fallback, provider/model selection, config path, or reasoning
    // level; the typed command must let the server choose its defaults.
    await act(async () => {
      applySessionContentAuthority(view, {
        provider: undefined,
        model_profile: undefined,
        model_id: undefined,
        cwd: undefined,
        created_cwd: undefined,
        config_path: undefined,
        reasoning_level: undefined,
      }, '2')
    })
    let createRequest: ProtocolMessage | undefined
    view.transport.onSend = (message) => {
      if (message.type === 'command' && message.payload.name === 'session.create') {
        createRequest = message
        respondToSessionCreate(view, message)
      }
    }
    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(createRequest).toBeTruthy())
    await waitFor(() => expect(view.application.signals.currentSession.get()).not.toBe('session-1'))
    const argumentsPayload = createRequest?.type === 'command' ? createRequest.payload.arguments : {}
    expect(argumentsPayload).toMatchObject({ project_id: 'project-1', full_access: false })
    expect(argumentsPayload).not.toHaveProperty('cwd')
    expect(argumentsPayload).not.toHaveProperty('config_path')
    expect(argumentsPayload).not.toHaveProperty('provider')
    expect(argumentsPayload).not.toHaveProperty('model_profile')
    expect(argumentsPayload).not.toHaveProperty('reasoning_level')
    expect(Object.values(argumentsPayload)).not.toContain('')
    view.unmount()
  })

  it("routes run admission exclusively through the typed command facade", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admittedRunID = ""
    view.transport.onSend = (message) => {
      if (message.type !== "command" || message.payload.name !== "run.start") return
      admittedRunID = String(message.payload.arguments.run_id)
      respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
    }
    fireEvent.change(composer, { target: { value: "typed prompt" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === "command" && message.payload.name === "run.start")).toBe(true))
    expect(view.container.querySelectorAll(".message.user")).toHaveLength(1)
    expect(composer.disabled).toBe(true)
    expect(admittedRunID).not.toBe("")
    view.unmount()
  })

  it("redacts typed run command failures and keeps the draft available", async () => {
    const { view, composer } = await renderContentReadyApp()
    view.transport.onSend = (message) => {
      if (message.type === "command" && message.payload.name === "run.start") failProjectCommand(view.transport, message, "admission_failed")
    }
    fireEvent.change(composer, { target: { value: "retryable prompt" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("Run operation failed."))
    expect(screen.queryByText("protocol-secret-not-for-ui")).toBeNull()
    expect(composer.value).toBe("retryable prompt")
    expect(composer.disabled).toBe(false)
    view.unmount()
  })
  it("releases admission only after Session Content publishes the matching run", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admittedRunID = ""
    view.transport.onSend = (message) => {
      if (message.type !== "command" || message.payload.name !== "run.start") return
      admittedRunID = String(message.payload.arguments.run_id)
      respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
    }
    fireEvent.change(composer, { target: { value: "typed prompt" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(admittedRunID).not.toBe(""))
    expect(composer.disabled).toBe(true)
    await act(async () => {
      // The navigation projection may learn the run before the selected
      // Session Content subscription does; that must not release the gate.
      applySessionIndexAuthority(view, {
        session_id: "session-1", project_id: "project-1", parent_session_id: null, display_name: "session-1", archived: false,
        status: "running", run_id: admittedRunID, resource_revision: "2", updated_at: "2026-01-01T00:00:02Z", has_unread_result: false,
      }, "2")
    })
    expect(composer.disabled).toBe(true)
    await act(async () => {
      view.application.replica.applyTransient(
        { type: "session_content", id: "session-1" },
        new SessionContentAdapter("session-1"),
        { type: "run.started", session_id: "session-1", run_id: admittedRunID, run_cursor: "1", status: "running", turn_id: "turn-new" } as never,
        1,
      )
    })
    await waitFor(() => expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy())
    expect(composer.disabled).toBe(false)
    view.unmount()
  })

  it("releases admission from matching terminal durable Session Content evidence", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admittedRunID = ""
    view.transport.onSend = (message) => {
      if (message.type !== "command" || message.payload.name !== "run.start") return
      admittedRunID = String(message.payload.arguments.run_id)
      respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
    }
    fireEvent.change(composer, { target: { value: "fast prompt" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(admittedRunID).not.toBe(""))
    expect(composer.disabled).toBe(true)
    await act(async () => {
      applySessionContentAuthorityFor(view, "session-1", { last_run_id: admittedRunID, latest_run_id: admittedRunID }, "2")
    })
    await waitFor(() => expect(composer.disabled).toBe(false))
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull()
    view.unmount()
  })

  it("releases admission when started, settled, and durable publication precede the ack", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admittedRunID = ""
    view.transport.onSend = (message) => {
      if (message.type !== "command" || message.payload.name !== "run.start") return
      admittedRunID = String(message.payload.arguments.run_id)
      applySessionContentAuthorityFor(view, "session-1", { last_run_id: admittedRunID, latest_run_id: admittedRunID }, "2")
      respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
    }
    fireEvent.change(composer, { target: { value: "already finished" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(admittedRunID).not.toBe(""))
    await waitFor(() => expect(composer.disabled).toBe(false))
    view.unmount()
  })

  it("does not release admission for another run or another Session Content resource", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admittedRunID = ""
    view.transport.onSend = (message) => {
      if (message.type !== "command" || message.payload.name !== "run.start") return
      admittedRunID = String(message.payload.arguments.run_id)
      respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
    }
    fireEvent.change(composer, { target: { value: "identity guard" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(admittedRunID).not.toBe(""))
    await act(async () => {
      applySessionContentAuthorityFor(view, "session-1", { last_run_id: "old-run", latest_run_id: "old-run" }, "2")
      applySessionContentAuthorityFor(view, "session-2", { last_run_id: admittedRunID, latest_run_id: admittedRunID }, "1")
    })
    expect(composer.disabled).toBe(true)
    view.unmount()
  })

  it("keeps an admitted run pending across navigation while it completes in the background", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admittedRunID = ""
    view.transport.onSend = (message) => {
      if (message.type !== "command" || message.payload.name !== "run.start") return
      admittedRunID = String(message.payload.arguments.run_id)
      respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
    }
    fireEvent.change(composer, { target: { value: "background completion" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(admittedRunID).not.toBe(""))
    const other: SessionSummary = {
      session_id: "session-2", project_id: "project-1", parent_session_id: null, display_name: "other session", archived: false,
      status: "idle", run_id: null, resource_revision: "1", updated_at: "2026-01-02T00:00:00Z", has_unread_result: false,
    }
    await act(async () => {
      applySessionIndexAuthority(view, other, "2")
      applySessionContentAuthorityFor(view, "session-2", { display_name: "other session" }, "1")
    })
    fireEvent.click((await screen.findAllByRole("button", { name: /other session/ }))[0])
    await waitFor(() => expect(screen.getByRole("heading", { name: "other session" })).toBeTruthy())
    await act(async () => {
      applySessionContentAuthorityFor(view, "session-1", { last_run_id: admittedRunID, latest_run_id: admittedRunID }, "3")
    })
    const session1Button = screen.getAllByRole("button", { name: /session-1/ }).find((button) => button.classList.contains("session-tree-button"))
    expect(session1Button).toBeTruthy()
    fireEvent.click(session1Button!)
    await waitFor(() => expect(view.application.signals.currentSession.get()).toBe("session-1"))
    await waitFor(() => expect(screen.getByRole("heading", { name: /sion-1/ })).toBeTruthy())
    await waitFor(() => expect((screen.getByRole("textbox") as HTMLTextAreaElement).disabled).toBe(false))
    view.unmount()
  })

  it("shows admission pending during an in-flight command and restores destructive controls after failure", async () => {
    const { view, composer } = await renderContentReadyApp()
    let admission: ProtocolMessage | undefined
    view.transport.onSend = (message) => {
      if (message.type === "command" && message.payload.name === "run.start") admission = message
    }
    fireEvent.change(composer, { target: { value: "deferred prompt" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await waitFor(() => expect(admission).toBeTruthy())
    expect(composer.disabled).toBe(true)
    expect((screen.getByRole("button", { name: "Archive session-1" }) as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByRole("button", { name: "Delete session-1" }) as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByRole("button", { name: "Delete project" }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(screen.getByRole("button", { name: "Archive session-1" }))
    fireEvent.click(screen.getByRole("button", { name: "Delete session-1" }))
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }))
    expect(view.transport.sent.filter((message) => message.type === "command" && /^(session|project)\./u.test(message.payload.name))).toHaveLength(0)
    failProjectCommand(view.transport, admission!, "admission_failed")
    await waitFor(() => expect(composer.disabled).toBe(false))
    expect(composer.value).toBe("deferred prompt")
    view.unmount()
  })

  it("keeps the second admission blocked after timeout and releases it after automatic resync evidence", async () => {
    const { view, composer } = await renderContentReadyApp()
    vi.useFakeTimers()
    let admittedRunID = ""
    let resyncSubscriptionID = ""
    view.transport.onSend = (message) => {
      if (message.type === "command" && message.payload.name === "run.start") {
        admittedRunID = String(message.payload.arguments.run_id)
        respondToProjectCommand(view.transport, message, { session_id: "session-1", run_id: admittedRunID, status: "running" })
      }
      if (message.type === "subscribe" && message.payload.resource.type === "session_content" && message.payload.resource.id === "session-1") {
        resyncSubscriptionID = message.payload.subscription_id
      }
    }
    fireEvent.change(composer, { target: { value: "lost publication" } })
    fireEvent.click(screen.getByRole("button", { name: "Send" }))
    await act(async () => { await Promise.resolve(); await Promise.resolve() })
    expect(admittedRunID).not.toBe("")
    await act(async () => { await vi.advanceTimersByTimeAsync(5000); await Promise.resolve(); await Promise.resolve() })
    expect(screen.getByRole("alert").textContent).toContain("Run accepted; automatic synchronization is in progress.")
    expect(composer.disabled).toBe(true)
    expect(view.transport.sent.filter((message) => message.type === "command" && message.payload.name === "run.start")).toHaveLength(1)
    expect(resyncSubscriptionID).not.toBe("")
    expect(view.transport.sent.filter((message) => message.type === "unsubscribe" && message.payload.subscription_id !== undefined)).toHaveLength(1)
    await act(async () => {
      view.transport.emit(sessionContentSubscribedMessage(resyncSubscriptionID, "session-1"))
      view.transport.emit(sessionContentSnapshotMessage(resyncSubscriptionID, "session-1", admittedRunID))
      await Promise.resolve()
      await Promise.resolve()
    })
    expect(composer.disabled).toBe(false)
    expect(screen.queryByText("Run accepted; automatic synchronization is in progress.")).toBeNull()
    view.unmount()
  })

  it("keeps run controls command-only and does not patch Session Content locally", async () => {
    const { view, composer } = await renderContentReadyApp()
    const activeRun = { run_id: "control-run", session_id: "session-1", turn_id: "turn-control", started_at: "2026-01-01T00:00:00Z", status: "running", recoverable: true, run_epoch: "epoch-control", run_cursor: "0", replay_available: false, recovery_required: false }
    await act(async () => { applySessionContentAuthorityFor(view, "session-1", { status: "running", running_run_id: "control-run", running_turn_id: "turn-control" }, "2", undefined, activeRun) })
    const applyContentEvent = (event: Record<string, unknown>) => view.application.replica.applyTransient(
      { type: "session_content", id: "session-1" }, new SessionContentAdapter("session-1"), event as never, 1,
    )
    view.transport.onSend = (message) => {
      if (message.type !== "command") return
      const args = message.payload.arguments as Record<string, unknown>
      let result: Record<string, unknown> = { session_id: "session-1", run_id: String(args.run_id ?? ""), accepted: true }
      if (message.payload.name === "run.prompt.append") result = { operation_id: String(args.operation_id), session_id: "session-1", run_id: String(args.run_id), accepted: true }
      if (message.payload.name === "run.prompt.remove") result = { session_id: "session-1", run_id: String(args.run_id), prompt_id: String(args.prompt_id), removed: true }
      if (message.payload.name === "run.tool.cancel") result = { session_id: "session-1", run_id: String(args.run_id), tool_call_id: String(args.tool_call_id), cancelled: true }
      view.transport.emit({ version: 1, type: "command_result", id: `control-result-${message.payload.request_id}`, payload: { request_id: message.payload.request_id, status: "succeeded", result } } as unknown as ProtocolMessage)
    }
    await act(async () => {
      applyContentEvent({ type: "tool.requested", session_id: "session-1", run_id: "control-run", run_cursor: "1", turn_id: "turn-control", agent_iteration: 1, tool_call_id: "tool-1", name: "shell", arguments: "{}" })
      applyContentEvent({ type: "tool.running", session_id: "session-1", run_id: "control-run", run_cursor: "2", turn_id: "turn-control", agent_iteration: 1, tool_call_id: "tool-1", name: "shell", arguments: "{}" })
      applyContentEvent({ type: "run.prompt_queue", session_id: "session-1", run_id: "control-run", run_cursor: "3", prompts: [{ id: "prompt-1", content: "queued control", steer: false }] })
    })
    await waitFor(() => expect(screen.getByText("queued control")).toBeTruthy())
    fireEvent.change(composer, { target: { value: "follow-up" } })
    fireEvent.click(screen.getByRole("button", { name: "Append to current run" }))
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === "command" && message.payload.name === "run.prompt.append")).toBe(true))
    fireEvent.click(screen.getByRole("button", { name: "Stop" }))
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === "command" && message.payload.name === "run.cancel")).toBe(true))
    fireEvent.click(screen.getByRole("button", { name: "Remove queued message" }))
    await waitFor(() => expect(view.transport.sent.some((message) => message.type === "command" && message.payload.name === "run.prompt.remove")).toBe(true))
    view.unmount()
  })


  it('keeps background Session Index completion independent of current Session Content reads', async () => {
    const { view } = await renderContentReadyApp()
    const background: SessionSummary = { session_id: 'session-2', project_id: 'project-1', parent_session_id: null, display_name: 'background', archived: false, status: 'running', run_id: 'background-run', resource_revision: '1', updated_at: '2026-01-01T00:00:01Z', has_unread_result: false }
    await act(async () => { applySessionIndexAuthority(view, background, '1') })
    await act(async () => { applySessionIndexAuthority(view, { ...background, status: 'completed', resource_revision: '2', has_unread_result: true }, '2') })
    await waitFor(() => expect(screen.getByText('background completed in the background.')).toBeTruthy())
    expect(view.application.repositories.sessionContent.get('session-1').session?.id).toBe('session-1')
    view.unmount()
  })


})
