// @vitest-environment jsdom
import { act, render, screen, waitFor } from '@testing-library/react'
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
})

