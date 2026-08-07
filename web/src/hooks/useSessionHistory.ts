import { useCallback, useEffect, useRef } from 'react'
import type { ItemsPage, Session } from '../types'
import type { SessionStoreHook } from './useSessionStore'

/**
 * Owns selection-safe history requests and pagination.
 *
 * Now backed by the normalized session store (useSessionStore). The hook no
 * longer holds independent authoritative state; it reads detail/page from the
 * store and triggers fetches on selection change. The store's LRU-capped
 * historyBySession replaces the old conversationCacheRef.
 */
export function useSessionHistory(selectedSessionID: string, onError: (reason: unknown) => void, store: SessionStoreHook): ReturnType<typeof useSessionHistoryImpl>
/** @deprecated The list reload callback is ignored; Session Index owns navigation. */
export function useSessionHistory(selectedSessionID: string, _legacyLoadSessions: (projectID: string, preferredSessionID?: string) => Promise<Session[]>, onError: (reason: unknown) => void, store: SessionStoreHook): ReturnType<typeof useSessionHistoryImpl>
export function useSessionHistory(
  selectedSessionID: string,
  onErrorOrLegacy: ((reason: unknown) => void) | ((projectID: string, preferredSessionID?: string) => Promise<Session[]>),
  storeOrError: SessionStoreHook | ((reason: unknown) => void),
  legacyStore?: SessionStoreHook,
) {
  const onError = (legacyStore ? storeOrError : onErrorOrLegacy) as (reason: unknown) => void
  const store = (legacyStore ?? storeOrError) as SessionStoreHook
  return useSessionHistoryImpl(selectedSessionID, onError, store)
}

function useSessionHistoryImpl(
  selectedSessionID: string,
  onError: (reason: unknown) => void,
  store: SessionStoreHook,
) {
  const { state, refreshSession: storeRefresh, loadOlder: storeLoadOlder } = store
  const selectedSessionRef = useRef(selectedSessionID)
  selectedSessionRef.current = selectedSessionID

  const sessionDetail = selectedSessionID ? state.sessionsByID[selectedSessionID] ?? null : null
  const itemsPage = selectedSessionID ? state.historyBySession[selectedSessionID]?.page ?? null : null

  const refreshSession = useCallback(async (sessionID: string): Promise<Session | null> => {
    if (!sessionID) return null
    try {
      const { session } = await storeRefresh(sessionID)
      // The fetched session is always returned: run settlement reconciles
      // background sessions through this value, and suppressing it strands
      // their runs in reconciling until the manual-refresh banner appears.
      // Only the sidebar reload and surfaced errors stay selection-scoped.
      return session
    } catch (reason) {
      if (selectedSessionRef.current !== sessionID) return null
      throw reason
    }
  }, [storeRefresh])

  useEffect(() => {
    if (!selectedSessionID) return
    void refreshSession(selectedSessionID).catch(onError)
  }, [onError, refreshSession, selectedSessionID])

  const loadOlder = useCallback(async (): Promise<boolean> => {
    if (!selectedSessionID) return false
    try {
      return await storeLoadOlder(selectedSessionID)
    } catch (reason) {
      if (selectedSessionRef.current === selectedSessionID) onError(reason)
      return false
    }
  }, [onError, selectedSessionID, storeLoadOlder])

  return { sessionDetail, itemsPage, selectedSessionRef, refreshSession, loadOlder }
}
