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
  const refreshGenerationRef = useRef<Record<string, number>>({})
  selectedSessionRef.current = selectedSessionID

  const refreshSession = useCallback(async (sessionID: string): Promise<Session | null> => {
    if (!sessionID) return null
    const generation = (refreshGenerationRef.current[sessionID] ?? 0) + 1
    refreshGenerationRef.current[sessionID] = generation
    let detail: Session
    let page: ItemsPage
    try {
      ;[detail, page] = await Promise.all([api.session(sessionID), api.items(sessionID)])
    } catch (reason) {
      // A request superseded by a selection change or a newer refresh must not
      // surface an error in the current session.
      if (selectedSessionRef.current !== sessionID || refreshGenerationRef.current[sessionID] !== generation) return null
      throw reason
    }
    const latest = refreshGenerationRef.current[sessionID] === generation
    if (selectedSessionRef.current === sessionID && latest) {
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
      if (selectedSessionRef.current === sessionID) onError(reason)
      return false
    }
  }, [itemsPage, onError, selectedSessionID])

  return { sessionDetail, itemsPage, setSessionDetail, setItemsPage, selectedSessionRef, refreshSession, loadOlder }
}
