import type { ItemsPage, Session, SessionItem, SessionItemProjectionEvent, SessionSnapshot } from '../types'
import { sessionItemIdentityKey } from './session'

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
  historyBySession: Record<string, SessionHistoryEntry>
  /**
   * Projection events received before the session gets its first snapshot.
   * This is deliberately not historyBySession. It has its own bounded LRU;
   * an in-flight session that is evicted retains only a resync watermark.
   */
  pendingProjectionBySession: Record<string, PendingProjectionEntry>
  /** Number of in-flight snapshot requests for each session. */
  snapshotInFlightBySession: Record<string, number>
  /**
   * A bounded queue can be evicted while a snapshot is in flight. Keep only
   * the revision watermark needed to force a later authoritative resync; no
   * item payloads are retained in this map.
   */
  snapshotResyncBySession: Record<string, string>
  /** session → loading/error/refresh metadata */
  metaBySession: Record<string, { loading: boolean; error: string; refreshGeneration: number }>
  /** project → last applied list generation (for out-of-order discard) */
  listGenerationByProject: Record<string, number>
}

export interface PendingProjectionEntry {
  /** Sorted by durable record seq; retained until a snapshot establishes a base. */
  events: SessionItemProjectionEvent[]
  revision: string
  /** True when one or more records were dropped by the per-session cap. */
  overflowed: boolean
  /** Highest revision among records dropped by the cap. */
  overflowRevision: string
}

export interface SessionHistoryEntry {
  page: ItemsPage
  /** Highest session-wide revision observed by this entry. */
  revision: string
  /** Highest revision covered by an authoritative snapshot. */
  snapshotCoverageRevision: string
  /** Snapshot coverage is page-scoped; older cached pages may be outside it. */
  snapshotCoverageByItemID: Record<string, string>
  /** Highest durable record sequence observed in projection events/snapshots. */
  appliedProjectionRecordSeq: string
  /** Durable record sequence of the latest accepted event for each item. */
  projectionRecordSeqByItemID: Record<string, string>
  /** Revision at which each item was last supplied by a projection event. */
  projectionEventRevisionByItemID: Record<string, string>
  /** Updates for items outside the currently cached page. */
  pendingProjectionByItemID: Record<string, { item: SessionItem; recordSeq: string; revision: string }>
}

/** LRU cap for historyBySession, matching the old conversationCacheRef. */
const historyLRUCap = 10
/** Bound background projection buffering; a later snapshot repairs evictions. */
export const pendingProjectionSessionCap = 32
export const pendingProjectionEventCap = 256

export function initialSessionStoreState(): SessionStoreState {
  return {
    sessionsByID: {},
    sessionIDsByProject: {},
    historyBySession: {},
    pendingProjectionBySession: {},
    snapshotInFlightBySession: {},
    snapshotResyncBySession: {},
    metaBySession: {},
    listGenerationByProject: {},
  }
}

export type SessionStoreAction =
  | { type: 'snapshot'; snapshot: SessionSnapshot; expectedSessionID: string }
  | { type: 'snapshotStarted'; sessionID: string }
  | { type: 'snapshotFinished'; sessionID: string }
  | { type: 'sessions'; projectID: string; sessions: Session[]; archived: boolean; generation: number }
  | { type: 'pageOlder'; sessionID: string; older: ItemsPage; requestRevision: string }
  | { type: 'projectionEvent'; event: SessionItemProjectionEvent }
  | { type: 'sessionMetadata'; session: Session }
  | { type: 'settlementMetadata'; session: Session; revision: string }
  | { type: 'setMeta'; sessionID: string; loading?: boolean; error?: string }
  | { type: 'clearSession'; sessionID: string }

/**
 * Normalize a session revision without ever routing it through Number.
 *
 * Revisions are wire-level decimal strings.  The runtime checks here are
 * intentionally defensive: an old server, a malformed fixture, or a
 * hostile SSE payload must make reconciliation conservative, not take down
 * the reducer with a BigInt conversion error.
 */
export function parseDecimalRevision(value: unknown): string | null {
  if (typeof value === 'bigint') return value >= 0n ? value.toString() : null
  if (typeof value === 'number') {
    return Number.isSafeInteger(value) && value >= 0 ? String(value) : null
  }
  if (typeof value !== 'string') return null
  const trimmed = value.trim()
  if (!/^\d+$/.test(trimmed)) return null
  return trimmed.replace(/^0+(?=\d)/, '')
}

