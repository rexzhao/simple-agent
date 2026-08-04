import { describe, expect, it } from 'vitest'
import type { ItemsPage, Session, SessionItem, SessionItemProjectionEvent, SessionSnapshot } from '../types'
import { initialSessionStoreState, mergeRefreshedPage, parseDecimalRevision, pendingProjectionEventCap, pendingProjectionSessionCap, revisionGTE, sessionStoreReducer } from './sessionStore'

const session = (id: string, lastSeq = 0): Session => ({ id, project_id: 'p', display_name: id, last_seq: lastSeq } as Session)
const page = (seqs: number[]): ItemsPage => ({
  items: seqs.map((seq) => ({ id: `item-${seq}`, seq } as never)),
  oldest_seq: seqs[0] ?? 0,
  newest_seq: seqs[seqs.length - 1] ?? 0,
  has_more_before: seqs[0] > 1,
  has_more_after: false,
})
const item = (id: string, seq: number, text = id): SessionItem => ({
  id,
  seq,
  created_at: '',
  kind: 'message',
  visibility: 'visible',
  audience: 'user',
  message: { role: 'user', content: { inline: text } },
})
const projectionEvent = (type: SessionItemProjectionEvent['type'], id: string, itemSeq: number, recordSeq: number, revision: string, text = id, sessionID = 's1'): SessionItemProjectionEvent => ({
  type,
  session_id: sessionID,
  seq: recordSeq,
  revision,
  item_id: id,
  item: item(id, itemSeq, text),
})
const snapshot = (sessionID: string, revision: string, seqs: number[]): SessionSnapshot => ({
  session_id: sessionID,
  revision,
  session: session(sessionID, Number(revision)),
  history: page(seqs),
})

describe('revisionGTE', () => {
  it('compares as integers, not dictionary order', () => {
    expect(revisionGTE('10', '9')).toBe(true)
    expect(revisionGTE('9', '10')).toBe(false)
    expect(revisionGTE('100', '99')).toBe(true)
    expect(revisionGTE('42', '42')).toBe(true)
  })

  it('rejects malformed revisions without throwing', () => {
    expect(parseDecimalRevision('not-a-revision')).toBeNull()
    expect(parseDecimalRevision('-1')).toBeNull()
    expect(revisionGTE('not-a-revision', '1')).toBe(false)
    expect(revisionGTE('1', 'not-a-revision')).toBe(false)
  })

  it('compares revisions beyond the JavaScript safe integer range', () => {
    expect(revisionGTE('90071992547409930', '90071992547409929')).toBe(true)
    expect(revisionGTE('90071992547409929', '90071992547409930')).toBe(false)
  })
})

describe('mergeRefreshedPage', () => {
  it('preserves older prefix when refreshed page overlaps', () => {
    const current = page([5, 6, 7, 8, 9, 10])
    const refreshed = page([8, 9, 10, 11])
    const merged = mergeRefreshedPage(current, refreshed)
    expect(merged.items.map((i) => i.seq)).toEqual([5, 6, 7, 8, 9, 10, 11])
    expect(merged.oldest_seq).toBe(5)
    expect(merged.newest_seq).toBe(11)
  })

  it('replaces when ranges do not overlap', () => {
    const current = page([1, 2, 3, 4, 5])
    const refreshed = page([30, 31, 32])
    const merged = mergeRefreshedPage(current, refreshed)
    expect(merged.items.map((i) => i.seq)).toEqual([30, 31, 32])
  })

  it('returns refreshed when current is null', () => {
    const refreshed = page([1, 2, 3])
    expect(mergeRefreshedPage(null, refreshed)).toBe(refreshed)
  })
})

