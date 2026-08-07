// @vitest-environment jsdom
import { act, render } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { SyncApplicationProvider } from '../applicationContext'
import { useSessionContentHistory } from './useSessionContentHistory'
import { createSyncApplication } from '../sync/applicationComposition'
import { LocalReplica } from '../sync/localReplica'
import { SessionContentAdapter } from '../sync/sessionContentAdapter'
import type { Sequence } from '../protocol/types'
import type { SessionContentHistoryWindow } from '../domain/sessionContent'

function snapshot(id: string) {
  return {
    schema_version: 1 as const,
    session: {
      id, version: 2, created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z',
      archived: false, last_used_at: '2025-01-01T00:00:00Z', has_unread_result: false, status: 'idle' as const,
      show_reasoning: false, full_access: false, debug: { request_bodies: false }, context: {}, save_tool_results: false,
    },
    history: {
      items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: `${id}-new` }, seq: 20, created_at: '2025-01-01T00:00:20Z', kind: 'message' as const, visibility: 'visible' as const, audience: 'user' as const }],
      descriptor: { limit: 20, oldest_item_seq: '20', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: true, has_more_after: false },
    },
    active_run: null,
    compaction: { checkpoints: [], truncated: false },
  }
}

function seed(application: ReturnType<typeof createSyncApplication>, id: string) {
  application.replica.applySnapshot(
    { type: 'session_content', id },
    new SessionContentAdapter(id),
    snapshot(id),
    { streamEpoch: 'epoch_1', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 },
  )
}

function page(id: string): SessionContentHistoryWindow {
  return {
    items: [{ key: { turn_id: 'turn', agent_iteration: 1, item_id: `${id}-old` }, seq: 10, created_at: '2025-01-01T00:00:10Z', kind: 'message', visibility: 'visible', audience: 'user' }],
    descriptor: { limit: 20, before_item_seq: '20', oldest_item_seq: '10', newest_item_seq: '20', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false },
  }
}

describe('useSessionContentHistory interest lifecycle', () => {
  const applications: ReturnType<typeof createSyncApplication>[] = []
  afterEach(() => applications.splice(0).forEach((application) => application.dispose()))

  it('passes AbortSignal through and ignores A after A→B→A selection interleaving', async () => {
    const reads: Array<{ sessionID: string; signal?: AbortSignal; resolve: (value: SessionContentHistoryWindow) => void }> = []
    const application = createSyncApplication({
      historyReader: (sessionID, _options, signal) => new Promise((resolve) => reads.push({ sessionID, signal, resolve })),
    })
    applications.push(application)
    seed(application, 'session_a')
    seed(application, 'session_b')

    let loadOlder: (() => Promise<boolean>) | undefined
    function Probe({ sessionID }: { sessionID: string }) {
      const result = useSessionContentHistory(sessionID, application.repositories.sessionContent)
      loadOlder = result.loadOlder
      return <output data-testid="history-state">{`${result.historyState.version}:${result.historyState.loading}`}</output>
    }
    const rendered = render(<SyncApplicationProvider application={application} startOnMount={false}><Probe sessionID="session_a" /></SyncApplicationProvider>)
    let oldRead: Promise<boolean> | undefined
    await act(async () => { oldRead = loadOlder?.() })
    expect(reads).toHaveLength(1)
    expect(reads[0].sessionID).toBe('session_a')
    expect(reads[0].signal?.aborted).toBe(false)

    await act(async () => { rendered.rerender(<SyncApplicationProvider application={application} startOnMount={false}><Probe sessionID="session_b" /></SyncApplicationProvider>) })
    expect(reads[0].signal?.aborted).toBe(true)
    // A reader that ignores AbortSignal must still be rejected at the merge
    // barrier. Its old page cannot be retained in the next A interest.
    await act(async () => {
      reads[0].resolve(page('late-a'))
      await oldRead
    })
    expect(application.repositories.sessionContent.get('session_a').history.items.map((item) => item.key.item_id)).toEqual(['session_a-new'])

    await act(async () => { rendered.rerender(<SyncApplicationProvider application={application} startOnMount={false}><Probe sessionID="session_a" /></SyncApplicationProvider>) })
    let newRead: Promise<boolean> | undefined
    await act(async () => { newRead = loadOlder?.() })
    expect(reads).toHaveLength(2)
    await act(async () => { reads[1].resolve(page('current-a')); await newRead })
    expect(application.repositories.sessionContent.get('session_a').history.items.map((item) => item.key.item_id)).toEqual(['current-a-old', 'session_a-new'])
    expect(application.repositories.sessionContent.get('session_b').history.items.map((item) => item.key.item_id)).toEqual(['session_b-new'])
    rendered.unmount()
  })

  it('aborts an outstanding page read on unmount', async () => {
    let signal: AbortSignal | undefined
    const application = createSyncApplication({
      historyReader: (_sessionID, _options, suppliedSignal) => {
        signal = suppliedSignal
        return new Promise<SessionContentHistoryWindow>(() => {})
      },
    })
    applications.push(application)
    seed(application, 'session_a')
    let loadOlder: (() => Promise<boolean>) | undefined
    function Probe() {
      const result = useSessionContentHistory('session_a', application.repositories.sessionContent)
      loadOlder = result.loadOlder
      return null
    }
    const rendered = render(<SyncApplicationProvider application={application} startOnMount={false}><Probe /></SyncApplicationProvider>)
    await act(async () => { void loadOlder?.() })
    expect(signal?.aborted).toBe(false)
    rendered.unmount()
    expect(signal?.aborted).toBe(true)
  })

  it('publishes loading, failure, retry, and success through the React snapshot', async () => {
    const reads: Array<{ resolve: (value: SessionContentHistoryWindow) => void; reject: (reason: unknown) => void }> = []
    const application = createSyncApplication({
      historyReader: () => new Promise((resolve, reject) => reads.push({ resolve, reject })),
    })
    applications.push(application)
    seed(application, 'session_a')

    let loadOlder: (() => Promise<boolean>) | undefined
    function Probe() {
      const result = useSessionContentHistory('session_a', application.repositories.sessionContent)
      loadOlder = result.loadOlder
      const error = result.historyState.error ? ':error' : ''
      return <output data-testid="history-state">{`${result.historyState.version}:${result.historyState.loading}${error}`}</output>
    }
    const rendered = render(<SyncApplicationProvider application={application} startOnMount={false}><Probe /></SyncApplicationProvider>)
    expect(rendered.getByTestId('history-state').textContent).toBe('0:false')

    let firstRead: Promise<boolean> | undefined
    await act(async () => { firstRead = loadOlder?.() })
    expect(rendered.getByTestId('history-state').textContent).toBe('1:true')
    await act(async () => {
      reads[0].reject(new Error('server detail must stay private'))
      await firstRead
    })
    expect(rendered.getByTestId('history-state').textContent).toBe('2:false:error')

    let retryRead: Promise<boolean> | undefined
    await act(async () => { retryRead = loadOlder?.() })
    expect(rendered.getByTestId('history-state').textContent).toBe('3:true')
    await act(async () => { reads[1].resolve(page('retried-a')); await retryRead })
    expect(rendered.getByTestId('history-state').textContent).toBe('4:false')
    expect(application.repositories.sessionContent.get('session_a').history.items.map((item) => item.key.item_id)).toEqual(['retried-a-old', 'session_a-new'])
    rendered.unmount()
  })
})
