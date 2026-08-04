// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App'

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
  }
  const api = {
    bootstrap: vi.fn().mockResolvedValue({ version: 'test', cwd: '/workspace', server_root: '/workspace', config_path: '/config' }),
    projects: vi.fn().mockResolvedValue({ projects: [{ id: 'project-1', root: '/workspace', display_name: 'project', archived: false, created_at: '', updated_at: '' }] }),
    activeRuns: vi.fn().mockResolvedValue({ runs: [{ run_id: 'run-1', session_id: 'session-1', turn_id: 'turn-1', started_at: '', status: 'running' }] }),
    sessions: vi.fn().mockResolvedValue({ sessions: [session] }),
    snapshot: vi.fn().mockImplementation(() => new Promise(() => {})),
    startRun: vi.fn(),
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
  afterEach(() => {
    vi.useRealTimers()
    vi.clearAllMocks()
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

  async function renderSubmitReadyApp(activeRuns: Array<{ run_id: string; session_id: string; turn_id?: string; started_at: string; status: string }> = []) {
    mocks.api.activeRuns.mockResolvedValue({ runs: activeRuns })
    mocks.api.snapshot.mockResolvedValue(emptySnapshot())
    mocks.streamRun.mockReset()
    mocks.streamRun.mockResolvedValue(undefined)
    const view = render(<App />)
    const composer = await screen.findByRole('textbox')
    return { view, composer }
  }

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
    expect(screen.queryByText('Generating')).toBeNull()
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
    expect(screen.getByText('Generating')).toBeTruthy()
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
    await waitFor(() => expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(1))

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

    // Admission never owned existing-run, so its transient failure state is
    // still present after the rejected attempt.
    expect(view.container.querySelectorAll('.message.assistant.transient')).toHaveLength(1)
    view.unmount()
  })
})

