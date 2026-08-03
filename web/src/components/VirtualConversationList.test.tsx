// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import type { ConversationRow } from '../lib/conversationRows'

const mockState = vi.hoisted(() => ({
  handles: [] as Array<Record<string, unknown>>,
  scrollCalls: [] as Array<ScrollToOptions>,
  scrollToIndexCalls: [] as Array<unknown>,
}))

vi.mock('react-virtuoso', async () => {
  const React = await import('react')
  const MockVirtuoso = React.forwardRef<any, any>(function MockVirtuoso(props, ref) {
    const scrollerRef = React.useRef<HTMLDivElement | null>(null)
    const handle = React.useMemo(() => ({
      autoscrollToBottom: () => {},
      getState: (callback: (state: unknown) => void) => callback({ ranges: [{ startIndex: props.firstItemIndex, endIndex: props.firstItemIndex + props.data.length - 1 }], scrollTop: scrollerRef.current?.scrollTop ?? 0 }),
      scrollBy: () => {},
      scrollIntoView: () => {},
      scrollTo: (location: ScrollToOptions) => {
        mockState.scrollCalls.push(location)
        if (scrollerRef.current && location.top !== undefined) {
          scrollerRef.current.scrollTop = location.top
          scrollerRef.current.dispatchEvent(new Event('scroll'))
        }
      },
      scrollToIndex: (location: unknown) => {
        mockState.scrollToIndexCalls.push(location)
      },
    }), [])
    React.useImperativeHandle(ref, () => handle, [handle])
    React.useLayoutEffect(() => {
      const scroller = scrollerRef.current
      if (!scroller) return
      Object.defineProperty(scroller, 'scrollHeight', { configurable: true, value: 1000 })
      Object.defineProperty(scroller, 'clientHeight', { configurable: true, value: 100 })
      props.scrollerRef?.(scroller)
      const items = (props.data ?? []).map((data: unknown, index: number) => ({
        data,
        index: props.firstItemIndex + index,
        offset: index * 100,
        size: 100,
      }))
      props.itemsRendered?.(items)
      props.rangeChanged?.({ startIndex: props.firstItemIndex })
      return () => props.scrollerRef?.(null)
    }, [props.data, props.firstItemIndex, props.itemsRendered, props.rangeChanged, props.scrollerRef])
    React.useEffect(() => {
      props.totalListHeightChanged?.()
    }, [props.totalListHeightChanged])
    React.useEffect(() => {
      mockState.handles.push(handle)
      return () => {}
    }, [handle])

    const Scroller = props.components?.Scroller ?? 'div'
    const Header = props.components?.Header
    const Footer = props.components?.Footer
    const List = props.components?.List ?? 'div'
    const EmptyPlaceholder = props.components?.EmptyPlaceholder
    const data = props.data ?? []
    return React.createElement(
      Scroller,
      { ref: scrollerRef, 'data-testid': 'mock-scroller', 'data-first-item-index': String(props.firstItemIndex) },
      Header ? React.createElement(Header) : null,
      React.createElement(
        List,
        null,
        data.length > 0
          ? data.map((row: unknown, index: number) => React.createElement(React.Fragment, { key: props.computeItemKey?.(index, row) ?? index }, props.itemContent?.(index, row)))
          : EmptyPlaceholder ? React.createElement(EmptyPlaceholder) : null,
      ),
      Footer ? React.createElement(Footer) : null,
    )
  })
  return { Virtuoso: MockVirtuoso }
})

import { VirtualConversationList } from './VirtualConversationList'
import type { VirtualConversationListProps } from './VirtualConversationList'

afterEach(() => {
  cleanup()
  mockState.handles.length = 0
  mockState.scrollCalls.length = 0
  mockState.scrollToIndexCalls.length = 0
})

const row = (key: string): ConversationRow => ({ kind: 'bottom-spacer', key })

