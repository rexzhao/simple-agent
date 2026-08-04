import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, streamLifecycle, streamRun } from './api'
import type { ActiveRun, ActiveRunDescriptor, Bootstrap, ImageAttachmentInput, ItemsPage, LifecycleEvent, Project, RunEvent, RunStep, Session, SessionDebugSettings, SessionItem, SessionItemProjectionEvent, SessionModelOption } from './types'
import { errorMessage } from './lib/format'
import { reduceLifecycleEvent, type SessionMaps } from './lib/lifecycleReducer'
import { reduceRunEvent } from './lib/runEventReducer'
import { modelKey, orderSessions, processKey, projectName, sessionDescendantIDs, sessionName } from './lib/session'
import { emptyComposerDraft } from './components/Composer'
import type { PastedImageAttachment } from './components/Composer'
import { Conversation } from './components/Conversation'
import { DebugSettingsDialog } from './components/DebugSettingsDialog'
import { EmptySession, ErrorBanner, ProjectSetup, Splash } from './components/misc'
import { ProviderManagerDialog } from './components/ProviderManagerDialog'
import type { ProviderManagerState } from './components/ProviderManagerDialog'
import { SessionModelDialog } from './components/SessionModelDialog'
import type { SessionCreatorState } from './components/SessionModelDialog'
import { WorkspaceTree } from './components/WorkspaceTree'
import { useComposerDrafts } from './hooks/useComposerDrafts'
import { useRunRegistry } from './hooks/useRunRegistry'
import { useSessionHistory } from './hooks/useSessionHistory'
import { useSessionStore } from './hooks/useSessionStore'
import { useSessionSelection } from './hooks/useSessionSelection'

type BackgroundCompletionNotice = {
  sessionID: string
  sessionName: string
}

