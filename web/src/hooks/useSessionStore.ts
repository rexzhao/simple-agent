import { useCallback, useMemo, useReducer, useRef } from 'react'
import type { ItemsPage, Session, SessionItemProjectionEvent, SessionSnapshot } from '../types'
import { api } from '../api'
import { initialSessionStoreState, parseDecimalRevision, revisionGTE, sessionStoreReducer, type SessionStoreAction } from '../lib/sessionStore'

type SnapshotFetchResult = { session: Session; history: ItemsPage; requiresResync: boolean }

/**
 * Wraps the session store reducer and provides the async operations that
 * fetch data and dispatch actions. All session/history/list state flows
 * through this single store so the UI never has competing authorities.
 */
export function useSessionStore() {
  const [state, dispatch] = useReducer(sessionStoreReducer, undefined, initialSessionStoreState)
  const stateRef = useRef(state)
  const renderedStateRef = useRef(state)
  // Keep the async-operation view current even when a response races a
  // projection dispatch before React has committed the next render. The
  // rendered-state guard prevents a rerender with the old reducer value from
  // erasing actions already queued in this ref.
  if (renderedStateRef.current !== state) {
    renderedStateRef.current = state
    stateRef.current = state
  }
  const dispatchStore = useCallback((action: SessionStoreAction) => {
    stateRef.current = sessionStoreReducer(stateRef.current, action)
    dispatch(action)
  }, [dispatch])

  const fetchSnapshot = useCallback(
    async (sessionID: string): Promise<SnapshotFetchResult> => {
      const snapshot: SessionSnapshot = await api.snapshot(sessionID)
      const snapshotRevision = parseDecimalRevision(snapshot.revision)
      if (snapshotRevision === null) throw new Error('Session snapshot returned an invalid revision')
      const normalizedSnapshot = snapshot.revision === snapshotRevision
        ? snapshot
        : { ...snapshot, revision: snapshotRevision }
      const pending = stateRef.current.pendingProjectionBySession[sessionID]
      const overflowRevision = pending?.overflowed ? pending.overflowRevision : '0'
      const markedRevision = stateRef.current.snapshotResyncBySession[sessionID] ?? '0'
      const requiredRevision = revisionGTE(overflowRevision, markedRevision) ? overflowRevision : markedRevision
      const requiresResync = !stateRef.current.historyBySession[sessionID]
        && !revisionGTE(snapshotRevision, requiredRevision)
      dispatchStore({ type: 'snapshot', snapshot: normalizedSnapshot, expectedSessionID: sessionID })
      return { session: normalizedSnapshot.session, history: normalizedSnapshot.history, requiresResync }
    },
    [dispatchStore],
  )

  const fetchSnapshotFallback = useCallback(
    async (sessionID: string): Promise<SnapshotFetchResult> => {
      // Fallback for servers that don't yet have the snapshot endpoint.
      const [session, history] = await Promise.all([api.session(sessionID), api.items(sessionID)])
      const revision = parseDecimalRevision(session.revision) ?? parseDecimalRevision(session.last_seq)
      if (revision === null) throw new Error('Session fallback returned an invalid revision')
      const snapshot: SessionSnapshot = {
        session_id: sessionID,
        revision,
        session,
        history,
      }
      const pending = stateRef.current.pendingProjectionBySession[sessionID]
      const overflowRevision = pending?.overflowed ? pending.overflowRevision : '0'
      const markedRevision = stateRef.current.snapshotResyncBySession[sessionID] ?? '0'
      const requiredRevision = revisionGTE(overflowRevision, markedRevision) ? overflowRevision : markedRevision
      const requiresResync = !stateRef.current.historyBySession[sessionID]
        && !revisionGTE(snapshot.revision, requiredRevision)
      dispatchStore({ type: 'snapshot', snapshot, expectedSessionID: sessionID })
      return { session, history, requiresResync }
    },
    [dispatchStore],
  )

  /** Fetches a snapshot, falling back to dual-fetch on 404. Always returns fetched data. */
  const refreshSession = useCallback(
    async (sessionID: string): Promise<{ session: Session; history: ItemsPage }> => {
      dispatchStore({ type: 'snapshotStarted', sessionID })
      try {
        const fetchAuthoritativeSnapshot = async (): Promise<SnapshotFetchResult> => {
          try {
            return await fetchSnapshot(sessionID)
          } catch (reason) {
            // If the snapshot endpoint doesn't exist, fall back to dual-fetch.
            if (reason && typeof reason === 'object' && 'status' in reason && (reason as { status: number }).status === 404) {
              return await fetchSnapshotFallback(sessionID)
            }
            throw reason
          }
        }
        let result = await fetchAuthoritativeSnapshot()
        // A bounded pending queue may have dropped records newer than the
        // first response. That response is intentionally not exposed as a
        // complete history; fetch again until the authoritative revision
        // covers the dropped range.
        while (result.requiresResync) result = await fetchAuthoritativeSnapshot()
        return { session: result.session, history: result.history }
      } catch (reason) {
        throw reason
      } finally {
        dispatchStore({ type: 'snapshotFinished', sessionID })
      }
    },
    [dispatchStore, fetchSnapshot, fetchSnapshotFallback],
  )

  const loadOlder = useCallback(
    async (sessionID: string): Promise<boolean> => {
      const existing = stateRef.current.historyBySession[sessionID]
      if (!existing?.page.has_more_before || !existing.page.oldest_seq) return false
      // Capture the watermark before awaiting the page. Events arriving while
      // this request is in flight are compared against this exact coverage
      // point when the response is merged.
      const requestRevision = existing.revision
      const older = await api.items(sessionID, existing.page.oldest_seq)
      dispatchStore({ type: 'pageOlder', sessionID, older, requestRevision })
      return true
    },
    [dispatchStore],
  )

  const setSessions = useCallback(
    (projectID: string, sessions: Session[], archived: boolean, generation: number) => {
      dispatchStore({ type: 'sessions', projectID, sessions, archived, generation })
    },
    [dispatchStore],
  )

  const clearSession = useCallback((sessionID: string) => {
    dispatchStore({ type: 'clearSession', sessionID })
  }, [dispatchStore])

  const setMeta = useCallback((sessionID: string, loading?: boolean, error?: string) => {
    dispatchStore({ type: 'setMeta', sessionID, loading, error })
  }, [dispatchStore])

  const applyProjectionEvent = useCallback((event: SessionItemProjectionEvent) => {
    dispatchStore({ type: 'projectionEvent', event })
  }, [dispatchStore])

  const updateSessionMetadata = useCallback((session: Session) => {
    dispatchStore({ type: 'sessionMetadata', session })
  }, [dispatchStore])

  const applySettlementMetadata = useCallback((session: Session, revision: unknown): boolean => {
    const normalizedRevision = parseDecimalRevision(revision)
    if (normalizedRevision === null) return false
    dispatchStore({ type: 'settlementMetadata', session, revision: normalizedRevision })
    return true
  }, [dispatchStore])

  /**
   * Read the reducer's synchronous watermark, rather than a React render
   * closure.  This is used by SSE settlement handlers which can run in the
   * same turn as the final projection event.
   */
  const getSessionRevision = useCallback((sessionID: string): string | undefined => {
    const current = stateRef.current
    const candidates: string[] = []
    const add = (value: unknown) => {
      const revision = parseDecimalRevision(value)
      if (revision !== null) candidates.push(revision)
    }
    // Sidebar/list metadata is not projection coverage. A session DTO can
    // describe a newer server revision while the browser still lacks the
    // corresponding item events. Only an established snapshot history entry
    // (plus events applied to that entry) is a complete local projection.
    add(current.historyBySession[sessionID]?.revision)
    if (candidates.length === 0) return undefined
    return candidates.reduce((highest, candidate) => revisionGTE(candidate, highest) ? candidate : highest)
  }, [])

  const isRevisionCovered = useCallback((sessionID: string, target: unknown): boolean => {
    const required = parseDecimalRevision(target)
    if (required === null) return false
    const local = getSessionRevision(sessionID)
    return local !== undefined && revisionGTE(local, required)
  }, [getSessionRevision])

  return useMemo(
    () => ({ state, refreshSession, loadOlder, setSessions, clearSession, setMeta, applyProjectionEvent, updateSessionMetadata, applySettlementMetadata, getSessionRevision, isRevisionCovered }),
    // state must be in deps so consumers re-render when the reducer produces new state.
    [state, refreshSession, loadOlder, setSessions, clearSession, setMeta, applyProjectionEvent, updateSessionMetadata, applySettlementMetadata, getSessionRevision, isRevisionCovered],
  )
}

export type SessionStoreHook = ReturnType<typeof useSessionStore>
