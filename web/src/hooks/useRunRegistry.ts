import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ActiveRun, ActiveRunDescriptor, RunEvent } from '../types'
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
      const sameAssistantCheckpoint = current.type !== 'text.delta' ||
        (prior.type === 'text.delta' &&
          prior.item_id === current.item_id &&
          prior.durable_text_length === current.durable_text_length &&
          prior.durable_checkpointed === current.durable_checkpointed)
      const sameReasoningItem = current.type !== 'reasoning.delta' ||
        (prior.type === 'reasoning.delta' && Boolean(prior.item_id?.trim()) && Boolean(current.item_id?.trim()) && prior.item_id?.trim() === current.item_id?.trim())
      if (prior.turn_id === current.turn_id && prior.agent_iteration === current.agent_iteration && sameAssistantCheckpoint && sameReasoningItem) {
        result[result.length - 1] = { ...current, text: prior.text + current.text }
        continue
      }
    }
    result.push(event)
  }
  return result
}

export interface UseRunRegistryOptions {
  /** Called when a new run is about to supersede a non-running old run in the same session. */
  onSupersedeRun?: (sessionID: string, oldRunID: string) => void
}

export function useRunRegistry(options?: UseRunRegistryOptions) {
  const [activeRunsBySession, setActiveRunsBySession] = useState<Record<string, ActiveRun>>({})
  const activeRunsRef = useRef<Record<string, ActiveRun>>({})
  const pendingRef = useRef<Record<string, Pending>>({})
  const onSupersedeRunRef = useRef(options?.onSupersedeRun)
  onSupersedeRunRef.current = options?.onSupersedeRun

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
    }
    publish(next)
  }, [publish])

  const addActiveRun = useCallback((run: ActiveRun): boolean => {
    const existing = activeRunsRef.current[run.sessionID]
    if (existing && existing.id !== run.id) {
      if (existing.status === 'running') {
        // Not allowed to supersede a running run (coordinator is single-run per session).
        return false
      }
      // Old run is reconciling/error_pending_refresh/failed/cancelled:
      // save steps + trigger refresh via App-layer callback, then overwrite.
      onSupersedeRunRef.current?.(run.sessionID, existing.id)
    }
    const pending = pendingRef.current[run.sessionID]
    if (pending && pending.runID !== run.id) {
      window.clearTimeout(pending.timer)
      delete pendingRef.current[run.sessionID]
    }
    publish({ ...activeRunsRef.current, [run.sessionID]: run })
    return true
  }, [publish])

  const syncActiveRuns = useCallback((descriptors: ActiveRunDescriptor[]) => {
    const next: Record<string, ActiveRun> = {}
    for (const descriptor of descriptors) {
      const existing = activeRunsRef.current[descriptor.session_id]
      next[descriptor.session_id] = existing && existing.id === descriptor.run_id
        ? { ...existing, turnID: descriptor.turn_id ?? existing.turnID }
        : {
            id: descriptor.run_id,
            sessionID: descriptor.session_id,
            turnID: descriptor.turn_id,
            assistantText: '',
            steps: [],
            agentIteration: 0,
            status: 'running',
          }
      const pending = pendingRef.current[descriptor.session_id]
      if (pending && pending.runID !== descriptor.run_id) {
        window.clearTimeout(pending.timer)
        delete pendingRef.current[descriptor.session_id]
      }
    }
    for (const [sessionID, pending] of Object.entries(pendingRef.current)) {
      if (!next[sessionID]) window.clearTimeout(pending.timer)
    }
    pendingRef.current = Object.fromEntries(Object.entries(pendingRef.current).filter(([sessionID]) => Boolean(next[sessionID])))
    publish(next)
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

  // Derive runningSessionIDs from activeRunsBySession: only sessions with
  // status === 'running' are considered active. This replaces the old manual
  // set management which treated any run presence as "running".
  const runningSessionIDs = useMemo(() =>
    new Set(Object.entries(activeRunsBySession)
      .filter(([, run]) => run.status === 'running')
      .map(([sessionID]) => sessionID))
  , [activeRunsBySession])

  useEffect(() => () => {
    for (const pending of Object.values(pendingRef.current)) window.clearTimeout(pending.timer)
    pendingRef.current = {}
  }, [])

  return { activeRunsBySession, activeRunsRef, runningSessionIDs, addActiveRun, syncActiveRuns, updateActiveRun, queueRunEvent, flushRunEvents }
}
