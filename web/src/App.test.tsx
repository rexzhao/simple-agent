// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'
import { frontendProtocolLogger } from './lib/frontendProtocolLogger'

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
  })

  it('does not poll sessions or active runs while a run remains active', async () => {
    const view = render(<App />)
    await waitFor(() => expect(mocks.api.sessions).toHaveBeenCalledTimes(2))

    vi.useFakeTimers()
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(mocks.api.activeRuns).toHaveBeenCalledTimes(1)
    expect(mocks.api.sessions).toHaveBeenCalledTimes(2)
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

    const view = render(<App />)
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
    const view = render(<App />)
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

    await waitFor(() => expect(mocks.api.createSession).toHaveBeenCalledTimes(1))
    expect(mocks.api.createSession).toHaveBeenCalledWith({
      projectID: 'project-1',
      provider: 'fake',
      modelProfile: 'default',
      reasoningLevel: 'high',
      fullAccess: true,
      cwd: '/workspace/src',
      configPath: '/config',
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
    await waitFor(() => expect(mocks.api.sessions.mock.calls.length).toBeGreaterThanOrEqual(6))
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
    expect(mocks.api.createSession).toHaveBeenCalledWith({
      projectID: 'project-1',
      provider: 'authoritative-provider',
      modelProfile: 'authoritative-model',
      reasoningLevel: 'low',
      fullAccess: true,
      cwd: '/workspace/authoritative',
      configPath: '/config/authoritative.yaml',
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

    await waitFor(() => expect(mocks.api.createSession).toHaveBeenCalledTimes(1))
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
    mocks.api.createSession.mockRejectedValue(new Error('create failed'))
    const { view, composer } = await renderSubmitReadyApp()

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('create failed'))
    expect((composer as HTMLTextAreaElement).value).toBe('/new')
    expect(mocks.api.startRun).not.toHaveBeenCalled()
    view.unmount()
  })

  it('does not create two roots when /new is submitted twice before the first response', async () => {
    const created = configureCreatedRoot()
    let resolveCreate!: (session: typeof created) => void
    mocks.api.createSession.mockImplementation(() => new Promise((resolve) => { resolveCreate = resolve }))
    const { view, composer } = await renderSubmitReadyApp()

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(mocks.api.createSession).toHaveBeenCalledTimes(1))

    resolveCreate(created)
    await waitFor(() => expect(screen.getByText('new root')).toBeTruthy())
    expect(mocks.api.createSession).toHaveBeenCalledTimes(1)
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
    let createdAvailable = false
    mocks.api.sessions.mockImplementation(async (_projectID: string, archived = false) => ({
      sessions: archived ? [] : [mocks.session, other, ...(createdAvailable ? [created] : [])],
    }))
    let resolveCreate!: (session: typeof created) => void
    mocks.api.createSession.mockImplementation(() => new Promise((resolve) => { resolveCreate = resolve }))
    const { view, composer } = await renderSubmitReadyApp([], { ...emptySnapshot(), session: source })

    fireEvent.change(composer, { target: { value: '/new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(mocks.api.createSession).toHaveBeenCalledTimes(1))

    mocks.api.snapshot.mockResolvedValue({
      ...emptySnapshot(),
      session_id: 'session-2',
      session: { ...other, revision: '0' },
    })
    const otherButton = screen.getByText('other session').closest('button')
    expect(otherButton).not.toBeNull()
    fireEvent.click(otherButton!)
    await waitFor(() => expect(mocks.api.snapshot).toHaveBeenCalledWith('session-2'))

    expect(mocks.api.createSession).toHaveBeenCalledWith({
      projectID: 'project-1',
      provider: 'source-provider',
      modelProfile: 'source-model',
      reasoningLevel: 'source-level',
      fullAccess: true,
      cwd: '/workspace/source',
      configPath: '/config/source.yaml',
    })
    createdAvailable = true
    resolveCreate(created)
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
    const view = render(<App />)
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

    const view = render(<App />)
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

    const view = render(<App />)
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

  it('does not refresh the selected session for a covered background settlement', async () => {
    const background = { ...mocks.session, id: 'session-2', display_name: 'background-session', revision: '5', last_seq: 5 }
    mocks.api.sessions.mockImplementation(async (_projectID: string, archived = false) => ({ sessions: archived ? [] : [mocks.session, background] }))
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
    const view = render(<App />)
    await screen.findByRole('textbox')
    fireEvent.click(screen.getByText('background-session'))
    await waitFor(() => expect(mocks.api.snapshot).toHaveBeenCalledTimes(2))
    fireEvent.click(screen.getByText('session-1'))
    await waitFor(() => expect(mocks.api.snapshot).toHaveBeenCalledTimes(3))
    const snapshotsBeforeSettlement = mocks.api.snapshot.mock.calls.length
    const lifecycleHandler = mocks.streamLifecycle.mock.calls[0][0] as (event: unknown) => Promise<void>
    await act(async () => {
      await lifecycleHandler({
        type: 'run.started', session_id: 'session-2', run_id: 'background-run', status: 'running', session: background,
      })
    })
    await waitFor(() => expect(mocks.streamRun).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('background-session completed in the background'))
    expect(mocks.api.snapshot).toHaveBeenCalledTimes(snapshotsBeforeSettlement)
    view.unmount()
  })
})

