import { createContext, forwardRef, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useReducer, useRef } from 'react'
import type { ReactNode } from 'react'
import { Virtuoso } from 'react-virtuoso'
import type { Components, ListItem, ListProps, ScrollerProps, StateSnapshot, VirtuosoHandle } from 'react-virtuoso'
import type { ConversationRow } from '../lib/conversationRows'
import { getConversationFirstItemIndex } from '../lib/conversationRows'

const Scroller = forwardRef<HTMLDivElement, ScrollerProps>(function MessagesScroller({ children, style, tabIndex, ...props }, ref) {
  return (
    <div
      {...props}
      ref={ref}
      className="messages"
      style={style}
      tabIndex={tabIndex ?? 0}
      role="region"
      aria-label="Conversation"
      aria-live="off"
    >
      {children}
    </div>
  )
})
Scroller.displayName = 'MessagesScroller'

const List = forwardRef<HTMLDivElement, ListProps>(function MessagesList({ children, style, ...props }, ref) {
  return (
    <div {...props} ref={ref} className="messages-list" style={style}>
      {children}
    </div>
  )
})
List.displayName = 'MessagesList'

type ConversationListContent = {
  header?: ReactNode
  footer?: ReactNode
  emptyPlaceholder?: ReactNode
}

// Virtuoso treats component types as part of its measured layout. Keep these
// types stable while the content is changing: the session-specific React
// nodes are supplied through context rather than by creating a new Header or
// Footer function on every Conversation render.
const ConversationListContentContext = createContext<ConversationListContent>({})

function ConversationHeader() {
  const { header } = useContext(ConversationListContentContext)
  return <div className="messages-header"><div className="messages-top-padding" aria-hidden="true" /><div className="messages-header-content">{header}</div></div>
}

function ConversationFooter() {
  const { footer } = useContext(ConversationListContentContext)
  return <div className="messages-footer">{footer}</div>
}

function ConversationEmptyPlaceholder() {
  return <>{useContext(ConversationListContentContext).emptyPlaceholder}</>
}

const virtuosoComponents: Components<ConversationRow> = {
  Scroller,
  Header: ConversationHeader,
  Footer: ConversationFooter,
  EmptyPlaceholder: ConversationEmptyPlaceholder,
}

// Virtuoso item offsets include this fixed header, while the saved anchor
// offset is measured against the visible .messages viewport.
const messagesHeaderHeight = 94

export interface VirtualConversationListProps {
  sessionID: string
  rows: ConversationRow[]
  header?: ReactNode
  footer?: ReactNode
  emptyPlaceholder?: ReactNode
  renderRow: (row: ConversationRow) => ReactNode
  followOutput: (sessionID: string, isAtBottom: boolean) => 'auto' | false
  onInteraction: (sessionID: string) => void
  onAtBottomStateChange: (sessionID: string, atBottom: boolean, fromScroll?: boolean) => void
  onTotalListHeightChanged: (sessionID: string) => void
  onVirtuosoRef: (sessionID: string, handle: VirtuosoHandle | null) => void
}

type SessionListState = {
  rows: ConversationRow[]
  firstItemIndex: number
}

type SessionListRecord = {
  model: SessionListState
  snapshot?: StateSnapshot
  anchor?: SessionScrollAnchor
}

type SessionScrollAnchor = {
  index: number
  offset: number
}

function compatibleSessionModels(previous: SessionListState, next: SessionListState): boolean {
  return previous.firstItemIndex === next.firstItemIndex
    && previous.rows.length === next.rows.length
    && previous.rows.every((row, index) => row.key === next.rows[index]?.key)
}

/**
 * The list owns the Virtuoso index bookkeeping. The reducer is the committed
 * per-session model, while the pure next-model calculation lets a prepend
 * provide rows and its matching firstItemIndex in the same render. Session
 * records are committed in a layout effect, never from render or the reducer.
 */
