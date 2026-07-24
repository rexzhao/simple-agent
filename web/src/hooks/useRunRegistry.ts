import { useCallback, useEffect, useRef, useState } from 'react'
import type { ActiveRun, RunEvent } from '../types'
import { reduceRunEvent } from '../lib/runEventReducer'

export const streamPublishIntervalMS = 40

type Pending = { runID: string; timer: number }

export function coalesceRunEvents(events: RunEvent[]): RunEvent[] {
  const result: RunEvent[] = []
  for (const event of events) {
    const previous = result[result.length - 1]
    if ((event.type === 'text.delta' || event.type === 'reasoning.delta') && previous?.type === event.type) {
      const current = event as Extract<RunEvent, { type: 'text.delta' | 'reasoning.delta' }>
      const prior = previous as typeof current
      if (prior.turn_id === current.turn_id && prior.agent_iteration === current.agent_iteration) {
        result[result.length - 1] = { ...current, text: prior.text + current.text }
        continue
      }
    }
    result.push(event)
  }
  return result
}

export function useRunRegistry() {
  const [activeRunsBySession, setActiveRunsBySession] = useState<Record<string, ActiveRun>>({})
  const [runningSessionIDs, setRunningSessionIDs] = useState<ReadonlySet<string>>(() => new Set())
  const activeRunsRef = useRef<Record<string, ActiveRun>>({})
  const pendingRef = useRef<Record<string, Pending>>({})

  const publish = useCallback((next: Record<string, ActiveRun>) => {
    activeRunsRef.current = next
    setActiveRunsBySession(next)
  }, [])
  const updateActiveRun = useCallback((sessionID: string, runID: string, updater: (run: ActiveRun) => ActiveRun | null) => {
    const current = activeRunsRef.current[sessionID]
    if (!current || current.id !== runID) return
    const updated = updater(current)
    const next = { ...activeRunsRef.current }
    if (updated) next[sessionID] = updated
    else {
      delete next[sessionID]
      const pending = pendingRef.current[sessionID]
      if (pending) window.clearTimeout(pending.timer)
      delete pendingRef.current[sessionID]
      setRunningSessionIDs((currentIDs) => {
        if (!currentIDs.has(sessionID)) return currentIDs
        const nextIDs = new Set(currentIDs)
        nextIDs.delete(sessionID)
        return nextIDs
      })
    }
    publish(next)
  }, [publish])
  const addActiveRun = useCallback((run: ActiveRun) => {
    const isNewSession = !activeRunsRef.current[run.sessionID]
    const pending = pendingRef.current[run.sessionID]
    if (pending && pending.runID !== run.id) {
      window.clearTimeout(pending.timer)
      delete pendingRef.current[run.sessionID]
    }
    publish({ ...activeRunsRef.current, [run.sessionID]: run })
    if (isNewSession) setRunningSessionIDs((current) => new Set(current).add(run.sessionID))
  }, [publish])

  const flushRunEvents = useCallback((sessionID: string, runID: string) => {
    const pending = pendingRef.current[sessionID]
    if (!pending || pending.runID !== runID) return
    window.clearTimeout(pending.timer)
    delete pendingRef.current[sessionID]
    // Publish a snapshot. The authoritative ref has already received every
    // delta, while React presentation is limited to this cadence.
    setActiveRunsBySession({ ...activeRunsRef.current })
  }, [])

  const queueRunEvent = useCallback((sessionID: string, runID: string, event: RunEvent) => {
    const current = activeRunsRef.current[sessionID]
    if (!current || current.id !== runID) return
    // Keep lifecycle reads authoritative without forcing a React commit.
    activeRunsRef.current = { ...activeRunsRef.current, [sessionID]: reduceRunEvent(current, event) }

    let pending: Pending | undefined = pendingRef.current[sessionID]
    if (pending && pending.runID !== runID) {
      window.clearTimeout(pending.timer)
      delete pendingRef.current[sessionID]
      pending = undefined
    }
    if (!pending) {
      const timer = window.setTimeout(() => flushRunEvents(sessionID, runID), streamPublishIntervalMS)
      pendingRef.current[sessionID] = { runID, timer }
    }
  }, [flushRunEvents])

  useEffect(() => () => {
    for (const pending of Object.values(pendingRef.current)) window.clearTimeout(pending.timer)
    pendingRef.current = {}
  }, [])

  return { activeRunsBySession, activeRunsRef, runningSessionIDs, addActiveRun, updateActiveRun, queueRunEvent, flushRunEvents }
}