describe('sessionStoreReducer', () => {
  describe('snapshot action', () => {
    it('applies a snapshot for a new session', () => {
      const state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession['s1'].revision).toBe('5')
      expect(state.sessionsByID['s1'].id).toBe('s1')
    })

    it('merges equal-revision snapshot items without rolling back an observed update', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '9', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, {
        type: 'projectionEvent',
        event: projectionEvent('item.updated', 'item-2', 2, 20, '10', 'event update'),
      })
      // Same revision but different display_name (metadata-only change).
      const renamedSnapshot: SessionSnapshot = {
        session_id: 's1',
        revision: '10',
        session: { ...session('s1'), display_name: 'Renamed' } as Session,
        history: { items: [{ id: 'item-1', seq: 1 } as never, { id: 'item-2', seq: 2 } as never, { id: 'item-3', seq: 3 } as never, { id: 'item-4', seq: 4 } as never], oldest_seq: 1, newest_seq: 4, has_more_before: false, has_more_after: false },
      }
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: renamedSnapshot,
        expectedSessionID: 's1',
      })
      // The snapshot fills the missed same-revision create, while the event
      // payload remains authoritative for the item already observed.
      expect(state.historyBySession['s1'].revision).toBe('10')
      expect(state.historyBySession['s1'].page.items).toHaveLength(4)
      expect(state.historyBySession['s1'].page.items[1].message?.content?.inline).toBe('event update')
      // But session metadata updated.
      expect(state.sessionsByID['s1'].display_name).toBe('Renamed')
    })

    it('discards history from a snapshot with older revision', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2, 3, 4]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession['s1'].revision).toBe('10')
      expect(state.historyBySession['s1'].page.items).toHaveLength(3)
    })

    it('accepts a snapshot with newer revision', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [3, 4, 5]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession['s1'].revision).toBe('10')
      expect(state.historyBySession['s1'].page.items.map((i) => i.seq)).toEqual([1, 2, 3, 4, 5])
    })

    it('rejects identity mismatch', () => {
      const state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('a', '5', [1, 2]),
        expectedSessionID: 'b',
      })
      expect(state.historyBySession).toEqual({})
    })

    it('ignores an invalid revision instead of throwing', () => {
      const state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', 'not-a-revision', [1]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession).toEqual({})
    })

    it('handles out-of-order snapshots (old resolved first)', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [5, 6, 7]),
        expectedSessionID: 's1',
      })
      // Old snapshot arrives late
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession['s1'].revision).toBe('10')
    })
  })

  it('ignores a projection event with an invalid watermark', () => {
    const state = sessionStoreReducer(initialSessionStoreState(), {
      type: 'projectionEvent',
      event: { ...projectionEvent('item.created', 'item-1', 1, 1, '1'), revision: 'oops' },
    })
    expect(state).toEqual(initialSessionStoreState())
  })

  describe('sessions action', () => {
    it('applies session list with generation', () => {
      const state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p1',
        sessions: [session('s1'), session('s2')],
        archived: false,
        generation: 1,
      })
      expect(state.sessionIDsByProject['p1'].active).toEqual(['s1', 's2'])
      expect(state.listGenerationByProject['p1']).toBe(1)
    })

    it('discards stale generation', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p1',
        sessions: [session('s1'), session('s2')],
        archived: false,
        generation: 2,
      })
      state = sessionStoreReducer(state, {
        type: 'sessions',
        projectID: 'p1',
        sessions: [session('s1')],
        archived: false,
        generation: 1,
      })
      expect(state.sessionIDsByProject['p1'].active).toEqual(['s1', 's2'])
    })

    it('preserves a higher projection revision while applying list metadata', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p',
        sessions: [{
          ...session('s1'),
          status: 'running',
          current_run_id: 'run-new',
          running_run_id: 'run-new',
          running_turn_id: 'turn-new',
          latest_run_id: 'run-new',
          last_run_id: 'run-new',
          last_run_status: 'running',
        } as Session],
        archived: false,
        generation: 1,
      })
      state = sessionStoreReducer(state, {
        type: 'projectionEvent',
        event: projectionEvent('item.appended', 'item-1', 1, 100, '100'),
      })
      state = sessionStoreReducer(state, {
        type: 'sessions',
        projectID: 'p',
        sessions: [{ ...session('s1', 5), revision: '5', display_name: 'renamed', status: 'idle' } as Session],
        archived: false,
        generation: 2,
      })
      expect(state.sessionsByID.s1.display_name).toBe('renamed')
      expect(state.sessionsByID.s1.revision).toBe('100')
      expect(state.sessionsByID.s1.last_seq).toBe(100)
      expect(state.sessionsByID.s1.status).toBe('running')
      expect(state.sessionsByID.s1.current_run_id).toBe('run-new')
      expect(state.sessionsByID.s1.latest_run_id).toBe('run-new')
      expect(state.sessionsByID.s1.last_run_status).toBe('running')
    })

    it('does not resurrect cleared run fields from a stale list', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p',
        sessions: [{ ...session('s1', 100), revision: '100', display_name: 'settled', status: 'completed' } as Session],
        archived: false,
        generation: 1,
      })
      state = sessionStoreReducer(state, {
        type: 'sessions',
        projectID: 'p',
        sessions: [{
          ...session('s1', 5),
          revision: '5',
          display_name: 'renamed by stale list',
          status: 'running',
          current_run_id: 'old-run',
          running_run_id: 'old-run',
          interrupted_run_id: 'old-run',
        } as Session],
        archived: false,
        generation: 2,
      })
      expect(state.sessionsByID.s1.display_name).toBe('renamed by stale list')
      expect(state.sessionsByID.s1.status).toBe('completed')
      expect(state.sessionsByID.s1.current_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.running_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.interrupted_run_id).toBeUndefined()
    })

    it('does not resurrect settled run metadata from stale lifecycle DTOs', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p',
        sessions: [{
          ...session('s1', 100), revision: '100', display_name: 'settled', status: 'idle',
          last_run_id: 'run-new', last_run_status: 'committed',
        } as Session],
        archived: false,
        generation: 1,
      })
      state = sessionStoreReducer(state, {
        type: 'sessionMetadata',
        session: {
          ...session('s1', 5), revision: '5', display_name: 'renamed by lifecycle', status: 'running',
          current_run_id: 'old-run', running_run_id: 'old-run', running_turn_id: 'old-turn',
          last_run_id: 'old-run', last_run_status: 'running',
        } as Session,
      })
      expect(state.sessionsByID.s1.display_name).toBe('renamed by lifecycle')
      expect(state.sessionsByID.s1.revision).toBe('100')
      expect(state.sessionsByID.s1.status).toBe('idle')
      expect(state.sessionsByID.s1.current_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.running_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.running_turn_id).toBeUndefined()
      expect(state.sessionsByID.s1.last_run_id).toBe('run-new')
      expect(state.sessionsByID.s1.last_run_status).toBe('committed')
    })

    it('lets an explicit settlement win over stale or equal-revision run fields', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p',
        sessions: [{
          ...session('s1', 100), revision: '100', display_name: 'authoritative name', status: 'running',
          current_run_id: 'run-live', running_run_id: 'run-live', running_turn_id: 'turn-live',
        } as Session],
        archived: false,
        generation: 1,
      })
      state = sessionStoreReducer(state, {
        type: 'settlementMetadata',
        revision: '100',
        session: {
          ...session('s1', 5), revision: '5', display_name: 'sidebar copy', status: 'idle',
          current_run_id: undefined, running_run_id: undefined, running_turn_id: undefined,
          interrupted_run_id: undefined, interrupted_turn_id: undefined,
          last_run_id: 'run-settled', last_run_status: 'committed',
        } as Session,
      })
      expect(state.sessionsByID.s1.display_name).toBe('authoritative name')
      expect(state.sessionsByID.s1.revision).toBe('100')
      expect(state.sessionsByID.s1.status).toBe('idle')
      expect(state.sessionsByID.s1.current_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.running_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.running_turn_id).toBeUndefined()
      expect(state.sessionsByID.s1.last_run_id).toBe('run-settled')
      expect(state.sessionsByID.s1.last_run_status).toBe('committed')
    })

    it('does not resurrect cleared run fields from a stale snapshot', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: { ...snapshot('s1', '100', [1]), session: { ...session('s1', 100), display_name: 'settled', status: 'completed' } as Session },
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: {
          ...snapshot('s1', '5', [1, 2]),
          session: {
            ...session('s1', 5),
            display_name: 'renamed by stale snapshot',
            status: 'running',
            current_run_id: 'old-run',
            running_run_id: 'old-run',
            interrupted_run_id: 'old-run',
          } as Session,
        },
        expectedSessionID: 's1',
      })
      expect(state.sessionsByID.s1.display_name).toBe('renamed by stale snapshot')
      expect(state.sessionsByID.s1.status).toBe('completed')
      expect(state.sessionsByID.s1.current_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.running_run_id).toBeUndefined()
      expect(state.sessionsByID.s1.interrupted_run_id).toBeUndefined()
    })
  })

  describe('pageOlder action', () => {
    it('prepends older items', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [5, 6, 7]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, {
        type: 'pageOlder',
        sessionID: 's1',
        older: page([2, 3, 4]),
        requestRevision: '10',
      })
      expect(state.historyBySession['s1'].page.items.map((i) => i.seq)).toEqual([2, 3, 4, 5, 6, 7])
      expect(state.historyBySession['s1'].page.oldest_seq).toBe(2)
    })

    it('ignores pageOlder for unknown session', () => {
      const state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'pageOlder',
        sessionID: 'unknown',
        older: page([1, 2]),
        requestRevision: '0',
      })
      expect(state.historyBySession).toEqual({})
    })
  })

  describe('projectionEvent action', () => {
    const cached = (revision = '0', seqs = [1, 2, 3]) => sessionStoreReducer(initialSessionStoreState(), {
      type: 'snapshot',
      snapshot: snapshot('s1', revision, seqs),
      expectedSessionID: 's1',
    })

    it('upserts duplicate creates by item id and keeps them idempotent', () => {
      let state = cached()
      const created = projectionEvent('item.appended', 'created-1', 4, 4, '4', 'same')
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: created })
      const applied = state
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: created })
      expect(state).toBe(applied)
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'item-2', 'item-3', 'created-1'])
    })

    it('keeps same-text items distinct and inserts same-revision events by item sequence', () => {
      let state = cached()
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.created', 'created-a', 4, 4, '9', 'same') })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.appended', 'created-b', 5, 5, '9', 'same') })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'item-2', 'item-3', 'created-a', 'created-b'])
    })

    it('updates an item in place without changing its history position', () => {
      let state = cached()
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.updated', 'item-2', 2, 12, '12', 'updated') })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'item-2', 'item-3'])
      expect(state.historyBySession.s1.page.items[1].message?.content?.inline).toBe('updated')
    })

    it('does not let an old create replay overwrite a newer update', () => {
      let state = cached()
      const create = projectionEvent('item.appended', 'item-2', 2, 12, '20', 'created')
      const update = projectionEvent('item.updated', 'item-2', 2, 17, '20', 'updated')
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: create })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: update })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: create })
      expect(state.historyBySession.s1.page.items[1].message?.content?.inline).toBe('updated')
    })

    it('does not insert an update outside the current page, then applies it after older paging', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [5, 6]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.updated', 'item-3', 3, 11, '11', 'updated') })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-5', 'item-6'])
      state = sessionStoreReducer(state, { type: 'pageOlder', sessionID: 's1', older: page([1, 2, 3, 4]), requestRevision: '10' })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'item-2', 'item-3', 'item-4', 'item-5', 'item-6'])
      expect(state.historyBySession.s1.page.items[2].message?.content?.inline).toBe('updated')
    })

    it('does not let a same-revision replay roll back an older page response', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [5, 6]),
        expectedSessionID: 's1',
      })
      const older: ItemsPage = { ...page([1, 2, 3, 4]), items: [item('item-1', 1, 'server'), item('item-2', 2), item('item-3', 3, 'server'), item('item-4', 4)] }
      state = sessionStoreReducer(state, { type: 'pageOlder', sessionID: 's1', older, requestRevision: '10' })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.updated', 'item-3', 3, 9, '10', 'same revision update') })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'item-2', 'item-3', 'item-4', 'item-5', 'item-6'])
      expect(state.historyBySession.s1.page.items[2].message?.content?.inline).toBe('server')
    })

    it('prefers a higher-revision update received while older paging is in flight', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [5, 6]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.updated', 'item-3', 3, 11, '11', 'newer event') })
      const older: ItemsPage = { ...page([1, 2, 3, 4]), items: [item('item-1', 1), item('item-2', 2), item('item-3', 3, 'old page'), item('item-4', 4)] }
      state = sessionStoreReducer(state, { type: 'pageOlder', sessionID: 's1', older, requestRevision: '10' })
      expect(state.historyBySession.s1.page.items[2].message?.content?.inline).toBe('newer event')
    })

    it('does not create a complete history for an uncached session', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'sessions',
        projectID: 'p',
        sessions: [session('s1')],
        archived: false,
        generation: 1,
      })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.appended', 'item-1', 1, 1, '1') })
      expect(state.historyBySession.s1).toBeUndefined()
      expect(state.sessionsByID.s1.revision).toBe('1')
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '0', [1]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1'])
      expect(state.sessionsByID.s1.revision).toBe('1')
    })

    it('keeps uncached events until an older first snapshot establishes the base', () => {
      let state = initialSessionStoreState()
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.created', 'created-1', 2, 2, '2') })
      expect(state.historyBySession.s1).toBeUndefined()
      expect(state.pendingProjectionBySession.s1.events).toHaveLength(1)

      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '1', [1]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'created-1'])
      expect(state.pendingProjectionBySession.s1).toBeUndefined()
      expect(state.historyBySession.s1.revision).toBe('2')
    })

    it('replays a pending create and update for the same item in record order', () => {
      let state = initialSessionStoreState()
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.created', 'created-1', 2, 2, '2', 'created') })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.updated', 'created-1', 2, 3, '3', 'updated') })
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '1', [1]),
        expectedSessionID: 's1',
      })
      const projected = state.historyBySession.s1.page.items.find((current) => current.id === 'created-1')
      expect(projected?.message?.content?.inline).toBe('updated')
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'created-1'])
    })

    it('deduplicates pending replay and clears it with the session', () => {
      const event = projectionEvent('item.created', 'created-1', 2, 2, '2')
      let state = sessionStoreReducer(initialSessionStoreState(), { type: 'projectionEvent', event })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event })
      expect(state.pendingProjectionBySession.s1.events).toHaveLength(1)
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '1', [1]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, { type: 'projectionEvent', event })
      expect(state.historyBySession.s1.page.items.filter((current) => current.id === 'created-1')).toHaveLength(1)
      state = sessionStoreReducer(state, { type: 'clearSession', sessionID: 's1' })
      expect(state.pendingProjectionBySession.s1).toBeUndefined()
      expect(state.historyBySession.s1).toBeUndefined()
    })

    it('updates an already cached background session without selecting it', () => {
      let state = cached()
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s2', '10', [10]),
        expectedSessionID: 's2',
      })
      state = sessionStoreReducer(state, {
        type: 'projectionEvent',
        event: projectionEvent('item.updated', 'item-10', 10, 11, '11', 'background update', 's2'),
      })
      expect(state.historyBySession.s2.page.items[0].message?.content?.inline).toBe('background update')
      expect(state.historyBySession.s1.page.items.map((current) => current.id)).toEqual(['item-1', 'item-2', 'item-3'])
    })

    it('keeps event and snapshot races monotonic at older and equal revisions', () => {
      let state = cached()
      const update = projectionEvent('item.updated', 'item-2', 2, 17, '20', 'updated')
      state = sessionStoreReducer(state, { type: 'projectionEvent', event: update })
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: { ...snapshot('s1', '20', [1, 2, 3]), history: { ...page([1, 2, 3]), items: [item('item-1', 1), item('item-2', 2, 'stale'), item('item-3', 3)] } },
        expectedSessionID: 's1',
      })
      expect(state.historyBySession.s1.page.items[1].message?.content?.inline).toBe('updated')

      state = sessionStoreReducer(state, { type: 'projectionEvent', event: projectionEvent('item.updated', 'item-2', 2, 16, '20', 'old replay') })
      expect(state.historyBySession.s1.page.items[1].message?.content?.inline).toBe('updated')
    })
  })

  describe('clearSession action', () => {
    it('removes session from all maps', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2]),
        expectedSessionID: 's1',
      })
      state = sessionStoreReducer(state, { type: 'clearSession', sessionID: 's1' })
      expect(state.historyBySession['s1']).toBeUndefined()
      expect(state.sessionsByID['s1']).toBeUndefined()
    })
  })

  describe('LRU eviction', () => {
    it('evicts oldest history when exceeding cap', () => {
      let state = initialSessionStoreState()
      for (let i = 0; i < 12; i++) {
        state = sessionStoreReducer(state, {
          type: 'snapshot',
          snapshot: snapshot(`s${i}`, '1', [1]),
          expectedSessionID: `s${i}`,
        })
      }
      // Cap is 10, so s0 and s1 should be evicted
      expect(state.historyBySession['s0']).toBeUndefined()
      expect(state.historyBySession['s1']).toBeUndefined()
      expect(state.historyBySession['s2']).toBeDefined()
      expect(state.historyBySession['s11']).toBeDefined()
      expect(Object.keys(state.historyBySession)).toHaveLength(10)
    })
  })

  describe('uncached projection queue capacity', () => {
    it('bounds background session and per-session pending queues', () => {
      let state = initialSessionStoreState()
      for (let i = 0; i < pendingProjectionSessionCap + 8; i++) {
        state = sessionStoreReducer(state, {
          type: 'projectionEvent',
          event: projectionEvent('item.created', `session-${i}`, i + 1, i + 1, String(i + 1), `session-${i}`, `background-${i}`),
        })
      }
      expect(Object.keys(state.pendingProjectionBySession)).toHaveLength(pendingProjectionSessionCap)
      expect(state.pendingProjectionBySession['background-0']).toBeUndefined()

      for (let i = 0; i < pendingProjectionEventCap + 8; i++) {
        state = sessionStoreReducer(state, {
          type: 'projectionEvent',
          event: projectionEvent('item.created', `many-${i}`, i + 1, i + 1000, String(i + 1000), `many-${i}`, 'many'),
        })
      }
      expect(state.pendingProjectionBySession.many.events).toHaveLength(pendingProjectionEventCap)
      expect(state.pendingProjectionBySession.many.events.at(-1)?.item.id).toBe(`many-${pendingProjectionEventCap + 7}`)
      expect(state.pendingProjectionBySession.many.overflowed).toBe(true)
    })

    it('keeps the queue bounded and records resync when an in-flight slot is evicted', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), { type: 'snapshotStarted', sessionID: 'protected' })
      state = sessionStoreReducer(state, {
        type: 'projectionEvent',
        event: projectionEvent('item.created', 'protected-item', 1, 1, '1', 'protected', 'protected'),
      })
      for (let i = 0; i < pendingProjectionSessionCap - 1; i++) {
        state = sessionStoreReducer(state, {
          type: 'projectionEvent',
          event: projectionEvent('item.created', `background-item-${i}`, i + 1, i + 2, String(i + 2), 'background', `background-${i}`),
        })
      }
      expect(state.pendingProjectionBySession.protected.events[0].item.id).toBe('protected-item')
      expect(Object.keys(state.pendingProjectionBySession)).toHaveLength(pendingProjectionSessionCap)

      state = sessionStoreReducer(state, {
        type: 'projectionEvent',
        event: projectionEvent('item.created', 'one-more', 100, 100, '100', 'one-more', 'one-more-session'),
      })
      expect(Object.keys(state.pendingProjectionBySession)).toHaveLength(pendingProjectionSessionCap)
      expect(state.snapshotResyncBySession.protected).toBe('1')
    })

    it('counts concurrent snapshot requests so one finish cannot unprotect early', () => {
      let state = initialSessionStoreState()
      state = sessionStoreReducer(state, { type: 'snapshotStarted', sessionID: 's1' })
      state = sessionStoreReducer(state, { type: 'snapshotStarted', sessionID: 's1' })
      expect(state.snapshotInFlightBySession.s1).toBe(2)
      state = sessionStoreReducer(state, { type: 'snapshotFinished', sessionID: 's1' })
      expect(state.snapshotInFlightBySession.s1).toBe(1)
      state = sessionStoreReducer(state, { type: 'snapshotFinished', sessionID: 's1' })
      expect(state.snapshotInFlightBySession.s1).toBeUndefined()
    })
  })

  describe('fallback revision', () => {
    it('revision=String(last_seq) is not discarded as stale', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2]),
        expectedSessionID: 's1',
      })
      // Simulate fallback: same revision, should be discarded (same revision)
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '5', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession['s1'].revision).toBe('5')
      // Fallback with newer revision should be accepted
      state = sessionStoreReducer(state, {
        type: 'snapshot',
        snapshot: snapshot('s1', '8', [1, 2, 3]),
        expectedSessionID: 's1',
      })
      expect(state.historyBySession['s1'].revision).toBe('8')
    })
  })
})