/** Returns -1, 0, or 1; null means at least one input is not a revision. */
export function compareRevisions(a: unknown, b: unknown): -1 | 0 | 1 | null {
  const left = parseDecimalRevision(a)
  const right = parseDecimalRevision(b)
  if (left === null || right === null) return null
  const leftBig = BigInt(left)
  const rightBig = BigInt(right)
  if (leftBig < rightBig) return -1
  if (leftBig > rightBig) return 1
  return 0
}

/**
 * Compares two revisions as integers so that "9" < "10". Invalid input is
 * deliberately not considered covered.
 */
export function revisionGTE(a: unknown, b: unknown): boolean {
  const comparison = compareRevisions(a, b)
  return comparison !== null && comparison >= 0
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
function touchLRU(historyBySession: Record<string, SessionHistoryEntry>, sessionID: string): void {
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

function maxDecimal(a: string, b: string): string {
  const left = parseDecimalRevision(a) ?? '0'
  const right = parseDecimalRevision(b) ?? '0'
  return BigInt(left) >= BigInt(right) ? left : right
}

function projectionEventRecordSeq(event: SessionItemProjectionEvent): string {
  return parseDecimalRevision(event.seq) ?? '0'
}

/** Validate and normalize the complete durable item identity. A malformed
 * envelope is ignored rather than allowing an item-id-only update to mutate a
 * different turn/iteration. */
function projectionItemIdentityKey(event: SessionItemProjectionEvent): string | null {
  const itemID = event.item_id?.trim()
  if (!itemID || event.item.id !== itemID) return null
  const itemTurnID = event.item.turn_id?.trim() ?? ''
  const envelopeTurnID = event.turn_id?.trim() ?? ''
  if (itemTurnID && envelopeTurnID && itemTurnID !== envelopeTurnID) return null
  const itemIteration = Number(event.item.agent_iteration ?? 0)
  const envelopeIteration = event.agent_iteration
  if (!Number.isInteger(itemIteration) || itemIteration < 0) return null
  if (envelopeIteration !== undefined && (!Number.isInteger(Number(envelopeIteration)) || Number(envelopeIteration) < 0)) return null
  if (envelopeIteration !== undefined && Number(envelopeIteration) !== itemIteration) return null
  return sessionItemIdentityKey({
    id: itemID,
    turn_id: envelopeTurnID || itemTurnID,
    agent_iteration: envelopeIteration ?? itemIteration,
  })
}

function compareProjectionEvents(a: SessionItemProjectionEvent, b: SessionItemProjectionEvent): number {
  const aSeq = BigInt(projectionEventRecordSeq(a))
  const bSeq = BigInt(projectionEventRecordSeq(b))
  if (aSeq < bSeq) return -1
  if (aSeq > bSeq) return 1
  return (projectionItemIdentityKey(a) ?? '').localeCompare(projectionItemIdentityKey(b) ?? '')
}

function sessionRevision(session: Session | undefined, fallback = '0'): string {
  return parseDecimalRevision(session?.revision)
    ?? parseDecimalRevision(session?.last_seq)
    ?? parseDecimalRevision(fallback)
    ?? '0'
}

function normalizeSession(session: Session, fallbackRevision = '0'): Session {
  return { ...session, revision: sessionRevision(session, fallbackRevision) }
}

function appendPendingProjectionEvent(
  pending: PendingProjectionEntry | undefined,
  event: SessionItemProjectionEvent,
): PendingProjectionEntry {
  const recordSeq = projectionEventRecordSeq(event)
  const identityKey = projectionItemIdentityKey(event)
  if (!identityKey) return pending ?? {
    events: [], revision: '0', overflowed: false, overflowRevision: '0',
  }
  const current = pending?.events ?? []
  if (current.some((candidate) => projectionEventRecordSeq(candidate) === recordSeq && projectionItemIdentityKey(candidate) === identityKey)) {
    return pending!
  }
  const events = [...current, event].sort(compareProjectionEvents)
  const dropped = events.length > pendingProjectionEventCap ? events.slice(0, events.length - pendingProjectionEventCap) : []
  return {
    // A session without a cached page is deliberately repairable rather than
    // rendered from this queue. Keeping the newest records gives background
    // sessions a hard memory bound; the next snapshot is authoritative for
    // anything evicted here.
    events: events.slice(-pendingProjectionEventCap),
    revision: maxDecimal(pending?.revision ?? '0', event.revision),
    overflowed: Boolean(pending?.overflowed) || dropped.length > 0,
    overflowRevision: dropped.reduce(
      (revision, droppedEvent) => maxDecimal(revision, droppedEvent.revision),
      pending?.overflowRevision ?? '0',
    ),
  }
}

/**
 * Keep pending sessions in insertion order and evict the oldest session when
 * the queue reaches its cap. If an in-flight session is evicted, retain only
 * a resync watermark so the old snapshot cannot be exposed as complete.
 */
function trimPendingProjectionSessions(
  state: SessionStoreState,
  pendingProjectionBySession: Record<string, PendingProjectionEntry>,
  touchedSessionID: string,
): { pendingProjectionBySession: Record<string, PendingProjectionEntry>; snapshotResyncBySession: Record<string, string> } {
  const trimmed = { ...pendingProjectionBySession }
  const snapshotResyncBySession = { ...state.snapshotResyncBySession }
  const touched = trimmed[touchedSessionID]
  if (touched) {
    delete trimmed[touchedSessionID]
    trimmed[touchedSessionID] = touched
  }
  while (Object.keys(trimmed).length > pendingProjectionSessionCap) {
    // The touched session must stay in the map so its current event can be
    // retained. If every other slot is protected, evicting one protected
    // queue is still safe: retain only its overflow watermark and force the
    // in-flight snapshot to resync if it is older than that watermark.
    const oldest = Object.keys(trimmed).find((sessionID) => sessionID !== touchedSessionID)
    if (oldest === undefined) break
    const evicted = trimmed[oldest]
    delete trimmed[oldest]
    if (state.snapshotInFlightBySession[oldest] > 0) {
      snapshotResyncBySession[oldest] = maxDecimal(
        snapshotResyncBySession[oldest] ?? '0',
        maxDecimal(evicted.revision, evicted.overflowRevision),
      )
    }
  }
  return { pendingProjectionBySession: trimmed, snapshotResyncBySession }
}

function applyPendingProjectionEvents(state: SessionStoreState, sessionID: string, snapshotRevision: string): SessionStoreState {
  const pending = state.pendingProjectionBySession[sessionID]
  if (!pending) return state

  const replay = pending.events.filter((event) => !revisionGTE(snapshotRevision, event.revision))
  const pendingProjectionBySession = { ...state.pendingProjectionBySession }
  delete pendingProjectionBySession[sessionID]
  const snapshotResyncBySession = { ...state.snapshotResyncBySession }
  delete snapshotResyncBySession[sessionID]
  let next: SessionStoreState = { ...state, pendingProjectionBySession, snapshotResyncBySession }
  for (const event of replay) {
    next = sessionStoreReducer(next, { type: 'projectionEvent', event })
  }
  return next
}

const eventDrivenSessionFields: Array<keyof Session> = [
  'current_run_id',
  'running_run_id',
  'running_turn_id',
  'interrupted_run_id',
  'interrupted_turn_id',
  'latest_run_id',
  'last_run_id',
  'last_run_status',
  'status',
]

/** Merge a point-in-time DTO that is known to be older than existing state. */
function mergeStaleSessionMetadata(existing: Session, incoming: Session, existingRevision: string): Session {
  const normalizedExistingRevision = parseDecimalRevision(existingRevision) ?? '0'
  const revisionNumber = Number(normalizedExistingRevision)
  const merged: Session = {
    ...normalizeSession(incoming, normalizedExistingRevision),
    revision: normalizedExistingRevision,
    ...(Number.isSafeInteger(revisionNumber) ? { last_seq: revisionNumber } : { last_seq: existing.last_seq }),
  }
  // Preserve the complete event-driven field shape, including cleared
  // (undefined/absent) values. Spreading a stale DTO first is intentional for
  // descriptive metadata, but stale run IDs must not be resurrected.
  for (const field of eventDrivenSessionFields) {
    if (existing[field] === undefined) delete (merged as unknown as Record<string, unknown>)[field]
    else Object.assign(merged, { [field]: existing[field] })
  }
  return merged
}

const settlementSessionFields: Array<keyof Session> = [
  'current_run_id',
  'running_run_id',
  'running_turn_id',
  'interrupted_run_id',
  'interrupted_turn_id',
  'last_run_id',
  'last_run_status',
  'status',
]

/**
 * Merge a terminal run transition independently of the revision ordering of
 * point-in-time session DTOs. The settlement watermark must not move the
 * durable revision backwards, but its run fields are authoritative even when
 * the sidebar DTO was captured at an older revision (or at the same revision
 * with stale lifecycle fields).
 */
function mergeAuthoritativeSettlementMetadata(existing: Session | undefined, incoming: Session, settlementRevision: string): Session {
  if (!existing) {
    const revisionNumber = Number(settlementRevision)
    return {
      // The lifecycle/sidebar DTO is the only metadata available for an
      // uncached session, so use it as the fallback base. The explicit
      // settlement watermark still wins over any stale revision on that DTO.
      ...incoming,
      revision: settlementRevision,
      ...(Number.isSafeInteger(revisionNumber) ? { last_seq: revisionNumber } : {}),
    }
  }

  const existingRevision = sessionRevision(existing)
  const effectiveRevision = maxDecimal(existingRevision, settlementRevision)
  // Do not use the stale sidebar DTO as the merge base for a cached session:
  // it may carry an older display name, provider metadata, or other
  // descriptive fields. Settlement owns only the terminal run fields below.
  const withAuthoritativeRunFields = { ...existing } as Session
  for (const field of settlementSessionFields) {
    if (incoming[field] === undefined) delete (withAuthoritativeRunFields as unknown as Record<string, unknown>)[field]
    else Object.assign(withAuthoritativeRunFields, { [field]: incoming[field] })
  }
  const revisionNumber = Number(effectiveRevision)
  return {
    ...withAuthoritativeRunFields,
    revision: effectiveRevision,
    ...(Number.isSafeInteger(revisionNumber) ? { last_seq: revisionNumber } : {}),
  }
}

function mergeSessionFromList(existing: Session | undefined, incoming: Session): Session {
  const normalizedIncoming = normalizeSession(incoming)
  if (!existing) return normalizedIncoming
  const existingRevision = sessionRevision(existing)
  const incomingRevision = sessionRevision(normalizedIncoming)
  if (revisionGTE(incomingRevision, existingRevision)) return normalizedIncoming
  return mergeStaleSessionMetadata(existing, incoming, existingRevision)
}

function sessionMetadataForSnapshot(existing: Session | undefined, incoming: Session, snapshotRevision: string): Session {
  const normalizedIncoming = normalizeSession(incoming, snapshotRevision)
  if (!existing) return normalizedIncoming
  const existingRevision = sessionRevision(existing)
  return !revisionGTE(snapshotRevision, existingRevision)
    ? mergeStaleSessionMetadata(existing, incoming, existingRevision)
    : normalizedIncoming
}

function initialSnapshotNeedsResync(state: SessionStoreState, sessionID: string, snapshotRevision: string): boolean {
  const pending = state.pendingProjectionBySession[sessionID]
  const pendingOverflowRevision = pending?.overflowed ? pending.overflowRevision : '0'
  const markedRevision = state.snapshotResyncBySession[sessionID] ?? '0'
  return !revisionGTE(snapshotRevision, maxDecimal(pendingOverflowRevision, markedRevision))
}

function applySessionRevision(state: SessionStoreState, sessionID: string, revision: string): Record<string, Session> {
  const existing = state.sessionsByID[sessionID]
  if (!existing) return state.sessionsByID
  const currentRevision = sessionRevision(existing)
  if (revisionGTE(currentRevision, revision)) return state.sessionsByID

  const normalizedRevision = parseDecimalRevision(revision) ?? currentRevision
  const revisionNumber = Number(normalizedRevision)
  return {
    ...state.sessionsByID,
    [sessionID]: {
      ...existing,
      // Keep the decimal string authoritative. Update the compatibility
      // number only while it remains representable without precision loss.
      revision: normalizedRevision,
      ...(Number.isSafeInteger(revisionNumber) ? { last_seq: revisionNumber } : {}),
    },
  }
}

function itemPageWithInsertedItem(page: ItemsPage, item: SessionItem): ItemsPage {
  const items = [...page.items]
  const insertAt = items.findIndex((current) => current.seq > item.seq)
  if (insertAt < 0) items.push(item)
  else items.splice(insertAt, 0, item)
  return {
    ...page,
    items,
    oldest_seq: page.items.length === 0 ? item.seq : Math.min(page.oldest_seq, item.seq),
    newest_seq: page.items.length === 0 ? item.seq : Math.max(page.newest_seq, item.seq),
  }
}

function mergeOlderPage(entry: SessionHistoryEntry, older: ItemsPage, requestRevision: string): SessionHistoryEntry {
  const currentItemsByID = new Map(entry.page.items.map((item) => [item.id, item]))
  const projectionRecordSeqByItemID = { ...entry.projectionRecordSeqByItemID }
  const projectionEventRevisionByItemID = { ...entry.projectionEventRevisionByItemID }
  const snapshotCoverageByItemID = { ...entry.snapshotCoverageByItemID }
  const pendingProjectionByItemID = { ...entry.pendingProjectionByItemID }

  for (const item of older.items) {
    if (currentItemsByID.has(item.id)) continue
    const pending = pendingProjectionByItemID[item.id]
    if (pending && BigInt(pending.revision) > BigInt(requestRevision)) {
      currentItemsByID.set(item.id, pending.item)
      projectionRecordSeqByItemID[item.id] = pending.recordSeq
      projectionEventRevisionByItemID[item.id] = pending.revision
      delete pendingProjectionByItemID[item.id]
    } else {
      currentItemsByID.set(item.id, item)
      if (pending) delete pendingProjectionByItemID[item.id]
      // The response covers at least the revision known when the request was
      // made. This blocks a same-revision replay from rolling the fetched
      // server value back, while a later event remains eligible.
      snapshotCoverageByItemID[item.id] = maxDecimal(snapshotCoverageByItemID[item.id] ?? '0', requestRevision)
      projectionRecordSeqByItemID[item.id] = maxDecimal(projectionRecordSeqByItemID[item.id] ?? '0', requestRevision)
    }
  }

  const items = [...currentItemsByID.values()].sort((a, b) => a.seq - b.seq)
  const currentHasItems = entry.page.items.length > 0
  const page: ItemsPage = {
    ...entry.page,
    items,
    oldest_seq: !currentHasItems ? older.oldest_seq : older.items.length > 0 ? Math.min(older.oldest_seq, entry.page.oldest_seq) : entry.page.oldest_seq,
    newest_seq: currentHasItems ? entry.page.newest_seq : older.newest_seq,
    has_more_before: older.has_more_before,
    has_more_after: false,
  }
  return {
    ...entry,
    page,
    projectionRecordSeqByItemID,
    projectionEventRevisionByItemID,
    snapshotCoverageByItemID,
    pendingProjectionByItemID,
  }
}

function applyProjectionEventToEntry(entry: SessionHistoryEntry, event: SessionItemProjectionEvent): SessionHistoryEntry | null {
  const itemID = event.item.id
  const identityKey = projectionItemIdentityKey(event)
  if (!itemID || !identityKey) return null

  // A globally unique backend item id is expected, but reject a corrupted
  // replay that tries to reuse it under a different complete identity.
  const conflictingItem = entry.page.items.find((item) => item.id === itemID && sessionItemIdentityKey(item) !== identityKey)
    ?? entry.pendingProjectionByItemID[itemID]?.item
  if (conflictingItem && sessionItemIdentityKey(conflictingItem) !== identityKey) return null

  // A snapshot at this revision is complete authority for that revision.
  // Events at an older or covered revision are replay/stale data, even when
  // their durable record sequence is otherwise unfamiliar to this window.
  if (revisionGTE(entry.revision, event.revision) && entry.revision !== event.revision) return null
  const snapshotCoverage = entry.snapshotCoverageByItemID[itemID]
  if (snapshotCoverage && revisionGTE(snapshotCoverage, event.revision)) return null

  const recordSeq = String(event.seq)
  const previousRecordSeq = entry.projectionRecordSeqByItemID[itemID]
    ?? entry.pendingProjectionByItemID[itemID]?.recordSeq
  if (previousRecordSeq !== undefined && BigInt(previousRecordSeq) >= BigInt(recordSeq)) return null

  const projectionRecordSeqByItemID = { ...entry.projectionRecordSeqByItemID, [itemID]: recordSeq }
  const projectionEventRevisionByItemID = { ...entry.projectionEventRevisionByItemID, [itemID]: event.revision }
  const pendingProjectionByItemID = { ...entry.pendingProjectionByItemID }
  const itemIndex = entry.page.items.findIndex((item) => item.id === itemID)
  let page = entry.page

  if (event.type === 'item.updated' && itemIndex < 0) {
    // Do not append an update that belongs to an unloaded older page. Keep it
    // until that page is fetched, where it can replace the matching ID.
    const previousPending = pendingProjectionByItemID[itemID]
    if (!previousPending || BigInt(previousPending.recordSeq) < BigInt(recordSeq)) {
      pendingProjectionByItemID[itemID] = { item: event.item, recordSeq, revision: event.revision }
    }
  } else if (itemIndex >= 0) {
    // Both duplicate creates and updates replace the item in place. In
    // particular, an update never re-sorts the surrounding history window.
    const items = [...entry.page.items]
    items[itemIndex] = event.item
    page = { ...entry.page, items }
    delete pendingProjectionByItemID[itemID]
  } else {
    // A new item is inserted by its creation sequence. This is also the
    // idempotent upsert path for a repeated create notification.
    page = itemPageWithInsertedItem(entry.page, event.item)
    delete pendingProjectionByItemID[itemID]
  }

  return {
    ...entry,
    page,
    revision: maxDecimal(entry.revision, event.revision),
    appliedProjectionRecordSeq: maxDecimal(entry.appliedProjectionRecordSeq, recordSeq),
    projectionRecordSeqByItemID,
    projectionEventRevisionByItemID,
    pendingProjectionByItemID,
  }
}

export function sessionStoreReducer(state: SessionStoreState, action: SessionStoreAction): SessionStoreState {
  switch (action.type) {
    case 'snapshotStarted': {
      return {
        ...state,
        snapshotInFlightBySession: {
          ...state.snapshotInFlightBySession,
          [action.sessionID]: (state.snapshotInFlightBySession[action.sessionID] ?? 0) + 1,
        },
      }
    }

    case 'snapshotFinished': {
      const inFlight = state.snapshotInFlightBySession[action.sessionID] ?? 0
      if (inFlight === 0) return state
      const snapshotInFlightBySession = { ...state.snapshotInFlightBySession }
      if (inFlight <= 1) delete snapshotInFlightBySession[action.sessionID]
      else snapshotInFlightBySession[action.sessionID] = inFlight - 1
      return { ...state, snapshotInFlightBySession }
    }

    case 'snapshot': {
      const { expectedSessionID } = action
      const snapshotRevision = parseDecimalRevision(action.snapshot.revision)
      if (snapshotRevision === null) return state
      const snapshot = action.snapshot.revision === snapshotRevision
        ? action.snapshot
        : { ...action.snapshot, revision: snapshotRevision }
      // Invariant 1: identity must match.
      if (snapshot.session_id !== expectedSessionID) return state

      const existing = state.historyBySession[snapshot.session_id]
      // If the bounded pre-snapshot queue overflowed, an old response cannot
      // establish a complete base: the dropped records may be newer than it.
      // Keep the queue non-rendered and let the hook request a newer snapshot.
      if (!existing && initialSnapshotNeedsResync(state, snapshot.session_id, snapshot.revision)) {
        const currentSession = state.sessionsByID[snapshot.session_id]
        const sessionsByID = {
          ...state.sessionsByID,
          [snapshot.session_id]: sessionMetadataForSnapshot(currentSession, snapshot.session, snapshot.revision),
        }
        return { ...state, sessionsByID }
      }
      const snapshotResyncBySession = { ...state.snapshotResyncBySession }
      delete snapshotResyncBySession[snapshot.session_id]
      const stateForSnapshot = { ...state, snapshotResyncBySession }
      // Invariant 3: older revision must not overwrite newer history.
      // But always update sessionsByID — metadata-only changes (rename, full
      // access toggle) don't change LastSeq but must still update the session.
      if (existing && !revisionGTE(snapshot.revision, existing.revision)) {
        // The snapshot is stale: just update session metadata in sessionsByID.
        const currentSession = state.sessionsByID[snapshot.session_id]
        const sessionsByID = {
          ...state.sessionsByID,
          [snapshot.session_id]: currentSession
            ? mergeStaleSessionMetadata(currentSession, snapshot.session, existing.revision)
            : snapshot.session,
        }
        return { ...stateForSnapshot, sessionsByID }
      }
      if (existing && compareRevisions(existing.revision, snapshot.revision) === 0) {
        // An equal-revision snapshot is still authoritative for the items it
        // returns. Merge it into the current window so a create from the same
        // transaction that was missed by the event stream is recovered, while
        // retaining payloads for items whose same/higher-revision event was
        // already observed locally.
        const mergedBase = mergeRefreshedPage(existing.page, snapshot.history)
        const snapshotItemsByID = new Map(snapshot.history.items.map((item) => [item.id, item]))
        const merged = {
          ...mergedBase,
          items: mergedBase.items.map((item) => {
            const snapshotItem = snapshotItemsByID.get(item.id)
            if (!snapshotItem) return item
            const eventRevision = existing.projectionEventRevisionByItemID[item.id]
            const eventItem = existing.page.items.find((current) => current.id === item.id)
              ?? existing.pendingProjectionByItemID[item.id]?.item
            return eventRevision && eventItem && revisionGTE(eventRevision, snapshot.revision) ? eventItem : snapshotItem
          }),
        }
        const projectionRecordSeqByItemID = { ...existing.projectionRecordSeqByItemID }
        const snapshotCoverageByItemID = { ...existing.snapshotCoverageByItemID }
        for (const item of snapshot.history.items) {
          snapshotCoverageByItemID[item.id] = maxDecimal(
            snapshotCoverageByItemID[item.id] ?? '0',
            snapshot.revision,
          )
          projectionRecordSeqByItemID[item.id] = maxDecimal(
            projectionRecordSeqByItemID[item.id] ?? '0',
            snapshot.revision,
          )
        }
        const pendingProjectionByItemID = { ...existing.pendingProjectionByItemID }
        const snapshotItemIDs = new Set(snapshot.history.items.map((item) => item.id))
        for (const [itemID, pending] of Object.entries(pendingProjectionByItemID)) {
          if (snapshotItemIDs.has(itemID) && revisionGTE(snapshot.revision, pending.revision)) delete pendingProjectionByItemID[itemID]
        }
        const historyBySession = {
          ...state.historyBySession,
          [snapshot.session_id]: {
            ...existing,
            page: merged,
            snapshotCoverageRevision: maxDecimal(existing.snapshotCoverageRevision, snapshot.revision),
            snapshotCoverageByItemID,
            appliedProjectionRecordSeq: maxDecimal(existing.appliedProjectionRecordSeq, snapshot.revision),
            projectionRecordSeqByItemID,
            pendingProjectionByItemID,
          },
        }
        touchLRU(historyBySession, snapshot.session_id)
        const sessionsByID = {
          ...state.sessionsByID,
          [snapshot.session_id]: sessionMetadataForSnapshot(state.sessionsByID[snapshot.session_id], snapshot.session, snapshot.revision),
        }
        return applyPendingProjectionEvents({ ...stateForSnapshot, historyBySession, sessionsByID }, snapshot.session_id, snapshot.revision)
      }

      const mergedBase = mergeRefreshedPage(existing?.page ?? null, snapshot.history)
      const snapshotItemsByID = new Map(snapshot.history.items.map((item) => [item.id, item]))
      const merged = {
        ...mergedBase,
        items: mergedBase.items.map((item) => {
          const snapshotItem = snapshotItemsByID.get(item.id)
          if (!snapshotItem) return item
          // If an event for this item was already observed at this revision,
          // retain that committed event payload. This handles an equal-
          // revision snapshot racing an event without rolling the item back.
          const eventRevision = existing?.projectionEventRevisionByItemID[item.id]
          const eventItem = existing?.page.items.find((current) => current.id === item.id)
          return eventRevision && eventItem && revisionGTE(eventRevision, snapshot.revision) ? eventItem : snapshotItem
        }),
      }
      const historyBySession = { ...state.historyBySession }
      const snapshotCoverageRevision = snapshot.revision
      const projectionRecordSeqByItemID = { ...(existing?.projectionRecordSeqByItemID ?? {}) }
      const snapshotCoverageByItemID = { ...(existing?.snapshotCoverageByItemID ?? {}) }
      const projectionEventRevisionByItemID = { ...(existing?.projectionEventRevisionByItemID ?? {}) }
      for (const item of snapshot.history.items) {
        snapshotCoverageByItemID[item.id] = maxDecimal(
          snapshotCoverageByItemID[item.id] ?? '0',
          snapshotCoverageRevision,
        )
        projectionRecordSeqByItemID[item.id] = maxDecimal(
          projectionRecordSeqByItemID[item.id] ?? '0',
          snapshotCoverageRevision,
        )
      }
      const pendingProjectionByItemID = { ...(existing?.pendingProjectionByItemID ?? {}) }
      const snapshotItemIDs = new Set(snapshot.history.items.map((item) => item.id))
      for (const [itemID, pending] of Object.entries(pendingProjectionByItemID)) {
        if (snapshotItemIDs.has(itemID) && revisionGTE(snapshotCoverageRevision, pending.revision)) delete pendingProjectionByItemID[itemID]
      }
      historyBySession[snapshot.session_id] = {
        page: merged,
        revision: snapshot.revision,
        snapshotCoverageRevision,
        snapshotCoverageByItemID,
        appliedProjectionRecordSeq: maxDecimal(existing?.appliedProjectionRecordSeq ?? '0', snapshotCoverageRevision),
        projectionRecordSeqByItemID,
        projectionEventRevisionByItemID,
        pendingProjectionByItemID,
      }
      touchLRU(historyBySession, snapshot.session_id)

      const sessionsByID = {
        ...state.sessionsByID,
        [snapshot.session_id]: sessionMetadataForSnapshot(state.sessionsByID[snapshot.session_id], snapshot.session, snapshot.revision),
      }

      return applyPendingProjectionEvents({ ...stateForSnapshot, historyBySession, sessionsByID }, snapshot.session_id, snapshot.revision)
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
      for (const s of sessions) sessionsByID[s.id] = mergeSessionFromList(sessionsByID[s.id], s)
      const listGenerationByProject = { ...state.listGenerationByProject, [projectID]: generation }

      return { ...state, sessionIDsByProject, sessionsByID, listGenerationByProject }
    }

    case 'pageOlder': {
      const { sessionID, older, requestRevision } = action
      if (parseDecimalRevision(requestRevision) === null) return state
      const existing = state.historyBySession[sessionID]
      if (!existing) return state
      const merged = mergeOlderPage(existing, older, requestRevision)
      const historyBySession = { ...state.historyBySession, [sessionID]: merged }
      touchLRU(historyBySession, sessionID)
      return { ...state, historyBySession }
    }

    case 'projectionEvent': {
      const revision = parseDecimalRevision(action.event.revision)
      const recordSeq = parseDecimalRevision(action.event.seq)
      if (revision === null || recordSeq === null || !action.event.session_id || !action.event.item?.id || !projectionItemIdentityKey(action.event)) return state
      const event = action.event.revision === revision
        ? action.event
        : { ...action.event, revision }
      const sessionID = event.session_id
      const existing = state.historyBySession[sessionID]
      const metadataRevision = existing && revisionGTE(existing.revision, event.revision)
        ? existing.revision
        : event.revision
      const sessionsByID = applySessionRevision(state, sessionID, metadataRevision)
      // A live event must never manufacture a complete history window for a
      // session that has not been snapshotted. The selection fetch will load
      // the authoritative page when that session is opened.
      if (!existing) {
        const pending = state.pendingProjectionBySession[sessionID]
        const nextPending = appendPendingProjectionEvent(pending, event)
        let pendingProjectionBySession = state.pendingProjectionBySession
        let snapshotResyncBySession = state.snapshotResyncBySession
        if (nextPending !== pending) {
          const trimmed = trimPendingProjectionSessions(
            state,
            { ...state.pendingProjectionBySession, [sessionID]: nextPending },
            sessionID,
          )
          pendingProjectionBySession = trimmed.pendingProjectionBySession
          snapshotResyncBySession = trimmed.snapshotResyncBySession
        }
        if (sessionsByID === state.sessionsByID && pendingProjectionBySession === state.pendingProjectionBySession && snapshotResyncBySession === state.snapshotResyncBySession) return state
        return { ...state, sessionsByID, pendingProjectionBySession, snapshotResyncBySession }
      }

      const nextEntry = applyProjectionEventToEntry(existing, event)
      if (!nextEntry) {
        return sessionsByID === state.sessionsByID ? state : { ...state, sessionsByID }
      }
      const historyBySession = { ...state.historyBySession, [sessionID]: nextEntry }
      touchLRU(historyBySession, sessionID)
      return { ...state, sessionsByID, historyBySession }
    }

    case 'sessionMetadata': {
      const incoming = normalizeSession(action.session)
      const existing = state.sessionsByID[incoming.id]
      // Lifecycle/session DTOs are point-in-time metadata. Reuse the same
      // revision-aware merge as session lists: a stale DTO may update safe
      // descriptive fields, but it must not resurrect event-driven run IDs or
      // statuses that a newer projection/lifecycle transition cleared.
      const merged = mergeSessionFromList(existing, incoming)
      const sessionsByID = { ...state.sessionsByID, [incoming.id]: merged }
      return { ...state, sessionsByID }
    }

    case 'settlementMetadata': {
      const revision = parseDecimalRevision(action.revision)
      if (revision === null || !action.session.id) return state
      const existing = state.sessionsByID[action.session.id]
      const merged = mergeAuthoritativeSettlementMetadata(existing, action.session, revision)
      const sessionsByID = { ...state.sessionsByID, [action.session.id]: merged }
      return { ...state, sessionsByID }
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
      const pendingProjectionBySession = { ...state.pendingProjectionBySession }
      delete pendingProjectionBySession[sessionID]
      const sessionsByID = { ...state.sessionsByID }
      delete sessionsByID[sessionID]
      const metaBySession = { ...state.metaBySession }
      delete metaBySession[sessionID]
      const snapshotInFlightBySession = { ...state.snapshotInFlightBySession }
      delete snapshotInFlightBySession[sessionID]
      const snapshotResyncBySession = { ...state.snapshotResyncBySession }
      delete snapshotResyncBySession[sessionID]
      return { ...state, historyBySession, pendingProjectionBySession, snapshotInFlightBySession, snapshotResyncBySession, sessionsByID, metaBySession }
    }

    default:
      return state
  }
}
