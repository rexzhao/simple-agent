import { useCallback, useMemo, useReducer, useRef } from 'react'
import type { ItemsPage, Session, SessionSnapshot } from '../types'
import { api } from '../api'
import { initialSessionStoreState, sessionStoreReducer } from '../lib/sessionStore'

/**
 * Wraps the session store reducer and provides the async operations that
 * fetch data and dispatch actions. All session/history/list state flows
 * through this single store so the UI never has competing authorities.
 */
export function useSessionStore() {
  const [state, dispatch] = useReducer(sessionStoreReducer, undefined, initialSessionStoreState)
  const stateRef = useRef(state)
  stateRef.current = state

  const fetchSnapshot = useCallback(
    async (sessionID: string): Promise<{ session: Session; history: ItemsPage }> => {
      const snapshot: SessionSnapshot = await api.snapshot(sessionID)
      dispatch({ type: 'snapshot', snapshot, expectedSessionID: sessionID })
      return { session: snapshot.session, history: snapshot.history }
    },
    [],
  )

  const fetchSnapshotFallback = useCallback(
    async (sessionID: string): Promise<{ session: Session; history: ItemsPage }> => {
      // Fallback for servers that don't yet have the snapshot endpoint.
      const [session, history] = await Promise.all([api.session(sessionID), api.items(sessionID)])
      const snapshot: SessionSnapshot = {
        session_id: sessionID,
        revision: String(session.last_seq),
        session,
        history,
      }
      dispatch({ type: 'snapshot', snapshot, expectedSessionID: sessionID })
      return { session, history }
    },
    [],
  )

  /** Fetches a snapshot, falling back to dual-fetch on 404. Always returns fetched data. */
  const refreshSession = useCallback(
    async (sessionID: string): Promise<{ session: Session; history: ItemsPage }> => {
      try {
        return await fetchSnapshot(sessionID)
      } catch (reason) {
        // If the snapshot endpoint doesn't exist, fall back to dual-fetch.
        if (reason && typeof reason === 'object' && 'status' in reason && (reason as { status: number }).status === 404) {
          return await fetchSnapshotFallback(sessionID)
        }
        throw reason
      }
    },
    [fetchSnapshot, fetchSnapshotFallback],
  )

  const loadOlder = useCallback(
    async (sessionID: string): Promise<boolean> => {
      const existing = stateRef.current.historyBySession[sessionID]
      if (!existing?.page.has_more_before || !existing.page.oldest_seq) return false
      const older = await api.items(sessionID, existing.page.oldest_seq)
      dispatch({ type: 'pageOlder', sessionID, older })
      return true
    },
    [],
  )

  const setSessions = useCallback(
    (projectID: string, sessions: Session[], archived: boolean, generation: number) => {
      dispatch({ type: 'sessions', projectID, sessions, archived, generation })
    },
    [],
  )

  const clearSession = useCallback((sessionID: string) => {
    dispatch({ type: 'clearSession', sessionID })
  }, [])

  const setMeta = useCallback((sessionID: string, loading?: boolean, error?: string) => {
    dispatch({ type: 'setMeta', sessionID, loading, error })
  }, [])

  return useMemo(
    () => ({ state, refreshSession, loadOlder, setSessions, clearSession, setMeta }),
    // state must be in deps so consumers re-render when the reducer produces new state.
    [state, refreshSession, loadOlder, setSessions, clearSession, setMeta],
  )
}

export type SessionStoreHook = ReturnType<typeof useSessionStore>
