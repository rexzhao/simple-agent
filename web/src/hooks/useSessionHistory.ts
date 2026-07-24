import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { ItemsPage, Session } from '../types'

/** Owns selection-safe history requests and pagination. */
export function useSessionHistory(
  selectedSessionID: string,
  loadSessions: (projectID: string, preferredSessionID?: string) => Promise<Session[]>,
  onError: (reason: unknown) => void,
) {
  const [sessionDetail, setSessionDetail] = useState<Session | null>(null)
  const [itemsPage, setItemsPage] = useState<ItemsPage | null>(null)
  const selectedSessionRef = useRef(selectedSessionID)
  selectedSessionRef.current = selectedSessionID

  const refreshSession = useCallback(async (sessionID: string): Promise<Session | null> => {
    if (!sessionID) return null
    const [detail, page] = await Promise.all([api.session(sessionID), api.items(sessionID)])
    if (selectedSessionRef.current === sessionID) {
      setSessionDetail(detail)
      setItemsPage(page)
    }
    if (detail.project_id) await loadSessions(detail.project_id)
    return detail
  }, [loadSessions])

  useEffect(() => {
    if (!selectedSessionID) {
      setSessionDetail(null)
      setItemsPage(null)
      return
    }
    setItemsPage(null)
    void refreshSession(selectedSessionID).catch(onError)
  }, [onError, refreshSession, selectedSessionID])

  const loadOlder = useCallback(async (): Promise<boolean> => {
    if (!selectedSessionID || !itemsPage?.has_more_before || !itemsPage.oldest_seq) return false
    const sessionID = selectedSessionID
    const oldestSeq = itemsPage.oldest_seq
    try {
      const older = await api.items(sessionID, oldestSeq)
      if (selectedSessionRef.current !== sessionID) return false
      setItemsPage((current) => {
        if (!current || current.oldest_seq !== oldestSeq) return current
        return { ...current, items: [...older.items, ...current.items], oldest_seq: older.oldest_seq, has_more_before: older.has_more_before, has_more_after: false }
      })
      return true
    } catch (reason) {
      onError(reason)
      return false
    }
  }, [itemsPage, onError, selectedSessionID])

  return { sessionDetail, itemsPage, setSessionDetail, setItemsPage, selectedSessionRef, refreshSession, loadOlder }
}
