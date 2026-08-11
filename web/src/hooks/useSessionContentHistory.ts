import { useCallback, useEffect, useRef, useState } from 'react'
import type { SessionContentRepository, SessionContentHistoryState, SessionView } from '../repositories/sessionContent'
import { useSessionView } from './useSyncApplication'

export interface SessionContentHistoryHook {
  readonly view: SessionView
  readonly historyState: SessionContentHistoryState
  readonly loadOlder: () => Promise<boolean>
  readonly retry: () => void
  /** True until the targeted retry reaches a snapshot or error barrier. */
  readonly retrying: boolean
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
  const retryingRef = useRef(false)
  const [retrying, setRetrying] = useState(false)
  selectedSessionRef.current = sessionID
  // A page interest owns the HTTP/blob read. Changing selection or unmounting
  // releases that interest immediately instead of relying only on a later
  // generation check to discard a potentially large response.
  useEffect(() => () => {
    readAbortRef.current?.abort()
    readAbortRef.current = null
    retryingRef.current = false
    setRetrying(false)
  }, [repository, sessionID])
  // Retry state is scoped to the selected resource. Do not let a late result
  // from the previous session disable the new session's Refresh action.
  useEffect(() => {
    retryingRef.current = false
    setRetrying(false)
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
  useEffect(() => {
    if (!retryingRef.current || selectedSessionRef.current !== sessionID) return
    // A ready resource means the authoritative snapshot/replay barrier
    // committed. If the snapshot still says recovery_required, the banner is
    // deliberately retained; that bit is server authority, not UI progress.
    if (view.availability.status === 'ready' || view.error) {
      retryingRef.current = false
      setRetrying(false)
    }
  }, [sessionID, view.availability.status, view.error?.code, view.error?.message])
  const retry = useCallback(() => {
    if (!sessionID || selectedSessionRef.current !== sessionID || retryingRef.current) return
    retryingRef.current = true
    setRetrying(true)
    repository.retry(sessionID)
  }, [repository, sessionID])
  return { view, historyState, loadOlder, retry, retrying }
}
