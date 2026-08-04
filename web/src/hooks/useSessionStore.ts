import { useCallback, useMemo, useReducer, useRef } from 'react'
import type { ItemsPage, Session, SessionItemProjectionEvent, SessionSnapshot } from '../types'
import { api } from '../api'
import { initialSessionStoreState, revisionGTE, sessionStoreReducer, type SessionStoreAction } from '../lib/sessionStore'

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
      const pending = stateRef.current.pendingProjectionBySession[sessionID]
      const overflowRevision = pending?.overflowed ? pending.overflowRevision : '0'
      const markedRevision = stateRef.current.snapshotResyncBySession[sessionID] ?? '0'
      const requiredRevision = revisionGTE(overflowRevision, markedRevision) ? overflowRevision : markedRevision
      const requiresResync = !stateRef.current.historyBySession[sessionID]
        && BigInt(snapshot.revision) < BigInt(requiredRevision)
      dispatchStore({ type: 'snapshot', snapshot, expectedSessionID: sessionID })
      return { session: snapshot.session, history: snapshot.history, requiresResync }
    },
    [dispatchStore],
  )

  const fetchSnapshotFallback = useCallback(
    async (sessionID: string): Promise<SnapshotFetchResult> => {
      // Fallback for servers that don't yet have the snapshot endpoint.
      const [session, history] = await Promise.all([api.session(sessionID), api.items(sessionID)])
      const snapshot: SessionSnapshot = {
        session_id: sessionID,
        revision: String(session.last_seq),
        session,
        history,
      }
      const pending = stateRef.current.pendingProjectionBySession[sessionID]
      const overflowRevision = pending?.overflowed ? pending.overflowRevision : '0'
      const markedRevision = stateRef.current.snapshotResyncBySession[sessionID] ?? '0'
      const requiredRevision = revisionGTE(overflowRevision, markedRevision) ? overflowRevision : markedRevision
      const requiresResync = !stateRef.current.historyBySession[sessionID]
        && BigInt(snapshot.revision) < BigInt(requiredRevision)
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

  return useMemo(
    () => ({ state, refreshSession, loadOlder, setSessions, clearSession, setMeta, applyProjectionEvent }),
    // state must be in deps so consumers re-render when the reducer produces new state.
    [state, refreshSession, loadOlder, setSessions, clearSession, setMeta, applyProjectionEvent],
  )
}

export type SessionStoreHook = ReturnType<typeof useSessionStore>