export function VirtualConversationList(props: VirtualConversationListProps) {
  const sessionRecordsRef = useRef(new Map<string, SessionListRecord>())
  const [sessionStates, syncSessionState] = useReducer(
    (state: Map<string, SessionListState>, action: { sessionID: string; next: SessionListState }) => {
      if (state.get(action.sessionID) === action.next) return state
      const next = new Map(state)
      next.set(action.sessionID, action.next)
      return next
    },
    props,
    (initialProps) => {
      const model = { rows: initialProps.rows, firstItemIndex: 1_000_000 }
      return new Map([[initialProps.sessionID, model]])
    },
  )
  const saveState = useCallback((sessionID: string, model: SessionListState, state: StateSnapshot, anchor?: SessionScrollAnchor) => {
    const record = sessionRecordsRef.current.get(sessionID)
    if (!record) {
      sessionRecordsRef.current.set(sessionID, { model, snapshot: state, anchor })
      return
    }
    if (compatibleSessionModels(record.model, model)) {
      record.snapshot = state
      if (anchor) record.anchor = anchor
    }
  }, [])
  const previous = sessionStates.get(props.sessionID)
  const model = useMemo(() => {
    if (!previous || previous.rows === props.rows) return previous ?? { rows: props.rows, firstItemIndex: 1_000_000 }
    return {
      rows: props.rows,
      firstItemIndex: getConversationFirstItemIndex(previous.rows, props.rows, previous.firstItemIndex),
    }
  }, [previous, props.rows])
  const currentRecord = sessionRecordsRef.current.get(props.sessionID)
  const savedState = currentRecord && compatibleSessionModels(currentRecord.model, model)
    ? currentRecord.snapshot
    : undefined
  const savedAnchor = currentRecord && compatibleSessionModels(currentRecord.model, model)
    ? currentRecord.anchor
    : undefined

  useLayoutEffect(() => {
    const record = sessionRecordsRef.current.get(props.sessionID)
    if (!record) {
      sessionRecordsRef.current.set(props.sessionID, { model })
    } else if (record.model !== model) {
      // Keep record mutation out of render and the reducer. StrictMode may
      // invoke both more than once; this effect is the single commit point
      // for snapshot invalidation.
      const compatible = compatibleSessionModels(record.model, model)
      record.model = model
      if (!compatible) {
        record.snapshot = undefined
        record.anchor = undefined
      }
    }
    syncSessionState({ sessionID: props.sessionID, next: model })
  }, [model, props.sessionID])

  return (
    <ConversationListContentContext.Provider value={{ header: props.header, footer: props.footer, emptyPlaceholder: props.emptyPlaceholder }}>
      <SessionVirtuoso
        // A session switch needs a fresh Virtuoso instance so its internal
        // size state cannot cross session boundaries. Do not remount merely
        // because the first real page arrived.
        key={props.sessionID}
        sessionID={props.sessionID}
        rows={model.rows}
        firstItemIndex={model.firstItemIndex}
        savedState={savedState}
        components={virtuosoComponents}
        renderRow={props.renderRow}
        followOutput={props.followOutput}
        onInteraction={props.onInteraction}
        onAtBottomStateChange={props.onAtBottomStateChange}
        onTotalListHeightChanged={props.onTotalListHeightChanged}
        onVirtuosoRef={props.onVirtuosoRef}
        onState={saveState}
        model={model}
        savedAnchor={savedAnchor}
      />
    </ConversationListContentContext.Provider>
  )
}

