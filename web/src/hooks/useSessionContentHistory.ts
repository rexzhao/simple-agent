import { useCallback, useEffect, useRef } from 'react'
import type { SessionContentRepository, SessionContentHistoryState, SessionView } from '../repositories/sessionContent'
import { useSessionView } from './useSyncApplication'

export interface SessionContentHistoryHook {
  readonly view: SessionView
  readonly historyState: SessionContentHistoryState
  readonly loadOlder: () => Promise<boolean>
  readonly retry: () => void
}

/**
 * Selection-safe application hook for the opened session. The repository owns
 * page merge/deduplication and the Blob data plane; this hook only exposes the
 * result and operation status to the page.
 */
export function useSessionContentHistory(
  sessionID: string,
  repository: SessionContentRepository,
): SessionContentHistoryHook {
  const view = useSessionView(sessionID)
  const selectedSessionRef = useRef(sessionID)
  const readAbortRef = useRef<AbortController | null>(null)
  selectedSessionRef.current = sessionID
  // A page interest owns the HTTP/blob read. Changing selection or unmounting
  // releases that interest immediately instead of relying only on a later
  // generation check to discard a potentially large response.
  useEffect(() => () => {
    readAbortRef.current?.abort()
    readAbortRef.current = null
  }, [repository, sessionID])
  const historyState = view.historyState
  const loadOlder = useCallback(async (): Promise<boolean> => {
    if (!sessionID || selectedSessionRef.current !== sessionID) return false
    readAbortRef.current?.abort()
    const controller = new AbortController()
    readAbortRef.current = controller
    try {
      return await repository.loadOlder(sessionID, controller.signal)
    } catch {
      // The repository retains a safe, non-protocol error state. The page can
      // show retry without exposing command/blob failure details.
      return false
    } finally {
      if (readAbortRef.current === controller) readAbortRef.current = null
    }
  }, [repository, sessionID])
  const retry = useCallback(() => repository.retry(sessionID), [repository, sessionID])
  return { view, historyState, loadOlder, retry }
}
