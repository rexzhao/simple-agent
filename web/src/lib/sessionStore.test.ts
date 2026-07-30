import { describe, expect, it } from 'vitest'
import type { ItemsPage, Session, SessionSnapshot } from '../types'
import { initialSessionStoreState, mergeRefreshedPage, revisionGTE, sessionStoreReducer } from './sessionStore'

const session = (id: string, lastSeq = 0): Session => ({ id, project_id: 'p', display_name: id, last_seq: lastSeq } as Session)
const page = (seqs: number[]): ItemsPage => ({
  items: seqs.map((seq) => ({ id: `item-${seq}`, seq } as never)),
  oldest_seq: seqs[0] ?? 0,
  newest_seq: seqs[seqs.length - 1] ?? 0,
  has_more_before: seqs[0] > 1,
  has_more_after: false,
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

    it('discards history from a snapshot with older revision but updates metadata', () => {
      let state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'snapshot',
        snapshot: snapshot('s1', '10', [1, 2, 3]),
        expectedSessionID: 's1',
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
      // History unchanged (revision not newer).
      expect(state.historyBySession['s1'].revision).toBe('10')
      expect(state.historyBySession['s1'].page.items).toHaveLength(3)
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
      })
      expect(state.historyBySession['s1'].page.items.map((i) => i.seq)).toEqual([2, 3, 4, 5, 6, 7])
      expect(state.historyBySession['s1'].page.oldest_seq).toBe(2)
    })

    it('ignores pageOlder for unknown session', () => {
      const state = sessionStoreReducer(initialSessionStoreState(), {
        type: 'pageOlder',
        sessionID: 'unknown',
        older: page([1, 2]),
      })
      expect(state.historyBySession).toEqual({})
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