function App() {
  const [bootstrap, setBootstrap] = useState<Bootstrap | null>(null)
  const [projects, setProjects] = useState<Project[]>([])
  const [sessionsByProject, setSessionsByProject] = useState<Record<string, Session[]>>({})
  const [archivedSessionsByProject, setArchivedSessionsByProject] = useState<Record<string, Session[]>>({})
  const { selectedProjectID, selectedSessionID, selectedProjectRef, setSelectedProjectID, setSelectedSessionID } = useSessionSelection()
  const selectedSessionIDRef = useRef(selectedSessionID)
  selectedSessionIDRef.current = selectedSessionID
  const sessionMapsRef = useRef<SessionMaps>({ active: sessionsByProject, archived: archivedSessionsByProject })
  sessionMapsRef.current = { active: sessionsByProject, archived: archivedSessionsByProject }
  const [recoveredRuns, setRecoveredRuns] = useState<ActiveRunDescriptor[]>([])
	const [recentStepsByTurn, setRecentStepsByTurn] = useState<Record<string, RunStep[]>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showProjectForm, setShowProjectForm] = useState(false)
  const [sessionCreator, setSessionCreator] = useState<SessionCreatorState | null>(null)
  const [providerManager, setProviderManager] = useState<ProviderManagerState | null>(null)
  const [debugSessionID, setDebugSessionID] = useState('')
  const [savingDebugSettings, setSavingDebugSettings] = useState(false)
  const [creatingSession, setCreatingSession] = useState(false)
  const [completionNotice, setCompletionNotice] = useState<BackgroundCompletionNotice | null>(null)
  const [turnErrors, setTurnErrors] = useState<Record<string, { turnID: string; message: string }>>({})
  const [compactingSessionIDs, setCompactingSessionIDs] = useState<Record<string, boolean>>({})
  const { draftsBySession, updateDraft, addPastedText, removePastedText, addPastedImage, removePastedImage, clearDraft } = useComposerDrafts()
  const sessionStore = useSessionStore()

  // Session id → display name across every known project, so tool rows can
  // label session_* calls with human-readable targets instead of raw ids.
  const sessionNames = useMemo(() => {
    const names: Record<string, string> = {}
    for (const sessions of Object.values(archivedSessionsByProject)) {
      for (const session of sessions) if (session.display_name) names[session.id] = session.display_name
    }
    for (const sessions of Object.values(sessionsByProject)) {
      for (const session of sessions) if (session.display_name) names[session.id] = session.display_name
    }
    return names
  }, [sessionsByProject, archivedSessionsByProject])
  // Refs for reconciling timeout and backoff retry tracking.
  const reconcileTimeoutRef = useRef<Record<string, number>>({})
  const reconcileRetryCountRef = useRef<Record<string, number>>({})
  const reconcileRetryTimerRef = useRef<Record<string, number>>({})
  const refreshSessionRef = useRef<(sessionID: string) => Promise<Session | null>>(async () => null)
  const settledRunIDsRef = useRef(new Set<string>())

  const onSupersedeRunRef = useRef<(sessionID: string, oldRunID: string) => void>(() => {})

  const { activeRunsBySession, activeRunsRef, runningSessionIDs, addActiveRun, syncActiveRuns, updateActiveRun, queueRunEvent, flushRunEvents } = useRunRegistry({ onSupersedeRun: (sessionID, oldRunID) => onSupersedeRunRef.current(sessionID, oldRunID) })

  const setSessionMaps = useCallback((maps: SessionMaps) => {
    sessionMapsRef.current = maps
    setSessionsByProject(maps.active)
    setArchivedSessionsByProject(maps.archived)
  }, [])

  const clearTurnError = useCallback((sessionID: string) => {
    setTurnErrors((current) => {
      if (!current[sessionID]) return current
      const next = { ...current }
      delete next[sessionID]
      return next
    })
  }, [])

  const loadProjects = useCallback(async () => {
    const payload = await api.projects()
    setProjects(payload.projects)
    const maps = sessionMapsRef.current
    setSessionMaps({
      active: Object.fromEntries(payload.projects.map((project) => [project.id, maps.active[project.id] ?? []])),
      archived: Object.fromEntries(payload.projects.map((project) => [project.id, maps.archived[project.id] ?? []])),
    })
    setSelectedProjectID((current) => {
      if (current && payload.projects.some((project) => project.id === current)) return current
      return payload.projects[0]?.id ?? ''
    })
    return payload.projects
  }, [setSessionMaps])

  const bootstrapInFlightRef = useRef<Promise<ActiveRunDescriptor[]> | null>(null)
  const bootstrapApplication = useCallback(async (preserveSelection: boolean): Promise<ActiveRunDescriptor[]> => {
    if (bootstrapInFlightRef.current) return bootstrapInFlightRef.current
    const operation = (async () => {
      const [bootstrapPayload, projectsPayload, activeRunsPayload] = await Promise.all([api.bootstrap(), api.projects(), api.activeRuns()])
      const sessionEntries = await Promise.all(projectsPayload.projects.map(async (project) => {
        const [activePayload, archivedPayload] = await Promise.all([api.sessions(project.id), api.sessions(project.id, true)])
        return [project.id, orderSessions(activePayload.sessions), orderSessions(archivedPayload.sessions)] as const
      }))
      const maps: SessionMaps = {
        active: Object.fromEntries(sessionEntries.map(([projectID, sessions]) => [projectID, sessions])),
        archived: Object.fromEntries(sessionEntries.map(([projectID, , sessions]) => [projectID, sessions])),
      }
      const recovered = activeRunsPayload.runs.filter((run) => projectsPayload.projects.some((project) => maps.active[project.id]?.some((session) => session.id === run.session_id)))
      const recoveredProject = recovered.length > 0
        ? projectsPayload.projects.find((project) => maps.active[project.id]?.some((session) => session.id === recovered[0].session_id))
        : null
      const currentProjectID = selectedProjectRef.current
      const currentSessionID = selectedSessionIDRef.current
      const currentProject = projectsPayload.projects.find((project) => project.id === currentProjectID)
      const currentSessionProject = projectsPayload.projects.find((project) =>
        maps.active[project.id]?.some((session) => session.id === currentSessionID) ||
        maps.archived[project.id]?.some((session) => session.id === currentSessionID),
      )
      const firstProjectID = preserveSelection && currentProject
        ? currentProject.id
        : recoveredProject?.id ?? projectsPayload.projects[0]?.id ?? ''
      const firstSessionID = preserveSelection && currentSessionProject && currentSessionID
        ? currentSessionID
        : recovered[0]?.session_id ?? maps.active[firstProjectID]?.[0]?.id ?? ''
      setBootstrap(bootstrapPayload)
      setProjects(projectsPayload.projects)
      setSessionMaps(maps)
      syncActiveRuns(recovered)
      setSelectedProjectID(firstProjectID)
      setSelectedSessionID(firstSessionID)
      setRecoveredRuns(recovered)
      if (!preserveSelection || projectsPayload.projects.length === 0) setShowProjectForm(projectsPayload.projects.length === 0)
      return recovered
    })()
    bootstrapInFlightRef.current = operation
    try {
      return await operation
    } finally {
      if (bootstrapInFlightRef.current === operation) bootstrapInFlightRef.current = null
    }
  }, [setSessionMaps, syncActiveRuns])

  useEffect(() => {
    let cancelled = false
    void bootstrapApplication(false)
      .catch((reason: unknown) => setError(errorMessage(reason)))
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [bootstrapApplication])

  const loadSessions = useCallback(async (projectID: string, preferredSessionID = '', preserveSelection = false) => {
    if (!projectID) {
      if (!preserveSelection) setSelectedSessionID('')
      return []
    }
    const [payload, archivedPayload] = await Promise.all([api.sessions(projectID), api.sessions(projectID, true)])
    const ordered = orderSessions(payload.sessions)
    const maps = sessionMapsRef.current
    setSessionMaps({
      active: { ...maps.active, [projectID]: ordered },
      archived: { ...maps.archived, [projectID]: orderSessions(archivedPayload.sessions) },
    })
    if (!preserveSelection && selectedProjectRef.current === projectID) {
      setSelectedSessionID((current) => {
        const preferred = preferredSessionID || current
        if (preferred && ordered.some((session) => session.id === preferred)) return preferred
        return ordered[0]?.id ?? ''
      })
    }
    return ordered
  }, [setSessionMaps])

  const reportError = useCallback((reason: unknown) => setError(errorMessage(reason)), [])
  const { sessionDetail, itemsPage, selectedSessionRef, refreshSession, loadOlder } =
    useSessionHistory(selectedSessionID, loadSessions, reportError, sessionStore)
  refreshSessionRef.current = refreshSession

  // saveRecentStepsAndRemove: saves steps to recentStepsByTurn, clears timers, removes run.
  const saveRecentStepsAndRemove = useCallback((sessionID: string, runID: string) => {
    const run = activeRunsRef.current[sessionID]
    if (!run || run.id !== runID) return
    const turnID = String(run.turnID ?? '')
    if (turnID && run.steps.length > 0) {
      setRecentStepsByTurn((current) => ({ ...current, [processKey(sessionID, turnID)]: run.steps }))
    }
    setRecoveredRuns((current) => current.filter((r) => r.run_id !== runID))
    // Clear reconciling timers for this session.
    const timeout = reconcileTimeoutRef.current[sessionID]
    if (timeout) { window.clearTimeout(timeout); delete reconcileTimeoutRef.current[sessionID] }
    const retryTimer = reconcileRetryTimerRef.current[sessionID]
    if (retryTimer) { window.clearTimeout(retryTimer); delete reconcileRetryTimerRef.current[sessionID] }
    delete reconcileRetryCountRef.current[`${sessionID}:${runID}`]
    updateActiveRun(sessionID, runID, () => null)
  }, [updateActiveRun])

  // Wire onSupersedeRun: save old run steps + best-effort refresh.
  onSupersedeRunRef.current = useCallback((sessionID: string, oldRunID: string) => {
    saveRecentStepsAndRemove(sessionID, oldRunID)
    void refreshSessionRef.current(sessionID).catch(() => {})
  }, [saveRecentStepsAndRemove])

  // startReconcileTimeout: 60s timer → error_pending_refresh.
  const startReconcileTimeout = useCallback((sessionID: string) => {
    const existing = reconcileTimeoutRef.current[sessionID]
    if (existing) window.clearTimeout(existing)
    reconcileTimeoutRef.current[sessionID] = window.setTimeout(() => {
      delete reconcileTimeoutRef.current[sessionID]
      const run = activeRunsRef.current[sessionID]
      if (run && run.status === 'reconciling') {
        updateActiveRun(sessionID, run.id, (r) => ({ ...r, status: 'error_pending_refresh' }))
      }
    }, 60000)
  }, [updateActiveRun])

  // onSnapshotApplied: unified settlement check point.
  const onSnapshotApplied = useCallback((sessionID: string, session: Session) => {
    const run = activeRunsRef.current[sessionID]
    if (!run) return
    if (run.status !== 'reconciling' && run.status !== 'error_pending_refresh') return
    if (run.settledLastSeq == null || run.settledLastSeq === 0) {
      saveRecentStepsAndRemove(sessionID, run.id)
      return
    }
    if (session.last_seq >= run.settledLastSeq) {
      saveRecentStepsAndRemove(sessionID, run.id)
    } else {
      if (run.status === 'error_pending_refresh') {
        updateActiveRun(sessionID, run.id, (r) => ({ ...r, status: 'reconciling' }))
        // Re-entering reconciling needs a fresh terminal deadline: the
        // original 60s timeout already fired when the run entered
        // error_pending_refresh, and without one the run could strand here
        // invisibly once the bounded retries are exhausted.
        startReconcileTimeout(sessionID)
      }
      scheduleReconcileRetry(sessionID, run.id, run.settledLastSeq)
    }
  }, [saveRecentStepsAndRemove, startReconcileTimeout, updateActiveRun])

  // scheduleReconcileRetry: backoff refresh, max 2 retries.
  const scheduleReconcileRetry = useCallback((sessionID: string, runID: string, settledLastSeq: number) => {
    const key = `${sessionID}:${runID}`
    const count = reconcileRetryCountRef.current[key] ?? 0
    if (count >= 2) return
    reconcileRetryCountRef.current[key] = count + 1
    const existing = reconcileRetryTimerRef.current[sessionID]
    if (existing) window.clearTimeout(existing)
    reconcileRetryTimerRef.current[sessionID] = window.setTimeout(() => {
      delete reconcileRetryTimerRef.current[sessionID]
      void refreshSessionRef.current(sessionID)
        .then((session) => { if (session) onSnapshotApplied(sessionID, session) })
        .catch(() => {})
    }, 2000)
  }, [onSnapshotApplied])

  // retryRefreshSession: manual "refresh to see latest" handler.
  const retryRefreshSession = useCallback(async (sessionID: string) => {
    const run = activeRunsRef.current[sessionID]
    if (!run || run.status !== 'error_pending_refresh') return
    try {
      const session = await refreshSessionRef.current(sessionID)
      if (session) onSnapshotApplied(sessionID, session)
    } catch { /* stay in error_pending_refresh */ }
  }, [onSnapshotApplied])

  // Auto-resolve pending reconciliation whenever fresh session detail
  // arrives: navigating to a session with a stuck "Refresh needed" banner
  // settles it without a manual click once the durable state has caught up.
  useEffect(() => {
    if (sessionDetail) onSnapshotApplied(sessionDetail.id, sessionDetail)
  }, [sessionDetail, onSnapshotApplied])

  useEffect(() => {
    if (!completionNotice) return
    const timer = window.setTimeout(() => setCompletionNotice(null), 5000)
    return () => window.clearTimeout(timer)
  }, [completionNotice])

  const createProject = async (root: string, displayName: string) => {
    try {
      const result = await api.createProject(root, displayName)
      await loadProjects()
      setSelectedProjectID(result.project.id)
      setSelectedSessionID('')
      await loadSessions(result.project.id)
      setShowProjectForm(false)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const openSessionCreator = useCallback(async (projectID = selectedProjectID) => {
    if (!projectID) return
    setSessionCreator({ projectID, models: [], selectedKey: '', defaultProvider: '', defaultModel: '', reasoningLevel: '', fullAccess: false, loading: true })
    try {
      const options = await api.sessionModels(projectID)
      const defaultModel = options.models.find((model) => model.provider === options.default_provider && model.model_profile === options.default_model)
      setSessionCreator((current) => current?.projectID === projectID ? {
        projectID,
        models: options.models,
        selectedKey: modelKey(defaultModel ?? options.models[0]),
        defaultProvider: options.default_provider,
        defaultModel: options.default_model,
        reasoningLevel: defaultModel?.default_reasoning_level ?? options.models[0]?.default_reasoning_level ?? '',
        fullAccess: current.fullAccess,
        loading: false,
      } : current)
    } catch (reason) {
      setSessionCreator(null)
      setError(errorMessage(reason))
    }
  }, [selectedProjectID])

  const createSession = async (projectID: string, model: SessionModelOption) => {
    if (!projectID || creatingSession) return
    setCreatingSession(true)
    try {
      const session = await api.createSession(projectID, model.provider, model.model_profile, sessionCreator?.reasoningLevel ?? model.default_reasoning_level ?? '', sessionCreator?.fullAccess ?? false)
      setSelectedProjectID(projectID)
      await loadSessions(projectID, session.id)
      setSelectedSessionID(session.id)
      setShowProjectForm(false)
      setSessionCreator(null)
    } catch (reason) {
      setError(errorMessage(reason))
    } finally {
      setCreatingSession(false)
    }
  }

  const openProviderManager = useCallback(async () => {
    setProviderManager({ document: null, loading: true })
    try {
      const document = await api.providerSettings()
      setProviderManager((current) => current ? { document, loading: false } : current)
    } catch (reason) {
      setProviderManager(null)
      setError(errorMessage(reason))
    }
  }, [])

  const selectProject = useCallback((projectID: string) => {
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionsByProject[projectID]?.[0]?.id ?? '')
    setShowProjectForm(false)
  }, [sessionsByProject, setSelectedProjectID, setSelectedSessionID])

  const selectSession = useCallback((projectID: string, sessionID: string) => {
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionID)
    setShowProjectForm(false)
  }, [setSelectedProjectID, setSelectedSessionID])

  const renameProject = useCallback(async (project: Project) => {
    const displayName = window.prompt('Rename project', projectName(project))
    if (displayName === null || displayName.trim() === project.display_name) return
    if (!displayName.trim()) {
      setError('Project name cannot be empty')
      return
    }
    try {
      await api.renameProject(project.id, displayName.trim())
      await loadProjects()
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [loadProjects])

  const deleteProject = useCallback(async (project: Project) => {
    const activeSessions = sessionsByProject[project.id] ?? []
    const archivedSessions = archivedSessionsByProject[project.id] ?? []
    if (activeSessions.some((session) => session.status === 'running' || activeRunsRef.current[session.id]?.status === 'running')) return
    const sessionCount = activeSessions.length + archivedSessions.length
    const message = `Permanently delete "${projectName(project)}" and ${sessionCount} saved ${sessionCount === 1 ? 'session' : 'sessions'}? All session history and attachments for this project will be removed. This action cannot be undone.`
    if (!window.confirm(message)) return
    let archived = false
    try {
      await api.archiveProject(project.id)
      archived = true
      await api.deleteProject(project.id)
      const remaining = await loadProjects()
      const nextProject = remaining[0]
      setSelectedProjectID(nextProject?.id ?? '')
      setSelectedSessionID(nextProject ? sessionsByProject[nextProject.id]?.[0]?.id ?? '' : '')
      setShowProjectForm(remaining.length === 0)
    } catch (reason) {
      if (archived) {
        try {
          await api.restoreProject(project.id)
        } catch {
          // Preserve the original removal error.
        }
      }
      try {
        await loadProjects()
      } catch {
        // Preserve the original removal error.
      }
      setError(errorMessage(reason))
    }
  }, [activeRunsRef, archivedSessionsByProject, loadProjects, sessionsByProject, setSelectedProjectID, setSelectedSessionID])

  const renameSession = useCallback(async (session: Session) => {
    const displayName = window.prompt('Rename session', sessionName(session))
    if (displayName === null || displayName.trim() === session.display_name) return
    if (!displayName.trim()) {
      setError('Session name cannot be empty')
      return
    }
    try {
      await api.renameSession(session.id, displayName.trim())
      await loadSessions(session.project_id, session.id)
      if (selectedSessionRef.current === session.id) await refreshSession(session.id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [loadSessions, refreshSession, selectedSessionRef])

  const toggleFullAccess = useCallback(async (session: Session) => {
    try {
      await api.setSessionFullAccess(session.id, !session.full_access)
      await loadSessions(session.project_id, session.id)
      if (selectedSessionRef.current === session.id) await refreshSession(session.id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [loadSessions, refreshSession, selectedSessionRef])

  const openDebugSettings = useCallback(() => {
    if (selectedSessionID) setDebugSessionID(selectedSessionID)
  }, [selectedSessionID])

  const saveDebugSettings = useCallback(async (sessionID: string, settings: SessionDebugSettings) => {
    setSavingDebugSettings(true)
    try {
      const updated = await api.setSessionDebug(sessionID, settings)
      await loadSessions(updated.project_id, updated.id, true)
      if (selectedSessionRef.current === sessionID) await refreshSession(sessionID)
      setDebugSessionID('')
    } catch (reason) {
      setError(errorMessage(reason))
      throw reason
    } finally {
      setSavingDebugSettings(false)
    }
  }, [loadSessions, refreshSession, selectedSessionRef])

  const archiveSession = useCallback(async (session: Session) => {
    // The backend archives the whole subtree together, so guard and confirm
    // against every descendant, not just the target.
    const projectSessions = sessionsByProject[session.project_id] ?? []
    const subtreeIDs = [session.id, ...sessionDescendantIDs(projectSessions, session.id)]
    const busyIDs = new Set(projectSessions.filter((item) => item.status === 'running').map((item) => item.id))
    if (subtreeIDs.some((id) => busyIDs.has(id) || activeRunsRef.current[id]?.status === 'running')) return
    const childCount = subtreeIDs.length - 1
    const childNote = childCount > 0 ? ` ${childCount} child ${childCount === 1 ? 'session' : 'sessions'} will also be archived.` : ''
    if (!window.confirm(`Archive "${sessionName(session)}"? It will be hidden from the current list.${childNote}`)) return
    try {
      await api.archiveSession(session.id)
      await loadSessions(session.project_id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [activeRunsRef, loadSessions, sessionsByProject])

  const restoreSession = useCallback(async (session: Session) => {
    try {
      const restored = await api.restoreSession(session.id)
      await loadSessions(session.project_id)
      setSelectedProjectID(session.project_id)
      setSelectedSessionID(restored.id)
      setShowProjectForm(false)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [loadSessions, setSelectedProjectID, setSelectedSessionID])

  const deleteSession = useCallback(async (session: Session) => {
    // The backend removes the whole subtree together: count descendants in
    // both the active and archived lists, since every one of them is deleted.
    const projectSessions = [...(sessionsByProject[session.project_id] ?? []), ...(archivedSessionsByProject[session.project_id] ?? [])]
    const subtreeIDs = [session.id, ...sessionDescendantIDs(projectSessions, session.id)]
    const busyIDs = new Set(projectSessions.filter((item) => item.status === 'running').map((item) => item.id))
    if (subtreeIDs.some((id) => busyIDs.has(id) || activeRunsRef.current[id]?.status === 'running')) return
    const childCount = subtreeIDs.length - 1
    const childNote = childCount > 0 ? ` ${childCount} child ${childCount === 1 ? 'session' : 'sessions'} will also be permanently deleted.` : ''
    if (!window.confirm(`Permanently delete "${sessionName(session)}"? This action cannot be undone.${childNote}`)) return
    try {
      await api.archiveSession(session.id)
      await api.deleteSession(session.id)
      await loadSessions(session.project_id)
    } catch (reason) {
      try {
        await loadSessions(session.project_id)
      } catch {
        // Preserve the original operation error.
      }
      setError(errorMessage(reason))
    }
  }, [activeRunsRef, archivedSessionsByProject, loadSessions, sessionsByProject])

  const handleRunEvent = useCallback(async (sessionID: string, runID: string, event: RunEvent) => {
    if (event.type === 'text.delta' || event.type === 'reasoning.delta') {
      queueRunEvent(sessionID, runID, event)
      return
    }
    // Preserve stream ordering and ensure tool/settled events observe all deltas.
    flushRunEvents(sessionID, runID)
    const update = (updater: (run: ActiveRun) => ActiveRun | null) => updateActiveRun(sessionID, runID, updater)
    switch (event.type) {
      case 'turn.started':
      case 'compaction.started':
      case 'compaction.completed':
      case 'provider.retrying':
      case 'run.prompt_queue':
      case 'run.prompt_appended':
      case 'agent.iteration.started':
      case 'tool.requested':
      case 'tool.started':
      case 'tool.finished':
      case 'usage.updated':
        update((run) => reduceRunEvent(run, event))
        break
      case 'item.appended':
      case 'item.created':
      case 'item.updated':
        // Projection events are already committed durable DTOs. Apply them
        // directly to the shared store; unlike run settlement they do not
        // require a full-page refresh.
        sessionStore.applyProjectionEvent(event as SessionItemProjectionEvent)
        break
      case 'run.resync_required':
        try {
          await refreshSession(sessionID)
        } catch (reason) {
          setError(errorMessage(reason))
        }
        break
      case 'turn.failed':
        update((run) => reduceRunEvent(run, event))
        setTurnErrors((current) => ({
          ...current,
          [sessionID]: { turnID: String(event.turn_id ?? ''), message: String(event.message ?? 'Run failed') },
        }))
        break
      case 'run.settled': {
        if (settledRunIDsRef.current.has(runID)) return
        settledRunIDsRef.current.add(runID)
        const settledRun = activeRunsRef.current[sessionID]
        if (!settledRun || settledRun.id !== runID) return
        const settledLastSeq = Number(event.last_seq ?? 0)
        const settledStatus = String(event.status)

        if (settledStatus === 'failed') {
          // turn.failed usually landed first with the real reason; fallback for late-attach.
          setTurnErrors((current) => current[sessionID]
            ? current
            : { ...current, [sessionID]: { turnID: String(event.turn_id ?? ''), message: String(event.message ?? 'Run failed') } })
          update((run) => ({ ...run, status: 'failed', settledLastSeq }))
          try { await refreshSession(sessionID) } catch { /* error already shown */ }
          saveRecentStepsAndRemove(sessionID, runID)
          break
        }

        if (settledStatus === 'cancelled') {
          update((run) => ({ ...run, status: 'cancelled', settledLastSeq }))
          try { await refreshSession(sessionID) } catch { /* ignore */ }
          saveRecentStepsAndRemove(sessionID, runID)
          break
        }

        // committed
        update((run) => ({ ...run, status: 'reconciling', settledLastSeq }))
        startReconcileTimeout(sessionID)
        let settledSession: Session | null = null
        try {
          settledSession = await refreshSession(sessionID)
        } catch {
          // Fall through to the bounded reconcile retry below: a transient
          // refresh failure must not strand the run on the manual-refresh
          // banner while the state is still converging.
        }
        if (activeRunsRef.current[sessionID]?.id === runID) {
          const durableLastSeq = settledSession?.last_seq ?? 0
          if (settledSession && (settledLastSeq === 0 || durableLastSeq >= settledLastSeq)) {
            saveRecentStepsAndRemove(sessionID, runID)
          } else {
            // Refresh failed or the store is genuinely behind: keep
            // reconciling with bounded retries. The 60s reconcile timeout is
            // the terminal fallback that surfaces the Refresh banner.
            scheduleReconcileRetry(sessionID, runID, settledLastSeq)
          }
        }
        if (settledStatus === 'committed' && selectedSessionRef.current !== sessionID) {
          setCompletionNotice({
            sessionID,
            sessionName: settledSession ? sessionName(settledSession) : `Session ${sessionID.slice(-6)}`,
          })
        }
        break
      }
    }
  }, [flushRunEvents, queueRunEvent, refreshSession, saveRecentStepsAndRemove, scheduleReconcileRetry, sessionStore.applyProjectionEvent, startReconcileTimeout, updateActiveRun])

  const runStreamsRef = useRef(new Set<string>())
  const connectRunStream = useCallback((runID: string, sessionID: string) => {
    if (!runID || !sessionID || runStreamsRef.current.has(runID)) return
    runStreamsRef.current.add(runID)
    void streamRun(runID, (event) => handleRunEvent(sessionID, runID, event))
      .catch((reason: unknown) => {
        updateActiveRun(sessionID, runID, (existing) => ({ ...existing, status: 'error_pending_refresh' }))
        setRecoveredRuns((current) => current.filter((item) => item.run_id !== runID))
        setError(errorMessage(reason))
      })
      .finally(() => runStreamsRef.current.delete(runID))
  }, [handleRunEvent, updateActiveRun])

  const knownSession = useCallback((sessionID: string): Session | null => {
    for (const sessions of Object.values(sessionMapsRef.current.active)) {
      const session = sessions.find((item) => item.id === sessionID)
      if (session) return session
    }
    for (const sessions of Object.values(sessionMapsRef.current.archived)) {
      const session = sessions.find((item) => item.id === sessionID)
      if (session) return session
    }
    return null
  }, [])

  const applyLifecycleSessionEvent = useCallback((event: LifecycleEvent) => {
    const current = sessionMapsRef.current
    const next = reduceLifecycleEvent(current, event)
    if (next === current) return
    setSessionMaps(next)

    if (event.type !== 'session.deleted') return
    const deletedIDs = new Set([
      typeof event.session === 'string' ? event.session : '',
      event.session_id ?? '',
      ...(event.descendants ?? []),
    ])
    if (!deletedIDs.has(selectedSessionIDRef.current)) return
    const projectID = event.project_id ?? event.project ?? selectedProjectRef.current
    const nextProjectID = next.active[projectID] || next.archived[projectID]
      ? projectID
      : Object.keys(next.active)[0] ?? ''
    setSelectedProjectID(nextProjectID)
    setSelectedSessionID(next.active[nextProjectID]?.[0]?.id ?? '')
  }, [setSelectedProjectID, setSelectedSessionID, setSessionMaps])

  const handleLifecycleEvent = useCallback(async (event: LifecycleEvent): Promise<void> => {
    const eventSession = event.session && typeof event.session === 'object'
      ? event.session
      : event.metadata ?? event.session_metadata
    if (event.type === 'session.created' || event.type === 'session.updated' || event.type === 'session.archived' || event.type === 'session.deleted' || event.type === 'run.settled') {
      applyLifecycleSessionEvent(event)
    }

    const sessionID = event.session_id ?? (typeof event.session === 'string' ? event.session : eventSession?.id) ?? ''
    const runID = event.run_id ?? event.run ?? ''
    if (event.type === 'run.started') {
      let session = eventSession ?? knownSession(sessionID)
      if (!session && sessionID) {
        try {
          session = await api.session(sessionID)
          applyLifecycleSessionEvent({ type: 'session.updated', session })
        } catch {
          // The event may race the initial project bootstrap. The next
          // reconnect will reconcile it from the authoritative lists.
        }
      }
      if (session) applyLifecycleSessionEvent({ type: 'session.updated', session: { ...session, status: 'running' } })
      if (sessionID && runID) {
        if (!activeRunsRef.current[sessionID] || activeRunsRef.current[sessionID].id !== runID) {
          addActiveRun({
            id: runID,
            sessionID,
            turnID: event.turn_id,
            restored: true,
            userText: '',
            assistantText: '',
            steps: [],
            agentIteration: 0,
            status: 'running',
          })
        }
        connectRunStream(runID, sessionID)
      }
      return
    }

    if (event.type === 'run.settled' && sessionID && runID) {
      const activeRun = activeRunsRef.current[sessionID]
      if (activeRun?.id === runID) {
        await handleRunEvent(sessionID, runID, {
          type: 'run.settled',
          run_id: runID,
          status: event.status ?? 'committed',
          turn_id: event.turn_id,
          last_seq: event.last_seq,
          message: event.message,
        })
      } else if (selectedSessionIDRef.current === sessionID) {
        try { await refreshSession(sessionID) } catch { /* the event metadata already updated the tree */ }
      }
    }
  }, [activeRunsRef, addActiveRun, applyLifecycleSessionEvent, connectRunStream, handleRunEvent, knownSession, refreshSession])

  const lifecycleEventHandlerRef = useRef<(event: LifecycleEvent) => Promise<void>>(async () => {})
  lifecycleEventHandlerRef.current = handleLifecycleEvent
  const reconcileLifecycleRef = useRef<() => Promise<void>>(async () => {})
  reconcileLifecycleRef.current = async () => {
    try {
      await bootstrapApplication(true)
    } catch (reason) {
      setError(errorMessage(reason))
      throw reason
    }
  }

  useEffect(() => {
    const controller = new AbortController()
    void streamLifecycle(
      (event) => lifecycleEventHandlerRef.current(event),
      { signal: controller.signal, onReconnect: () => reconcileLifecycleRef.current() },
    ).catch((reason: unknown) => {
      if (!controller.signal.aborted) setError(errorMessage(reason))
    })
    return () => controller.abort()
  }, [])

  useEffect(() => {
    if (recoveredRuns.length === 0) return
    for (const run of recoveredRuns) {
      if (!activeRunsRef.current[run.session_id]) {
        addActiveRun({
          id: run.run_id,
          sessionID: run.session_id,
          turnID: run.turn_id,
          restored: true,
          userText: '',
          assistantText: '',
          steps: [],
          agentIteration: 0,
          status: 'running',
        })
      }
      connectRunStream(run.run_id, run.session_id)
    }
  }, [activeRunsRef, addActiveRun, connectRunStream, recoveredRuns])

  // startNewRun handles new composer input. A successful start clears the
  // session's recorded turn failure.
  const startNewRun = async (sessionID: string, content: string, imageInputs: ImageAttachmentInput[]): Promise<boolean> => {
    try {
      const started = await api.startRun(sessionID, content, imageInputs)
      addActiveRun({
        id: started.run_id,
        sessionID,
        userText: content,
        userImages: imageInputs,
        assistantText: '',
        steps: [],
        agentIteration: 0,
        status: 'running',
      })
      clearTurnError(sessionID)
      const boundRunID = started.run_id
      connectRunStream(boundRunID, sessionID)
      return true
    } catch (reason) {
      const runID = activeRunsRef.current[sessionID]?.id
      if (runID) updateActiveRun(sessionID, runID, () => null)
      setError(errorMessage(reason))
      return false
    }
  }

  const sendMessage = async (content: string, images: PastedImageAttachment[]): Promise<boolean> => {
    if (!selectedSessionID || (!content.trim() && images.length === 0)) return false
    const sessionID = selectedSessionID
    const activeRun = activeRunsRef.current[sessionID]
    if (activeRun && activeRun.status === 'running') {
      // Append to the in-flight run: the message is queued and injected into
      // the active turn at the next safe checkpoint, or sent as a follow-up
      // turn. It is never sent as a new run here. The queued state arrives via
      // the run.prompt_queue stream event; no local echo is added.
      if (!content.trim()) return false
      try {
        await api.appendRunMessage(activeRun.id, content)
        return true
      } catch (reason) {
        setError(errorMessage(reason))
        return false
      }
    }
    const imageInputs: ImageAttachmentInput[] = images.map((image) => ({ data_url: image.dataURL, detail: 'auto' }))
    return startNewRun(sessionID, content, imageInputs)
  }

  const continueRun = useCallback(async (): Promise<boolean> => {
    if (!selectedSessionID) return false
    const sessionID = selectedSessionID
    const activeRun = activeRunsRef.current[sessionID]
    const detail = sessionDetail?.id === sessionID ? sessionDetail : undefined
    if (activeRun?.status === 'running' || !detail || (detail.status !== 'interrupted' && detail.status !== 'failed') || !detail.interrupted_run_id || !detail.interrupted_turn_id) {
      return false
    }
    try {
      const started = await api.continueRun(sessionID)
      addActiveRun({
        id: started.run_id,
        sessionID,
        userText: '',
        userImages: [],
        assistantText: '',
        steps: [],
        agentIteration: 0,
        status: 'running',
      })
      clearTurnError(sessionID)
      const boundRunID = started.run_id
      connectRunStream(boundRunID, sessionID)
      return true
    } catch (reason) {
      setError(errorMessage(reason))
      return false
    }
  }, [addActiveRun, clearTurnError, connectRunStream, selectedSessionID, sessionDetail])

  const cancelRun = async () => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.cancelRun(run.id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const cancelToolCall = useCallback(async (toolCallID: string) => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.cancelToolCall(run.id, toolCallID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [selectedSessionID])

  const removeQueuedPrompt = async (promptID: string) => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.removeRunMessage(run.id, promptID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  // Steer/move mutations only call the API; the updated queue arrives via the
  // run.prompt_queue stream event, keeping the server the single source of
  // truth for queue order.
  const setQueuedPromptSteer = async (promptID: string, steer: boolean) => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.steerRunMessage(run.id, promptID, steer)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const moveQueuedPrompt = async (promptID: string, direction: 'up' | 'down') => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.moveRunMessage(run.id, promptID, direction)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const compactSession = async () => {
    if (!selectedSessionID || sessionDetail?.status === 'running' || activeRunsRef.current[selectedSessionID]?.status === 'running') return
    const sessionID = selectedSessionID
    setCompactingSessionIDs((current) => ({ ...current, [sessionID]: true }))
    try {
      await api.compact(sessionID)
      await refreshSession(sessionID)
    } catch (reason) {
      setError(errorMessage(reason))
    } finally {
      setCompactingSessionIDs((current) => {
        const next = { ...current }
        delete next[sessionID]
        return next
      })
    }
  }

  const selectedProject = projects.find((project) => project.id === selectedProjectID) ?? null
  const selectedActiveRun = activeRunsBySession[selectedSessionID] ?? null
  useEffect(() => {
    if (debugSessionID && debugSessionID !== selectedSessionID) setDebugSessionID('')
  }, [debugSessionID, selectedSessionID])
  const debugSession = debugSessionID
    ? sessionDetail?.id === debugSessionID
      ? sessionDetail
      : Object.values(sessionsByProject).flat().find((session) => session.id === debugSessionID) ?? null
    : null
  const visibleRunningSessionIDs = new Set([...runningSessionIDs, ...Object.keys(compactingSessionIDs)])
  const showAddProject = useCallback(() => setShowProjectForm(true), [])

  if (loading) return <Splash />

  return (
    <div className="app-shell">
      <WorkspaceTree
        projects={projects}
        sessionsByProject={sessionsByProject}
        archivedSessionsByProject={archivedSessionsByProject}
        selectedProjectID={selectedProjectID}
        selectedSessionID={selectedSessionID}
		runningSessionIDs={visibleRunningSessionIDs}
        onSelectProject={selectProject}
        onSelectSession={selectSession}
        onCreateSession={openSessionCreator}
        onManageProviders={openProviderManager}
        onRenameProject={renameProject}
        onDeleteProject={deleteProject}
        onRenameSession={renameSession}
        onArchiveSession={archiveSession}
        onRestoreSession={restoreSession}
        onDeleteSession={deleteSession}
        onAdd={showAddProject}
        version={bootstrap?.version ?? ''}
      />
      <main className="conversation-panel">
        {error && <ErrorBanner message={error} onDismiss={() => setError('')} />}
        {completionNotice && <div className="completion-notice" role="status"><span>{completionNotice.sessionName} completed in the background.</span><button onClick={() => setCompletionNotice(null)} aria-label="Dismiss completion notification">×</button></div>}
        {showProjectForm ? (
          <ProjectSetup
            suggestedRoot={bootstrap?.cwd ?? ''}
            hasProjects={projects.length > 0}
            onCancel={() => setShowProjectForm(false)}
            onSubmit={(root, name) => void createProject(root, name)}
          />
        ) : selectedSessionID ? (
          <Conversation
            sessionID={selectedSessionID}
            detail={sessionDetail}
            page={itemsPage}
            activeRun={selectedActiveRun}
            compacting={Boolean(compactingSessionIDs[selectedSessionID])}
			draft={draftsBySession[selectedSessionID] ?? emptyComposerDraft}
			onDraftChange={(content) => updateDraft(selectedSessionID, content)}
			onPastedTextAdd={(pastedText) => addPastedText(selectedSessionID, pastedText)}
			onPastedTextRemove={(pastedTextID) => removePastedText(selectedSessionID, pastedTextID)}
			onPastedImageAdd={(pastedImage) => addPastedImage(selectedSessionID, pastedImage)}
			onPastedImageRemove={(pastedImageID) => removePastedImage(selectedSessionID, pastedImageID)}
			onDraftClear={() => clearDraft(selectedSessionID)}
					recentStepsByTurn={recentStepsByTurn}
            sessionNames={sessionNames}
            turnError={turnErrors[selectedSessionID] ?? null}
            onDismissTurnError={() => clearTurnError(selectedSessionID)}
            onLoadOlder={loadOlder}
            onSend={(content, images) => sendMessage(content, images)}
            onCancel={() => void cancelRun()}
            onCancelTool={(toolCallID) => void cancelToolCall(toolCallID)}
            onContinue={() => void continueRun()}
            onRetryRefresh={() => void retryRefreshSession(selectedSessionID)}
            onDebug={openDebugSettings}
            onRemoveQueuedPrompt={(promptID) => void removeQueuedPrompt(promptID)}
            onSteerQueuedPrompt={(promptID, steer) => void setQueuedPromptSteer(promptID, steer)}
            onMoveQueuedPrompt={(promptID, direction) => void moveQueuedPrompt(promptID, direction)}
            onCompact={() => void compactSession()}
            onToggleFullAccess={() => sessionDetail && void toggleFullAccess(sessionDetail)}
          />
        ) : selectedProject ? (
		  <EmptySession disabled={false} onCreate={() => void openSessionCreator()} />
        ) : (
          <ProjectSetup
            suggestedRoot={bootstrap?.cwd ?? ''}
            hasProjects={false}
            onCancel={() => undefined}
            onSubmit={(root, name) => void createProject(root, name)}
          />
        )}
      </main>
      {sessionCreator && (
        <SessionModelDialog
          project={projects.find((project) => project.id === sessionCreator.projectID)}
          state={sessionCreator}
          creating={creatingSession}
          onSelect={(selectedKey) => setSessionCreator((current) => {
            if (!current) return current
            const model = current.models.find((item) => modelKey(item) === selectedKey)
            return { ...current, selectedKey, reasoningLevel: model?.default_reasoning_level ?? '' }
          })}
          onReasoningLevel={(reasoningLevel) => setSessionCreator((current) => current ? { ...current, reasoningLevel } : current)}
          onFullAccess={(fullAccess) => setSessionCreator((current) => current ? { ...current, fullAccess } : current)}
          onCancel={() => { if (!creatingSession) setSessionCreator(null) }}
          onCreate={(model) => void createSession(sessionCreator.projectID, model)}
        />
      )}
      {providerManager && (
        <ProviderManagerDialog
          state={providerManager}
          onDocument={(document) => setProviderManager((current) => current ? { ...current, document, loading: false } : current)}
          onClose={() => setProviderManager(null)}
          onError={(message) => setError(message)}
        />
      )}
      {debugSession && (
        <DebugSettingsDialog
          session={debugSession}
          saving={savingDebugSettings}
          onSave={(settings) => saveDebugSettings(debugSession.id, settings)}
          onClose={() => { if (!savingDebugSettings) setDebugSessionID('') }}
        />
      )}
    </div>
  )
}

export default App
