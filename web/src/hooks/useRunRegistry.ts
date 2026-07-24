import { useCallback, useEffect, useRef, useState } from 'react'
import type { ActiveRun, RunEvent } from '../types'
import { reduceRunEvent } from '../lib/runEventReducer'

const fallbackDelayMS = 40

type Pending = { events: RunEvent[]; frame?: number; timer?: number }

export function useRunRegistry() {
  const [activeRunsBySession, setActiveRunsBySession] = useState<Record<string, ActiveRun>>({})
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
    else delete next[sessionID]
    publish(next)
  }, [publish])
  const addActiveRun = useCallback((run: ActiveRun) => publish({ ...activeRunsRef.current, [run.sessionID]: run }), [publish])

  const flushRunEvents = useCallback((sessionID: string, runID: string) => {
    const pending = pendingRef.current[sessionID]
    if (!pending) return
    if (pending.frame !== undefined) cancelAnimationFrame(pending.frame)
    if (pending.timer !== undefined) window.clearTimeout(pending.timer)
    delete pendingRef.current[sessionID]
    if (pending.events.length) updateActiveRun(sessionID, runID, (run) => pending.events.reduce(reduceRunEvent, run))
  }, [updateActiveRun])

  const queueRunEvent = useCallback((sessionID: string, runID: string, event: RunEvent) => {
    let pending = pendingRef.current[sessionID]
    if (!pending) pending = pendingRef.current[sessionID] = { events: [] }
    pending.events.push(event)
    if (pending.frame !== undefined) return
    const flush = () => flushRunEvents(sessionID, runID)
    pending.frame = requestAnimationFrame(flush)
    // Browsers throttle rAF in background tabs. The timeout keeps streams moving.
    pending.timer = window.setTimeout(flush, fallbackDelayMS)
  }, [flushRunEvents])

  useEffect(() => () => {
    for (const pending of Object.values(pendingRef.current)) {
      if (pending.frame !== undefined) cancelAnimationFrame(pending.frame)
      if (pending.timer !== undefined) window.clearTimeout(pending.timer)
    }
    pendingRef.current = {}
  }, [])

  return { activeRunsBySession, activeRunsRef, addActiveRun, updateActiveRun, queueRunEvent, flushRunEvents }
}
