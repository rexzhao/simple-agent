import type { ItemsPage, Session, SessionSnapshot } from '../types'

/**
 * The normalized session store holds all session data in a single
 * reducer-driven state tree. Every asynchronous result (snapshot, list,
 * page) merges through here so the UI never has multiple competing
 * authorities.
 */
export interface SessionStoreState {
  sessionsByID: Record<string, Session>
  /** project → ordered session ID lists (active + archived) */
  sessionIDsByProject: Record<string, { active: string[]; archived: string[] }>
  /** session → history window + revision */
  historyBySession: Record<string, { page: ItemsPage; revision: string }>
  /** session → loading/error/refresh metadata */
  metaBySession: Record<string, { loading: boolean; error: string; refreshGeneration: number }>
  /** project → last applied list generation (for out-of-order discard) */
  listGenerationByProject: Record<string, number>
}

/** LRU cap for historyBySession, matching the old conversationCacheRef. */
const historyLRUCap = 10

export function initialSessionStoreState(): SessionStoreState {
  return {
    sessionsByID: {},
    sessionIDsByProject: {},
    historyBySession: {},
    metaBySession: {},
    listGenerationByProject: {},
  }
}

export type SessionStoreAction =
  | { type: 'snapshot'; snapshot: SessionSnapshot; expectedSessionID: string }
  | { type: 'sessions'; projectID: string; sessions: Session[]; archived: boolean; generation: number }
  | { type: 'pageOlder'; sessionID: string; older: ItemsPage }
  | { type: 'setMeta'; sessionID: string; loading?: boolean; error?: string }
  | { type: 'clearSession'; sessionID: string }

/**
 * Compares two revision strings as integers using BigInt so that "9" < "10"
 * (dictionary order would give the wrong result).
 */
export function revisionGTE(a: string, b: string): boolean {
  return BigInt(a) >= BigInt(b)
}

/**
 * Merges a freshly loaded tail page into the page currently on screen.
 * Reused from the old useSessionHistory.mergeRefreshedPage logic.
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

/** Touches the LRU order for the given session, evicting the oldest if needed. */
function touchLRU(historyBySession: Record<string, { page: ItemsPage; revision: string }>, sessionID: string): void {
  // Delete and re-insert so the key moves to the end of insertion order.
  const entry = historyBySession[sessionID]
  if (!entry) return
  delete historyBySession[sessionID]
  historyBySession[sessionID] = entry
  while (Object.keys(historyBySession).length > historyLRUCap) {
    const oldest = Object.keys(historyBySession)[0]
    if (oldest === undefined) break
    delete historyBySession[oldest]
  }
}

export function sessionStoreReducer(state: SessionStoreState, action: SessionStoreAction): SessionStoreState {
  switch (action.type) {
    case 'snapshot': {
      const { snapshot, expectedSessionID } = action
      // Invariant 1: identity must match.
      if (snapshot.session_id !== expectedSessionID) return state

      const existing = state.historyBySession[snapshot.session_id]
      // Invariant 3: older revision must not overwrite newer.
      if (existing && revisionGTE(existing.revision, snapshot.revision)) return state

      const merged = mergeRefreshedPage(existing?.page ?? null, snapshot.history)
      const historyBySession = { ...state.historyBySession }
      historyBySession[snapshot.session_id] = { page: merged, revision: snapshot.revision }
      touchLRU(historyBySession, snapshot.session_id)

      const sessionsByID = { ...state.sessionsByID, [snapshot.session_id]: snapshot.session }

      return { ...state, historyBySession, sessionsByID }
    }

    case 'sessions': {
      const { projectID, sessions, archived, generation } = action
      const currentGen = state.listGenerationByProject[projectID] ?? 0
      if (generation < currentGen) return state // stale list response

      const ids = sessions.map((s) => s.id)
      const current = state.sessionIDsByProject[projectID] ?? { active: [], archived: [] }
      const sessionIDsByProject = {
        ...state.sessionIDsByProject,
        [projectID]: archived ? { ...current, archived: ids } : { ...current, active: ids },
      }
      const sessionsByID = { ...state.sessionsByID }
      for (const s of sessions) sessionsByID[s.id] = s
      const listGenerationByProject = { ...state.listGenerationByProject, [projectID]: generation }

      return { ...state, sessionIDsByProject, sessionsByID, listGenerationByProject }
    }

    case 'pageOlder': {
      const { sessionID, older } = action
      const existing = state.historyBySession[sessionID]
      if (!existing) return state
      const next: ItemsPage = {
        ...existing.page,
        items: [...older.items, ...existing.page.items],
        oldest_seq: older.oldest_seq,
        has_more_before: older.has_more_before,
        has_more_after: false,
      }
      const historyBySession = { ...state.historyBySession, [sessionID]: { ...existing, page: next } }
      touchLRU(historyBySession, sessionID)
      return { ...state, historyBySession }
    }

    case 'setMeta': {
      const { sessionID, loading, error } = action
      const current = state.metaBySession[sessionID] ?? { loading: false, error: '', refreshGeneration: 0 }
      const metaBySession = {
        ...state.metaBySession,
        [sessionID]: {
          ...current,
          ...(loading !== undefined ? { loading } : {}),
          ...(error !== undefined ? { error } : {}),
        },
      }
      return { ...state, metaBySession }
    }

    case 'clearSession': {
      const { sessionID } = action
      const historyBySession = { ...state.historyBySession }
      delete historyBySession[sessionID]
      const sessionsByID = { ...state.sessionsByID }
      delete sessionsByID[sessionID]
      const metaBySession = { ...state.metaBySession }
      delete metaBySession[sessionID]
      return { ...state, historyBySession, sessionsByID, metaBySession }
    }

    default:
      return state
  }
}
