import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { ItemsPage, Session } from '../types'

/**
 * Merges a freshly loaded tail page into the page currently on screen.
 *
 * Older pages the user already paged through stay loaded when the refreshed
 * tail overlaps (or touches) the loaded window, so background refreshes
 * triggered by run.settled / resync / rename / compact do not discard the
 * history the user is reading or yank the viewport. Falls back to wholesale
 * replacement when the ranges diverged (e.g. compaction rewrote the tail).
 *
 * Items strictly older than the refreshed page are kept as-is, so a refresh
 * never removes very old entries from view; that is a deliberate trade to
 * keep the scroll position stable.
 */
export function mergeRefreshedPage(current: ItemsPage | null, refreshed: ItemsPage): ItemsPage {
  if (!current) return refreshed
  const overlapsTail = refreshed.oldest_seq <= current.newest_seq + 1
  const extendsTail = refreshed.newest_seq >= current.newest_seq
  if (!overlapsTail || !extendsTail) return refreshed
  const prefix = current.items.filter((item) => item.seq < refreshed.oldest_seq)
  if (prefix.length === 0) return refreshed
  return {
    items: [...prefix, ...refreshed.items],
    oldest_seq: current.oldest_seq,
    newest_seq: refreshed.newest_seq,
    has_more_before: current.has_more_before,
    has_more_after: refreshed.has_more_after,
  }
}

/** Owns selection-safe history requests and pagination. */
export function useSessionHistory(
  selectedSessionID: string,
  loadSessions: (projectID: string, preferredSessionID?: string) => Promise<Session[]>,
  onError: (reason: unknown) => void,
) {
  const [sessionDetail, setSessionDetail] = useState<Session | null>(null)
  const [itemsPage, setItemsPage] = useState<ItemsPage | null>(null)
  const selectedSessionRef = useRef(selectedSessionID)
  const previousSessionRef = useRef('')
  const refreshGenerationRef = useRef<Record<string, number>>({})
  const sessionDetailRef = useRef<Session | null>(null)
  const itemsPageRef = useRef<ItemsPage | null>(null)
  // Recently viewed conversations, LRU-capped. Restored synchronously on
  // session switch so the viewport can re-anchor on real content before the
  // background refresh lands; the refresh then merges into the restored
  // window via mergeRefreshedPage.
  const conversationCacheRef = useRef(new Map<string, { detail: Session; page: ItemsPage }>())
  selectedSessionRef.current = selectedSessionID
  sessionDetailRef.current = sessionDetail
  itemsPageRef.current = itemsPage

  const cacheConversation = useCallback((sessionID: string, detail: Session | null, page: ItemsPage | null) => {
    if (!sessionID || !detail || !page) return
    const cache = conversationCacheRef.current
    cache.delete(sessionID)
    cache.set(sessionID, { detail, page })
    while (cache.size > 10) {
      const oldest = cache.keys().next().value
      if (oldest === undefined) break
      cache.delete(oldest)
    }
  }, [])

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
      const merged = mergeRefreshedPage(itemsPageRef.current, page)
      sessionDetailRef.current = detail
      itemsPageRef.current = merged
      setSessionDetail(detail)
      setItemsPage(merged)
      cacheConversation(sessionID, detail, merged)
    }
    if (detail.project_id) {
      try {
        await loadSessions(detail.project_id)
      } catch (reason) {
        if (selectedSessionRef.current === sessionID && refreshGenerationRef.current[sessionID] === generation) throw reason
      }
    }
    return detail
  }, [cacheConversation, loadSessions])

  useEffect(() => {
    if (!selectedSessionID) {
      previousSessionRef.current = ''
      setSessionDetail(null)
      setItemsPage(null)
      return
    }
    // Stash the outgoing conversation so switching back to it is instant.
    const outgoingID = previousSessionRef.current
    if (outgoingID && outgoingID !== selectedSessionID) {
      cacheConversation(outgoingID, sessionDetailRef.current, itemsPageRef.current)
    }
    previousSessionRef.current = selectedSessionID
    const cached = conversationCacheRef.current.get(selectedSessionID) ?? null
    sessionDetailRef.current = cached?.detail ?? null
    itemsPageRef.current = cached?.page ?? null
    setSessionDetail(cached?.detail ?? null)
    setItemsPage(cached?.page ?? null)
    void refreshSession(selectedSessionID).catch(onError)
  }, [cacheConversation, onError, refreshSession, selectedSessionID])

  const loadOlder = useCallback(async (): Promise<boolean> => {
    if (!selectedSessionID || !itemsPage?.has_more_before || !itemsPage.oldest_seq) return false
    const sessionID = selectedSessionID
    const oldestSeq = itemsPage.oldest_seq
    try {
      const older = await api.items(sessionID, oldestSeq)
      if (selectedSessionRef.current !== sessionID) return false
      const current = itemsPageRef.current
      if (!current || current.oldest_seq !== oldestSeq) return false
      const next = { ...current, items: [...older.items, ...current.items], oldest_seq: older.oldest_seq, has_more_before: older.has_more_before, has_more_after: false }
      itemsPageRef.current = next
      setItemsPage(next)
      cacheConversation(sessionID, sessionDetailRef.current, next)
      return true
    } catch (reason) {
      if (selectedSessionRef.current === sessionID) onError(reason)
      return false
    }
  }, [cacheConversation, itemsPage, onError, selectedSessionID])

  return { sessionDetail, itemsPage, setSessionDetail, setItemsPage, selectedSessionRef, refreshSession, loadOlder }
}
