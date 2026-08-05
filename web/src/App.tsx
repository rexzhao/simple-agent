import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, streamLifecycle, streamRun } from './api'
import type { CreateSessionOptions } from './api'
import type { ActiveRun, ActiveRunDescriptor, Bootstrap, ImageAttachmentInput, ItemsPage, LifecycleEvent, Project, RunEvent, Session, SessionDebugSettings, SessionItem, SessionItemProjectionEvent, SessionModelOption } from './types'
import { errorMessage } from './lib/format'
import { copyFrontendProtocolJSONL, downloadFrontendProtocolJSONL, frontendProtocolLogger, protocolLogIdentity, useFrontendProtocolLogging } from './lib/frontendProtocolLogger'
import { reduceLifecycleEvent, type SessionMaps } from './lib/lifecycleReducer'
import { reduceRunEvent } from './lib/runEventReducer'
import { modelKey, orderSessions, projectName, sessionDescendantIDs, sessionName } from './lib/session'
import { settlementRevision } from './lib/settlement'
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
  const projectsRef = useRef<Project[]>(projects)
  projectsRef.current = projects
  const { selectedProjectID, selectedSessionID, selectedProjectRef, setSelectedProjectID, setSelectedSessionID } = useSessionSelection()
  const selectedSessionIDRef = useRef(selectedSessionID)
  selectedSessionIDRef.current = selectedSessionID
  const sessionMapsRef = useRef<SessionMaps>({ active: sessionsByProject, archived: archivedSessionsByProject })
  sessionMapsRef.current = { active: sessionsByProject, archived: archivedSessionsByProject }
  // Session-list requests are point-in-time reads.  A rename/archive/refresh
  // can start another read before the first one returns, so keep a per-project
  // generation outside React state and discard an older pair of responses as
  // one unit.  This is separate from the projection store's generation: these
  // maps are still the tree's presentation cache.
  const sessionListGenerationRef = useRef<Record<string, number>>({})
  // Bootstrap and explicit project-list mutations share one generation.  A
  // stale bootstrap must not restore a project that a newer mutation removed.
  const projectListGenerationRef = useRef(0)
  const [recoveredRuns, setRecoveredRuns] = useState<ActiveRunDescriptor[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showProjectForm, setShowProjectForm] = useState(false)
  const [sessionCreator, setSessionCreator] = useState<SessionCreatorState | null>(null)
  const [providerManager, setProviderManager] = useState<ProviderManagerState | null>(null)
  const [debugSessionID, setDebugSessionID] = useState('')
  const [savingDebugSettings, setSavingDebugSettings] = useState(false)
  const [creatingSession, setCreatingSession] = useState(false)
  const creatingRootSessionRef = useRef(false)
  const [completionNotice, setCompletionNotice] = useState<BackgroundCompletionNotice | null>(null)
  const [turnErrors, setTurnErrors] = useState<Record<string, { turnID: string; message: string }>>({})
  const [compactingSessionIDs, setCompactingSessionIDs] = useState<Record<string, boolean>>({})
  const [awaitingRunStartedBySession, setAwaitingRunStartedBySession] = useState<Record<string, boolean>>({})
  const { draftsBySession, updateDraft, addPastedText, removePastedText, addPastedImage, removePastedImage, clearDraft } = useComposerDrafts()
  const sessionStore = useSessionStore()
  const frontendLogging = useFrontendProtocolLogging(debugSessionID)

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
  // Local admission responses bind a run id until that run's replay delivers
  // run.started. This prevents the process-wide lifecycle hint from creating
  // a transient run before the per-run replay boundary.
  const runStartedReplayBindingsRef = useRef(new Map<string, string>())
  const pendingAdmissionSessionsRef = useRef(new Set<string>())

  const onSupersedeRunRef = useRef<(sessionID: string, oldRunID: string) => void>(() => {})

  const { activeRunsBySession, activeRunsRef, runningSessionIDs, addActiveRun, syncActiveRuns, updateActiveRun, queueRunEvent, flushRunEvents } = useRunRegistry({ onSupersedeRun: (sessionID, oldRunID) => onSupersedeRunRef.current(sessionID, oldRunID) })

  const setSessionMaps = useCallback((maps: SessionMaps) => {
    // Any direct map replacement (bootstrap, lifecycle reducer, or mutation
    // reload) supersedes a list request that was in flight for one of these
    // projects.  Invalidate both the old and new key sets, including a
    // project that was removed by the replacement.
    const projectIDs = new Set([
      ...Object.keys(sessionMapsRef.current.active),
      ...Object.keys(sessionMapsRef.current.archived),
      ...Object.keys(maps.active),
      ...Object.keys(maps.archived),
    ])
    for (const projectID of projectIDs) {
      sessionListGenerationRef.current[projectID] = (sessionListGenerationRef.current[projectID] ?? 0) + 1
    }
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

  const setAwaitingRunStarted = useCallback((sessionID: string, awaiting: boolean) => {
    setAwaitingRunStartedBySession((current) => {
      if (awaiting) return current[sessionID] ? current : { ...current, [sessionID]: true }
      if (!current[sessionID]) return current
      const next = { ...current }
      delete next[sessionID]
      return next
    })
  }, [])

  const loadProjects = useCallback(async () => {
    const generation = ++projectListGenerationRef.current
    const payload = await api.projects()
    if (projectListGenerationRef.current !== generation) return projectsRef.current
    projectsRef.current = payload.projects
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
    const projectGeneration = ++projectListGenerationRef.current
    const operation = (async () => {
      const [bootstrapPayload, projectsPayload, activeRunsPayload] = await Promise.all([api.bootstrap(), api.projects(), api.activeRuns()])
      const sessionEntries = await Promise.all(projectsPayload.projects.map(async (project) => {
        const listGeneration = sessionListGenerationRef.current[project.id] ?? 0
        const [activePayload, archivedPayload] = await Promise.all([api.sessions(project.id), api.sessions(project.id, true)])
        return {
          projectID: project.id,
          active: orderSessions(activePayload.sessions),
          archived: orderSessions(archivedPayload.sessions),
          listGeneration,
        }
      }))
      // A newer project mutation/list bootstrap owns the project set.  Do
      // not let this operation resurrect removed projects or overwrite the
      // newer selection with its stale active-run snapshot.
      if (projectListGenerationRef.current !== projectGeneration) return []
      const currentMaps = sessionMapsRef.current
      const maps: SessionMaps = {
        active: Object.fromEntries(sessionEntries.map((entry) => [
          entry.projectID,
          (sessionListGenerationRef.current[entry.projectID] ?? 0) === entry.listGeneration
            ? entry.active
            : currentMaps.active[entry.projectID] ?? [],
        ])),
        archived: Object.fromEntries(sessionEntries.map((entry) => [
          entry.projectID,
          (sessionListGenerationRef.current[entry.projectID] ?? 0) === entry.listGeneration
            ? entry.archived
            : currentMaps.archived[entry.projectID] ?? [],
        ])),
      }
      const recovered = activeRunsPayload.runs.filter((run) => projectsPayload.projects.some((project) => maps.active[project.id]?.some((session) => session.id === run.session_id)))
      // A reconnect/bootstrap can overlap a local admission. Do not let the
      // authoritative active-run listing bypass the replay boundary by
      // creating the transient container for that pending session.
      const admissionBoundSessions = new Set(runStartedReplayBindingsRef.current.values())
      const recoveredForPresentation = recovered.filter((run) =>
        !pendingAdmissionSessionsRef.current.has(run.session_id) && !admissionBoundSessions.has(run.session_id),
      )
      const recoveredProject = recoveredForPresentation.length > 0
        ? projectsPayload.projects.find((project) => maps.active[project.id]?.some((session) => session.id === recoveredForPresentation[0].session_id))
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
        : recoveredForPresentation[0]?.session_id ?? maps.active[firstProjectID]?.[0]?.id ?? ''
      setBootstrap(bootstrapPayload)
      projectsRef.current = projectsPayload.projects
      setProjects(projectsPayload.projects)
      setSessionMaps(maps)
      syncActiveRuns(recoveredForPresentation)
      setSelectedProjectID(firstProjectID)
      setSelectedSessionID(firstSessionID)
      setRecoveredRuns(recoveredForPresentation)
      if (!preserveSelection || projectsPayload.projects.length === 0) setShowProjectForm(projectsPayload.projects.length === 0)
      return recoveredForPresentation
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
    // A response for a project that has already disappeared from the newest
    // project list must not recreate that project's session maps.  In-flight
    // requests started before removal are additionally rejected by the
    // per-project generation check below.
    if (!projectsRef.current.some((project) => project.id === projectID)) return []
    const generation = (sessionListGenerationRef.current[projectID] ?? 0) + 1
    sessionListGenerationRef.current[projectID] = generation
    const [payload, archivedPayload] = await Promise.all([api.sessions(projectID), api.sessions(projectID, true)])
    const ordered = orderSessions(payload.sessions)
    // Do not let an older pair of active/archived responses roll back a more
    // recent selection or mutation.  The caller still receives its response
    // for operation-local bookkeeping, but it cannot mutate the tree.
    if (sessionListGenerationRef.current[projectID] !== generation) return ordered
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

  // Durable items are already in the session projection. Removing a run must
  // therefore only tear down transient state; it must not copy its steps into
  // a second, post-settlement history cache.
  const removeRun = useCallback((sessionID: string, runID: string) => {
    const run = activeRunsRef.current[sessionID]
    if (!run || run.id !== runID) return
    setRecoveredRuns((current) => current.filter((r) => r.run_id !== runID))
    // Clear reconciling timers for this session.
    const timeout = reconcileTimeoutRef.current[sessionID]
    if (timeout) { window.clearTimeout(timeout); delete reconcileTimeoutRef.current[sessionID] }
    const retryTimer = reconcileRetryTimerRef.current[sessionID]
    if (retryTimer) { window.clearTimeout(retryTimer); delete reconcileRetryTimerRef.current[sessionID] }
    delete reconcileRetryCountRef.current[`${sessionID}:${runID}`]
    updateActiveRun(sessionID, runID, () => null)
  }, [updateActiveRun])

  // A superseded run may still have a reconciliation request in flight. Keep
  // that authoritative request, but do not preserve its transient steps.
  onSupersedeRunRef.current = useCallback((sessionID: string, oldRunID: string) => {
    removeRun(sessionID, oldRunID)
    void refreshSessionRef.current(sessionID).catch(() => {})
  }, [removeRun])

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

  // onSnapshotApplied: the only path that decides whether a settled run can
  // be removed after a refresh. The store reader is synchronous and therefore
  // includes projection events dispatched immediately before run.settled.
  const onSnapshotApplied = useCallback((sessionID: string) => {
    const run = activeRunsRef.current[sessionID]
    if (!run) return
    if (run.status !== 'reconciling' && run.status !== 'error_pending_refresh') return
    if (!run.settledRevision || sessionStore.isRevisionCovered(sessionID, run.settledRevision)) {
      removeRun(sessionID, run.id)
      return
    }
    if (run.status === 'error_pending_refresh') {
      updateActiveRun(sessionID, run.id, (r) => ({ ...r, status: 'reconciling' }))
      // Re-entering reconciling needs a fresh terminal deadline: the
      // original timeout already fired when the run entered error_pending.
      startReconcileTimeout(sessionID)
    }
    scheduleReconcileRetry(sessionID, run.id)
  }, [removeRun, sessionStore.isRevisionCovered, startReconcileTimeout, updateActiveRun])

  // scheduleReconcileRetry: backoff refresh, max 2 retries.
  const scheduleReconcileRetry = useCallback((sessionID: string, runID: string) => {
    const key = `${sessionID}:${runID}`
    const count = reconcileRetryCountRef.current[key] ?? 0
    if (count >= 2) return
    reconcileRetryCountRef.current[key] = count + 1
    const existing = reconcileRetryTimerRef.current[sessionID]
    if (existing) window.clearTimeout(existing)
    reconcileRetryTimerRef.current[sessionID] = window.setTimeout(() => {
      delete reconcileRetryTimerRef.current[sessionID]
      void refreshSessionRef.current(sessionID)
        .then(() => onSnapshotApplied(sessionID))
        .catch(() => {
          // A failed retry still consumes one bounded attempt, but should not
          // strand a lagging run until the 60s deadline without the remaining
          // retry opportunity.
          if (activeRunsRef.current[sessionID]?.id === runID) scheduleReconcileRetry(sessionID, runID)
        })
    }, 2000)
  }, [activeRunsRef, onSnapshotApplied])

  // retryRefreshSession: manual "refresh to see latest" handler.
  const retryRefreshSession = useCallback(async (sessionID: string) => {
    const run = activeRunsRef.current[sessionID]
    if (!run || run.status !== 'error_pending_refresh') return
    try {
      await refreshSessionRef.current(sessionID)
      onSnapshotApplied(sessionID)
    } catch { /* stay in error_pending_refresh */ }
  }, [onSnapshotApplied])

  // Auto-resolve pending reconciliation whenever fresh session detail
  // arrives: navigating to a session with a stuck "Refresh needed" banner
  // settles it without a manual click once the durable state has caught up.
  useEffect(() => {
    if (sessionDetail) onSnapshotApplied(sessionDetail.id)
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
      const session = await api.createSession({
        projectID,
        provider: model.provider,
        modelProfile: model.model_profile,
        reasoningLevel: sessionCreator?.reasoningLevel ?? model.default_reasoning_level ?? '',
        fullAccess: sessionCreator?.fullAccess ?? false,
      })
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

  const applyLifecycleSessionEvent = useCallback((event: LifecycleEvent, options: { updateStore?: boolean } = {}) => {
    const eventSession = event.session && typeof event.session === 'object'
      ? event.session
      : event.metadata ?? event.session_metadata
    if (eventSession && options.updateStore !== false) sessionStore.updateSessionMetadata(eventSession)
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
  }, [sessionStore.updateSessionMetadata, setSelectedProjectID, setSelectedSessionID, setSessionMaps])

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

  const applySettledSidebarStatus = useCallback((sessionID: string, runID: string, status: string, turnID?: string, settlementRevision?: string) => {
    const current = knownSession(sessionID)
    if (!current) return
    const failed = status === 'failed'
    const nextSession: Session = {
      ...current,
      status: failed ? 'failed' : 'idle',
      current_run_id: undefined,
      running_run_id: undefined,
      running_turn_id: undefined,
      last_run_id: runID,
      last_run_status: status,
      ...(failed
        ? { interrupted_run_id: current.interrupted_run_id ?? runID, interrupted_turn_id: current.interrupted_turn_id ?? turnID }
        : { interrupted_run_id: undefined, interrupted_turn_id: undefined }),
    }
    // Update the sidebar from the synthetic terminal transition, but do not
    // first feed its potentially stale DTO through the ordinary metadata
    // merge. The explicit settlement reducer action below owns the terminal
    // run fields when a valid watermark is available.
    applyLifecycleSessionEvent({ type: 'session.updated', session: nextSession }, { updateStore: false })
    if (settlementRevision) sessionStore.applySettlementMetadata(nextSession, settlementRevision)
  }, [applyLifecycleSessionEvent, knownSession, sessionStore.applySettlementMetadata])

  const handleRunEvent = useCallback(async (sessionID: string, runID: string, event: RunEvent) => {
    const payload = event as unknown as Record<string, unknown>
    const eventSessionID = typeof payload.session_id === 'string' ? payload.session_id : ''
    const eventRunID = typeof payload.run_id === 'string' ? payload.run_id : ''
    const projectionEvent = event.type === 'item.appended' || event.type === 'item.created' || event.type === 'item.updated'
    const logGate = (kind: 'accepted' | 'ignored', reason?: string) => {
      if (!frontendProtocolLogger.isEnabled(sessionID)) return
      frontendProtocolLogger.log({
        sessionID,
        source: 'app.event_gate',
        kind,
        ...protocolLogIdentity(payload, runID),
        reason,
        bound_session_id: sessionID,
        bound_run_id: runID,
        event: payload,
      })
    }
    // The stream callback is bound to (sessionID, runID), not to whatever an
    // untrusted/replayed payload happens to claim.  A stale stream can finish
    // after the user starts another run in the same session; accepting its
    // lifecycle/terminal events would otherwise clear the newer run's
    // sidebar state.  Committed projection events remain useful in that race,
    // but they must still agree with the bound identity.
    if ((eventSessionID && eventSessionID !== sessionID) || (eventRunID && eventRunID !== runID)) {
      logGate('ignored', 'wrong binding')
      return
    }
    const currentRun = activeRunsRef.current[sessionID]
    const admissionBinding = runStartedReplayBindingsRef.current.get(runID) === sessionID
    const newerAdmissionBinding = [...runStartedReplayBindingsRef.current.entries()]
      .some(([boundRunID, boundSessionID]) => boundSessionID === sessionID && boundRunID !== runID)
    // A new POST owns the session as soon as it is in flight, even before its
    // response can establish a run binding.  An old terminal replay must not
    // clear that admission gate or synthesize an idle sidebar transition.
    // Once the old run is the only owner, terminal replay remains legitimate
    // even when its transient ActiveRun was already removed.
    if (event.type === 'run.settled' && !admissionBinding && (pendingAdmissionSessionsRef.current.has(sessionID) || newerAdmissionBinding)) {
      logGate('ignored', 'stale run/no active run')
      return
    }
    if (!projectionEvent && event.type !== 'run.settled' && !admissionBinding && (!currentRun || currentRun.id !== runID)) {
      logGate('ignored', 'stale run/no active run')
      return
    }
    if (!projectionEvent && currentRun && currentRun.id !== runID && !(event.type === 'run.started' && currentRun.status !== 'running')) {
      logGate('ignored', 'stale run')
      return
    }
    if (event.type !== 'run.settled') logGate('accepted')
    if (event.type === 'text.delta' || event.type === 'reasoning.delta') {
      queueRunEvent(sessionID, runID, event)
      return
    }
    // Preserve stream ordering and ensure tool/settled events observe all deltas.
    flushRunEvents(sessionID, runID)
    const update = (updater: (run: ActiveRun) => ActiveRun | null) => updateActiveRun(sessionID, runID, updater)
    switch (event.type) {
      case 'run.started': {
        // Admission only identifies the run stream. The replay's committed
        // run.started event is the boundary that creates the transient run
        // container for subsequent deltas, tools, and process UI.
        runStartedReplayBindingsRef.current.delete(runID)
        setAwaitingRunStarted(sessionID, false)
        const turnID = typeof event.turn_id === 'string' ? event.turn_id : undefined
        const existing = activeRunsRef.current[sessionID]
        if (existing?.id === runID) {
          update((run) => ({ ...run, turnID: turnID ?? run.turnID, status: 'running' }))
        } else {
          addActiveRun({
            id: runID,
            sessionID,
            turnID,
            assistantText: '',
            steps: [],
            agentIteration: 0,
            status: 'running',
          })
        }
        break
      }
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
        const acceptedProjection = sessionStore.applyProjectionEvent(event as SessionItemProjectionEvent)
        // Also hand an accepted projection's explicit assistant identity to
        // the transient run reducer. A stale/replayed projection must not
        // mutate the live binding; accepted items are matched by the full
        // (turn, iteration, item) identity, never by text.
        if (acceptedProjection) update((run) => reduceRunEvent(run, event))
        break
      case 'run.resync_required':
        try {
          await refreshSession(sessionID)
        } catch (reason) {
          setError(errorMessage(reason))
        }
        break
      case 'turn.failed':
        // A late failure from a superseded stream must not surface as the
        // current turn's error.  The identity guard above normally handles
        // this; retain the explicit check for a terminal replay with no
        // transient container.
        if (activeRunsRef.current[sessionID]?.id !== runID && !admissionBinding) {
          logGate('ignored', 'stale run/no active run')
          return
        }
        update((run) => reduceRunEvent(run, event))
        setTurnErrors((current) => ({
          ...current,
          [sessionID]: { turnID: String(event.turn_id ?? ''), message: String(event.message ?? 'Run failed') },
        }))
        break
      case 'run.settled': {
        if (settledRunIDsRef.current.has(runID)) {
          logGate('ignored', 'duplicate settlement')
          return
        }
        settledRunIDsRef.current.add(runID)
        while (settledRunIDsRef.current.size > 256) {
          const oldest = settledRunIDsRef.current.values().next().value as string | undefined
          if (!oldest) break
          settledRunIDsRef.current.delete(oldest)
        }
        runStartedReplayBindingsRef.current.delete(runID)
        setAwaitingRunStarted(sessionID, false)
        const settledRun = activeRunsRef.current[sessionID]
        // A terminal event from a previous run is still a valid durable
        // projection notification, but it is not allowed to settle or
        // reconcile a newer run occupying this session.
        if (settledRun && settledRun.id !== runID) {
          logGate('ignored', 'stale run')
          return
        }
        logGate('accepted')
        const settledStatus = String(event.status)
        const settledRevision = settlementRevision(event)
        applySettledSidebarStatus(sessionID, runID, settledStatus, typeof event.turn_id === 'string' ? event.turn_id : undefined, settledRevision)
        if (!settledRun || settledRun.id !== runID) {
          // Lifecycle replay can contain only the terminal event. Do not
          // refresh merely because the transient container is gone: the
          // reducer may already have applied the final item projection.
          if (!settledRevision || !sessionStore.isRevisionCovered(sessionID, settledRevision)) {
            try { await refreshSession(sessionID) } catch (reason) {
              if (selectedSessionRef.current === sessionID) setError(errorMessage(reason))
            }
          }
          if (settledStatus === 'committed' && selectedSessionRef.current !== sessionID) {
            const session = knownSession(sessionID)
            setCompletionNotice({ sessionID, sessionName: session ? sessionName(session) : `Session ${sessionID.slice(-6)}` })
          }
          return
        }

        if (settledStatus === 'failed') {
          // turn.failed usually landed first with the real reason; fallback for late-attach.
          setTurnErrors((current) => current[sessionID]
            ? current
            : { ...current, [sessionID]: { turnID: String(event.turn_id ?? ''), message: String(event.message ?? 'Run failed') } })
        } else if (settledStatus === 'cancelled') {
          // Keep the durable partial projection. It is only a refresh target
          // when the terminal watermark is absent or not yet local.
        }

        const covered = settledRevision !== undefined && sessionStore.isRevisionCovered(sessionID, settledRevision)
        if (covered) {
          removeRun(sessionID, runID)
        } else {
          // A missing watermark is the compatibility/defensive path. It is
          // deliberately conservative, but still bounded and never deletes
          // the run until an authoritative refresh succeeds.
          update((run) => ({
            ...run,
            status: 'reconciling',
            ...(settledRevision ? { settledRevision } : {}),
          }))
          startReconcileTimeout(sessionID)
          try {
            await refreshSession(sessionID)
            onSnapshotApplied(sessionID)
          } catch {
            scheduleReconcileRetry(sessionID, runID)
          }
        }
        if (settledStatus === 'committed' && selectedSessionRef.current !== sessionID) {
          const settledSession = knownSession(sessionID)
          setCompletionNotice({
            sessionID,
            sessionName: settledSession ? sessionName(settledSession) : `Session ${sessionID.slice(-6)}`,
          })
        }
        break
      }
    }
  }, [activeRunsRef, addActiveRun, applySettledSidebarStatus, flushRunEvents, knownSession, onSnapshotApplied, queueRunEvent, refreshSession, removeRun, scheduleReconcileRetry, sessionStore.applyProjectionEvent, sessionStore.isRevisionCovered, setAwaitingRunStarted, startReconcileTimeout, updateActiveRun])

  // A run has at most one active connection by this App instance. streamRun
  // owns its replay cursor and reconnects internally; a later authoritative
  // recovery may retry a reader that has failed.
  const runStreamsRef = useRef(new Set<string>())
  const runStreamControllersRef = useRef(new Map<string, AbortController>())
  const connectRunStream = useCallback((runID: string, sessionID: string) => {
    if (!runID || !sessionID || runStreamsRef.current.has(runID) || settledRunIDsRef.current.has(runID)) return
    runStreamsRef.current.add(runID)
    const controller = new AbortController()
    runStreamControllersRef.current.set(runID, controller)
    void streamRun(runID, (event) => handleRunEvent(sessionID, runID, event), { signal: controller.signal, sessionID })
      .catch((reason: unknown) => {
        if (controller.signal.aborted) return
        // The stream may outlive a superseded run or a completed run.  Only
        // its own current run (or its still-awaiting admission binding) may
        // transition transient UI or surface an error.  In particular, a
        // late error from session A must not put a selected session B in an
        // error state.
        const currentRun = activeRunsRef.current[sessionID]
        const ownsCurrentRun = currentRun?.id === runID
        const ownsAdmission = runStartedReplayBindingsRef.current.get(runID) === sessionID
        if (settledRunIDsRef.current.has(runID) || (!ownsCurrentRun && !ownsAdmission)) return
        // streamRun already exhausted its own replay reconnects. Allow an
        // authoritative background bootstrap to retry this failed reader,
        // while keeping successful/settled runs permanently de-duplicated.
        runStreamsRef.current.delete(runID)
        if (ownsAdmission) {
          runStartedReplayBindingsRef.current.delete(runID)
          setAwaitingRunStarted(sessionID, false)
        }
        if (ownsCurrentRun) {
          updateActiveRun(sessionID, runID, (existing) => ({ ...existing, status: 'error_pending_refresh' }))
          setRecoveredRuns((current) => current.filter((item) => item.run_id !== runID))
        }
        if (selectedSessionIDRef.current === sessionID) setError(errorMessage(reason))
      })
      .finally(() => {
        runStreamsRef.current.delete(runID)
        runStreamControllersRef.current.delete(runID)
      })
  }, [activeRunsRef, handleRunEvent, setAwaitingRunStarted, updateActiveRun])

  useEffect(() => () => {
    for (const controller of runStreamControllersRef.current.values()) controller.abort()
    runStreamControllersRef.current.clear()
  }, [])

  const handleLifecycleEvent = useCallback(async (event: LifecycleEvent): Promise<void> => {
    const eventSession = event.session && typeof event.session === 'object'
      ? event.session
      : event.metadata ?? event.session_metadata
    const sessionID = event.session_id ?? (typeof event.session === 'string' ? event.session : eventSession?.id) ?? ''
    const runID = event.run_id ?? event.run ?? ''
    const logLifecycleGate = (kind: 'accepted' | 'ignored', reason?: string) => {
      if (!sessionID || !frontendProtocolLogger.isEnabled(sessionID)) return
      const payload = event as unknown as Record<string, unknown>
      frontendProtocolLogger.log({
        sessionID,
        source: 'app.lifecycle_gate',
        kind,
        ...protocolLogIdentity(payload, runID),
        reason,
        event: payload,
      })
    }
    if (event.type === 'run.settled' && sessionID && runID) {
      const currentRun = activeRunsRef.current[sessionID]
      const admissionBinding = runStartedReplayBindingsRef.current.get(runID) === sessionID
      const newerAdmissionBinding = [...runStartedReplayBindingsRef.current.entries()]
        .some(([boundRunID, boundSessionID]) => boundSessionID === sessionID && boundRunID !== runID)
      // Lifecycle settlement applies metadata before forwarding the terminal
      // event to the run reducer. Apply the same ownership gate here, or an
      // old frame carrying `metadata` could still mark the sidebar idle while
      // a new POST is waiting for its run_id.
      if (!admissionBinding && (pendingAdmissionSessionsRef.current.has(sessionID) || newerAdmissionBinding || (currentRun && currentRun.id !== runID))) {
        logLifecycleGate('ignored', 'stale run/no active run')
        return
      }
    }
    if (event.type !== 'run.started') logLifecycleGate('accepted')
    if (event.type === 'session.created' || event.type === 'session.updated' || event.type === 'session.archived' || event.type === 'session.deleted' || event.type === 'run.settled') {
      applyLifecycleSessionEvent(event)
    }

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
        // A lifecycle hint may race a local POST. It can update session
        // metadata above, but it never creates a transient run or connects a
        // stream. Local admission waits for its run replay; background runs
        // are discovered through the authoritative active-run registry.
        if (pendingAdmissionSessionsRef.current.has(sessionID) || runStartedReplayBindingsRef.current.has(runID)) {
          logLifecycleGate('ignored', 'waiting for run replay binding')
          return
        }
        if (activeRunsRef.current[sessionID]?.id === runID) {
          logLifecycleGate('ignored', 'run already active')
          return
        }
        logLifecycleGate('accepted')
        void bootstrapApplication(true).catch(() => {})
      } else {
        logLifecycleGate('accepted')
      }
      return
    }

    if (event.type === 'run.settled' && sessionID && runID) {
      // Feed lifecycle settlement through the same watermark gate as the run
      // stream. This is idempotent when both channels carry the event and,
      // importantly, does not turn a background status notification into a
      // selected-session snapshot.
      await handleRunEvent(sessionID, runID, {
        type: 'run.settled',
        run_id: runID,
        status: event.status ?? 'committed',
        turn_id: event.turn_id,
        last_seq: event.last_seq,
        committed_revision: event.committed_revision,
        message: event.message,
      })
    }
  }, [applyLifecycleSessionEvent, bootstrapApplication, handleRunEvent, knownSession])

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
      if (pendingAdmissionSessionsRef.current.has(run.session_id) || runStartedReplayBindingsRef.current.has(run.run_id)) continue
      if (!activeRunsRef.current[run.session_id]) {
        addActiveRun({
          id: run.run_id,
          sessionID: run.session_id,
          turnID: run.turn_id,
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
    pendingAdmissionSessionsRef.current.add(sessionID)
    try {
      // Admission is the only source of the run identity for a new submit.
      // Do not construct a stream URL or attach to a session stream before
      // this response has supplied the authoritative run_id.
      const started = await api.startRun(sessionID, content, imageInputs)
      if (!started.run_id || started.session_id !== sessionID) {
        throw new Error('Run admission response did not include the requested session and run_id')
      }
      pendingAdmissionSessionsRef.current.delete(sessionID)
      runStartedReplayBindingsRef.current.set(started.run_id, sessionID)
      setAwaitingRunStarted(sessionID, true)
      clearTurnError(sessionID)
      const boundRunID = started.run_id
      connectRunStream(boundRunID, sessionID)
      return true
    } catch (reason) {
      pendingAdmissionSessionsRef.current.delete(sessionID)
      setAwaitingRunStarted(sessionID, false)
      setError(errorMessage(reason))
      return false
    }
  }

  // /new creates a user root through the configured-session API. Capture the
  // source session id before the first await so a later selection change cannot
  // alter which configuration is cloned. Session-list entries do not carry
  // all creation fields, so hydrate one when the selected snapshot is not yet
  // available.
  const createRootSession = async (sourceSessionID: string): Promise<boolean> => {
    if (creatingRootSessionRef.current) return false
    const listedSource = sessionDetail?.id === sourceSessionID ? sessionDetail : knownSession(sourceSessionID)
    if (!listedSource) {
      setError('The current session is still loading; try /new again')
      return false
    }
    creatingRootSessionRef.current = true
    try {
      const source = listedSource.cwd && listedSource.config_path && listedSource.reasoning_level !== undefined
        ? listedSource
        : await api.session(sourceSessionID)
      const options: CreateSessionOptions = {
        projectID: source.project_id,
        provider: source.provider,
        modelProfile: source.model_profile,
        reasoningLevel: source.reasoning_level ?? '',
        fullAccess: source.full_access,
        cwd: source.cwd ?? source.created_cwd,
        configPath: source.config_path ?? '',
      }
      const session = await api.createSession(options)
      await loadSessions(options.projectID, session.id)
      setSelectedProjectID(options.projectID)
      setSelectedSessionID(session.id)
      setShowProjectForm(false)
      return true
    } catch (reason) {
      setError(errorMessage(reason))
      return false
    } finally {
      creatingRootSessionRef.current = false
    }
  }

  const sendMessage = async (content: string, images: PastedImageAttachment[]): Promise<boolean> => {
    if (!selectedSessionID || (!content.trim() && images.length === 0)) return false
    const sessionID = selectedSessionID
    if (content.trim() === '/new' && images.length === 0) {
      return createRootSession(sessionID)
    }
    const activeRun = activeRunsRef.current[sessionID]
    if (activeRun && activeRun.status === 'running') {
      // Append to the in-flight run: the message is queued and injected into
      // the active turn at the next safe checkpoint, or sent as a follow-up
      // turn. It is never sent as a new run here. The queued state arrives via
      // the run.prompt_queue stream event; no local echo is added.
      if (!content.trim()) return false
      try {
        await api.appendRunMessage(activeRun.id, content, sessionID)
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
    pendingAdmissionSessionsRef.current.add(sessionID)
    try {
      const started = await api.continueRun(sessionID)
      if (!started.run_id || started.session_id !== sessionID) {
        throw new Error('Run admission response did not include the requested session and run_id')
      }
      pendingAdmissionSessionsRef.current.delete(sessionID)
      runStartedReplayBindingsRef.current.set(started.run_id, sessionID)
      setAwaitingRunStarted(sessionID, true)
      clearTurnError(sessionID)
      const boundRunID = started.run_id
      connectRunStream(boundRunID, sessionID)
      return true
    } catch (reason) {
      pendingAdmissionSessionsRef.current.delete(sessionID)
      setAwaitingRunStarted(sessionID, false)
      setError(errorMessage(reason))
      return false
    }
  }, [clearTurnError, connectRunStream, selectedSessionID, sessionDetail, setAwaitingRunStarted])

  const cancelRun = async () => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.cancelRun(run.id, selectedSessionID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const cancelToolCall = useCallback(async (toolCallID: string) => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.cancelToolCall(run.id, toolCallID, selectedSessionID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [selectedSessionID])

  const removeQueuedPrompt = async (promptID: string) => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.removeRunMessage(run.id, promptID, selectedSessionID)
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
      await api.steerRunMessage(run.id, promptID, steer, selectedSessionID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const moveQueuedPrompt = async (promptID: string, direction: 'up' | 'down') => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.moveRunMessage(run.id, promptID, direction, selectedSessionID)
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
            admissionPending={Boolean(awaitingRunStartedBySession[selectedSessionID])}
            compacting={Boolean(compactingSessionIDs[selectedSessionID])}
			draft={draftsBySession[selectedSessionID] ?? emptyComposerDraft}
			onDraftChange={(content) => updateDraft(selectedSessionID, content)}
			onPastedTextAdd={(pastedText) => addPastedText(selectedSessionID, pastedText)}
			onPastedTextRemove={(pastedTextID) => removePastedText(selectedSessionID, pastedTextID)}
			onPastedImageAdd={(pastedImage) => addPastedImage(selectedSessionID, pastedImage)}
			onPastedImageRemove={(pastedImageID) => removePastedImage(selectedSessionID, pastedImageID)}
			onDraftClear={() => clearDraft(selectedSessionID)}
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
          frontendLogging={frontendLogging}
          onFrontendProtocolLoggingToggle={frontendLogging.setEnabled}
          onCopyFrontendProtocolLogs={() => copyFrontendProtocolJSONL(debugSession.id)}
          onDownloadFrontendProtocolLogs={() => downloadFrontendProtocolJSONL(debugSession.id)}
          onClearFrontendProtocolLogs={frontendLogging.clear}
          onClose={() => { if (!savingDebugSettings) setDebugSessionID('') }}
        />
      )}
    </div>
  )
}

export default App