function props(overrides: Partial<VirtualConversationListProps> = {}) {
  return {
    sessionID: 'session-1',
    rows: [row('a'), row('b')],
    header: <div>header</div>,
    footer: <div>footer</div>,
    emptyPlaceholder: <div>empty state</div>,
    renderRow: (value: ConversationRow): ReactNode => <span>{value.key}</span>,
    followOutput: () => false as const,
    onInteraction: vi.fn(),
    onAtBottomStateChange: vi.fn(),
    onTotalListHeightChanged: vi.fn(),
    onVirtuosoRef: vi.fn(),
    ...overrides,
  }
}

describe('VirtualConversationList controller', () => {
  it('keeps the virtualized scroll region out of the live announcement stream', () => {
    render(<VirtualConversationList {...props()} />)
    const scroller = screen.getByTestId('mock-scroller')

    expect(scroller.getAttribute('role')).toBe('region')
    expect(scroller.getAttribute('aria-label')).toBe('Conversation')
    expect(scroller.getAttribute('aria-live')).toBe('off')
    expect(scroller.getAttribute('tabindex')).toBe('0')
  })

  it('does not detach on pointer focus alone, but detaches after a user scroll', () => {
    const onInteraction = vi.fn()
    render(<VirtualConversationList {...props({ onInteraction })} />)
    const scroller = screen.getByTestId('mock-scroller')

    fireEvent.pointerDown(scroller)
    fireEvent.pointerUp(scroller)
    expect(onInteraction).not.toHaveBeenCalled()

    scroller.scrollTop = 40
    fireEvent.scroll(scroller)
    expect(onInteraction).not.toHaveBeenCalled()

    fireEvent.pointerDown(scroller)
    scroller.scrollTop = 80
    fireEvent.scroll(scroller)
    expect(onInteraction).toHaveBeenCalledTimes(1)
  })

  it('keeps the same Virtuoso instance when the first real page replaces an empty list', () => {
    const onVirtuosoRef = vi.fn()
    const initial = props({ rows: [], onVirtuosoRef })
    const { rerender } = render(<VirtualConversationList {...initial} />)
    const firstHandle = onVirtuosoRef.mock.calls.map((call) => call[1]).find(Boolean)

    rerender(<VirtualConversationList {...props({ rows: [row('first')], onVirtuosoRef })} />)

    const handles = onVirtuosoRef.mock.calls.map((call) => call[1]).filter(Boolean)
    expect(handles.at(-1)).toBe(firstHandle)
  })

  it('keeps the empty state in Virtuoso and decrements absolute firstItemIndex on prepend', () => {
    const initial = props({ rows: [], emptyPlaceholder: <div>empty state</div> })
    const { rerender } = render(<VirtualConversationList {...initial} />)
    expect(screen.getByText('empty state')).toBeDefined()

    rerender(<VirtualConversationList {...props({ rows: [row('a'), row('b')] })} />)
    rerender(<VirtualConversationList {...props({ rows: [row('new'), row('a'), row('b')] })} />)
    expect(screen.getByTestId('mock-scroller').getAttribute('data-first-item-index')).toBe('999999')
  })

  it('restores an absolute anchor through Virtuoso imperative methods', () => {
    const rowsA = [row('a'), row('b')]
    const rowsB = [row('other-a'), row('other-b')]
    const first = props({ rows: rowsA })
    const { rerender } = render(<VirtualConversationList {...first} />)
    const initialScroller = screen.getByTestId('mock-scroller')
    initialScroller.scrollTop = 240
    fireEvent.scroll(initialScroller)

    rerender(<VirtualConversationList {...props({ sessionID: 'session-2', rows: rowsB })} />)
    rerender(<VirtualConversationList {...props({ sessionID: 'session-1', rows: rowsA })} />)

    expect(mockState.scrollToIndexCalls.at(-1)).toMatchObject({ index: 0, behavior: 'auto', align: 'start' })
    const restoredScroller = screen.getByTestId('mock-scroller')
    restoredScroller.scrollTop = 100
    fireEvent.scroll(restoredScroller)
    expect(mockState.scrollCalls.at(-1)).toMatchObject({ top: 240, behavior: 'auto' })
  })
})