function SessionVirtuoso(props: {
  sessionID: string
  rows: ConversationRow[]
  firstItemIndex: number
  savedState?: StateSnapshot
  components: Components<ConversationRow>
  renderRow: (row: ConversationRow) => ReactNode
  followOutput: (sessionID: string, isAtBottom: boolean) => 'auto' | false
  onInteraction: (sessionID: string) => void
  onAtBottomStateChange: (sessionID: string, atBottom: boolean, fromScroll?: boolean) => void
  onTotalListHeightChanged: (sessionID: string) => void
  onVirtuosoRef: (sessionID: string, handle: VirtuosoHandle | null) => void
  onState: (sessionID: string, model: SessionListState, state: StateSnapshot, anchor?: SessionScrollAnchor) => void
  model: SessionListState
  savedAnchor?: SessionScrollAnchor
}) {
  const virtuosoRef = useRef<VirtuosoHandle>(null)
  const scrollerRef = useRef<HTMLElement | null>(null)
  const lastScrollTopRef = useRef<number | null>(null)
  const lastAtBottomRef = useRef<boolean | null>(null)
  const pendingCaptureFrameRef = useRef<number | null>(null)
  const userScrollCandidateRef = useRef(false)
  const candidateExpiryTimerRef = useRef<number | null>(null)
  const savedState = props.savedState?.ranges.length ? props.savedState : undefined
  const restorePendingRef = useRef(Boolean(savedState))
  const anchorRef = useRef<SessionScrollAnchor | null>(null)
  const renderedItemsRef = useRef<ListItem<ConversationRow>[]>([])
  const rangeStartRef = useRef<number | null>(null)
  const restoreCorrectionRef = useRef<{ index: number; messageOffset: number } | null>(null)

  const captureState = useCallback(() => {
    // The single bottom helper row is the empty-session model. Do not let its
    // provisional measurement become the restore snapshot for the first real
    // page that arrives.
    if (props.rows.length <= 1 || restorePendingRef.current) return
    virtuosoRef.current?.getState((state) => props.onState(props.sessionID, props.model, state, anchorRef.current ?? undefined))
  }, [props.model, props.onState, props.rows.length, props.sessionID])

  const captureStateAfterScroll = useCallback(() => {
    captureState()
    if (typeof requestAnimationFrame === 'undefined') return
    if (pendingCaptureFrameRef.current !== null) cancelAnimationFrame(pendingCaptureFrameRef.current)
    pendingCaptureFrameRef.current = requestAnimationFrame(() => {
      pendingCaptureFrameRef.current = null
      captureState()
    })
  }, [captureState])

  const handleVirtuosoRef = useCallback((handle: VirtuosoHandle | null) => {
    virtuosoRef.current = handle
    props.onVirtuosoRef(props.sessionID, handle)
  }, [props.onVirtuosoRef, props.sessionID])
  const handleInteraction = useCallback(() => {
    userScrollCandidateRef.current = true
    if (candidateExpiryTimerRef.current !== null) clearTimeout(candidateExpiryTimerRef.current)
    candidateExpiryTimerRef.current = window.setTimeout(() => {
      candidateExpiryTimerRef.current = null
      userScrollCandidateRef.current = false
    }, 250)
  }, [])
  const handlePersistentInteraction = useCallback(() => {
    userScrollCandidateRef.current = true
    if (candidateExpiryTimerRef.current !== null) {
      clearTimeout(candidateExpiryTimerRef.current)
      candidateExpiryTimerRef.current = null
    }
  }, [])
  const clearInteractionCandidate = useCallback(() => {
    userScrollCandidateRef.current = false
    if (candidateExpiryTimerRef.current !== null) {
      clearTimeout(candidateExpiryTimerRef.current)
      candidateExpiryTimerRef.current = null
    }
  }, [])
  const cancelRestore = useCallback(() => {
    restorePendingRef.current = false
    restoreCorrectionRef.current = null
  }, [])
  const correctRestoredAnchor = useCallback(() => {
    const correction = restoreCorrectionRef.current
    const scroller = scrollerRef.current
    if (!correction || !scroller) return
    const item = renderedItemsRef.current.find((entry) => entry.index === correction.index)
    if (!item) return
    const targetScrollTop = item.offset + messagesHeaderHeight - correction.messageOffset
    if (Math.abs(scroller.scrollTop - targetScrollTop) <= 1) {
      restoreCorrectionRef.current = null
      return
    }
    virtuosoRef.current?.scrollTo({ top: targetScrollTop, behavior: 'auto' })
  }, [])
  const updateAnchor = useCallback(() => {
    const scroller = scrollerRef.current
    const rangeStart = rangeStartRef.current
    if (!scroller || rangeStart === null) return
    const visible = renderedItemsRef.current.find((item) => item.index === rangeStart)
    if (visible) anchorRef.current = { index: visible.index, offset: visible.offset - scroller.scrollTop }
  }, [])
  const handleScrollerScroll = useCallback(() => {
    const scroller = scrollerRef.current
    if (!scroller) return
    const previousScrollTop = lastScrollTopRef.current
    lastScrollTopRef.current = scroller.scrollTop
    if (previousScrollTop === scroller.scrollTop) return
    const fromUserScroll = userScrollCandidateRef.current
    if (fromUserScroll) {
      clearInteractionCandidate()
      cancelRestore()
      props.onInteraction(props.sessionID)
    } else {
      correctRestoredAnchor()
    }
    updateAnchor()
    captureStateAfterScroll()
    const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight <= 8
    if (lastAtBottomRef.current === atBottom) return
    lastAtBottomRef.current = atBottom
    // The third argument describes a scroll signal, not its origin. User
    // origin is reported separately through onInteraction; keeping this true
    // lets a detached session re-engage when an explicit scroll lands at the
    // bottom, including imperative positioning used by the UI.
    props.onAtBottomStateChange(props.sessionID, atBottom, true)
  }, [cancelRestore, captureStateAfterScroll, clearInteractionCandidate, correctRestoredAnchor, props.onAtBottomStateChange, props.onInteraction, props.sessionID, updateAnchor])
  const handleItemsRendered = useCallback((items: ListItem<ConversationRow>[]) => {
    renderedItemsRef.current = items
    updateAnchor()
    correctRestoredAnchor()
  }, [correctRestoredAnchor, updateAnchor])
  const handleRangeChanged = useCallback((range: { startIndex: number }) => {
    rangeStartRef.current = range.startIndex
    updateAnchor()
    correctRestoredAnchor()
  }, [correctRestoredAnchor, updateAnchor])
  const handleTotalListHeightChanged = useCallback(() => {
    captureState()
    correctRestoredAnchor()
    const scroller = scrollerRef.current
    if (restorePendingRef.current && savedState && scroller && scroller.scrollHeight >= savedState.scrollTop + scroller.clientHeight) {
      restorePendingRef.current = false
      if (props.savedAnchor) {
        restoreCorrectionRef.current = {
          index: props.savedAnchor.index,
          messageOffset: props.savedAnchor.offset + messagesHeaderHeight,
        }
        virtuosoRef.current?.scrollToIndex({
          index: props.savedAnchor.index - props.firstItemIndex,
          align: 'start',
          offset: 0,
          behavior: 'auto',
        })
      } else {
        virtuosoRef.current?.scrollTo({ top: savedState.scrollTop, behavior: 'auto' })
      }
    }
    props.onTotalListHeightChanged(props.sessionID)
  }, [captureState, correctRestoredAnchor, props.firstItemIndex, props.onTotalListHeightChanged, props.savedAnchor, props.sessionID, savedState])

  const handlePointerDown = useCallback((event: PointerEvent) => {
    const target = event.target
    if (target instanceof Element && target.closest('button, a, input, select, textarea, [contenteditable="true"]')) return
    // A pointer down is only a candidate. Detachment happens after a scroll
    // event with a changed offset, so clicking a row or the scrollbar track
    // without moving it does not detach the follow state.
    handlePersistentInteraction()
  }, [handlePersistentInteraction])
  const handleKeyDown = useCallback((event: KeyboardEvent) => {
    if (!['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' ', 'Spacebar'].includes(event.key)) return
    const target = event.target
    if (target instanceof Element && target.closest('button, a, input, select, textarea, [contenteditable="true"]')) return
    handlePersistentInteraction()
  }, [handlePersistentInteraction])
  const handlePointerEnd = useCallback(() => {
    clearInteractionCandidate()
  }, [clearInteractionCandidate])
  const handleKeyUp = useCallback(() => {
    clearInteractionCandidate()
  }, [clearInteractionCandidate])

  useEffect(() => {
    const scroller = scrollerRef.current
    if (!scroller) return
    scroller.addEventListener('scroll', handleScrollerScroll, { passive: true })
    scroller.addEventListener('wheel', handleInteraction, { passive: true })
    scroller.addEventListener('touchmove', handleInteraction, { passive: true })
    scroller.addEventListener('pointerdown', handlePointerDown, { passive: true })
    scroller.addEventListener('pointerup', handlePointerEnd, { passive: true })
    scroller.addEventListener('pointercancel', handlePointerEnd, { passive: true })
    scroller.addEventListener('keydown', handleKeyDown, { passive: true })
    scroller.addEventListener('keyup', handleKeyUp, { passive: true })
    return () => {
      scroller.removeEventListener('scroll', handleScrollerScroll)
      scroller.removeEventListener('wheel', handleInteraction)
      scroller.removeEventListener('touchmove', handleInteraction)
      scroller.removeEventListener('pointerdown', handlePointerDown)
      scroller.removeEventListener('pointerup', handlePointerEnd)
      scroller.removeEventListener('pointercancel', handlePointerEnd)
      scroller.removeEventListener('keydown', handleKeyDown)
      scroller.removeEventListener('keyup', handleKeyUp)
    }
  }, [handleInteraction, handleKeyDown, handleKeyUp, handlePointerDown, handlePointerEnd, handleScrollerScroll])

  useEffect(() => () => {
    if (pendingCaptureFrameRef.current !== null) cancelAnimationFrame(pendingCaptureFrameRef.current)
    clearInteractionCandidate()
  }, [clearInteractionCandidate])

  // Scroll events keep the snapshot current. Capture once more in layout
  // cleanup, before React detaches the Virtuoso ref during a session switch.
  useLayoutEffect(() => () => captureState(), [captureState])

  const handleScrollerRef = (element: HTMLElement | Window | null) => {
    const scroller = element instanceof HTMLElement ? element : null
    scrollerRef.current = scroller
    lastScrollTopRef.current = scroller?.scrollTop ?? null
    lastAtBottomRef.current = scroller
      ? scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight <= 8
      : null
  }

  const components = useMemo(() => ({ ...props.components, List }), [props.components])

  return (
    <Virtuoso<ConversationRow>
      ref={handleVirtuosoRef}
      data={props.rows}
      firstItemIndex={props.firstItemIndex}
      computeItemKey={(_, row) => row.key}
      itemContent={(_, row) => <div className="conversation-row" data-row-key={row.key}>{props.renderRow(row)}</div>}
      components={components}
      restoreStateFrom={savedState}
      initialTopMostItemIndex={!savedState && props.rows.length > 1
        ? { index: 'LAST', align: 'end' }
        : undefined}
      followOutput={(isAtBottom) => props.followOutput(props.sessionID, isAtBottom)}
      itemsRendered={handleItemsRendered}
      rangeChanged={handleRangeChanged}
      atBottomThreshold={8}
      atBottomStateChange={(atBottom) => {
        captureStateAfterScroll()
        props.onAtBottomStateChange(props.sessionID, atBottom)
      }}
      totalListHeightChanged={handleTotalListHeightChanged}
      defaultItemHeight={80}
      scrollerRef={handleScrollerRef}
    />
  )
}
