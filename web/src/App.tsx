import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, streamLifecycle, streamRun } from './api'
import type { ActiveRun, ActiveRunDescriptor, Bootstrap, ImageAttachmentInput, LifecycleEvent, RunEvent, Session, SessionDebugSettings, SessionModelOption } from './types'
import { ProjectIndexObservationError, type ProjectIndexReadModel, type ProjectSummary } from './repositories/projectIndex'
import { isBackgroundSessionCompletionTransition, sessionIndexCompletionNoticeKey, type SessionIndexCompletionObservation, type SessionIndexReadModel } from './repositories/sessionIndex'
import type { SessionCreateOptions } from './commands/sessionCommands'
import { copyFrontendProtocolJSONL, downloadFrontendProtocolJSONL, frontendProtocolLogger, protocolLogIdentity, useFrontendProtocolLogging } from './lib/frontendProtocolLogger'
import { reduceRunEvent } from './lib/runEventReducer'
import { modelKey, navigationSession, projectName, sessionDescendantIDs, sessionName, sessionSubPanelContext, type SessionNavigation } from './lib/session'
import { activeRunForConversation, itemsPageForConversation, sessionMetadataForConversation } from './lib/sessionContentPresentation'
import { emptyComposerDraft } from './components/Composer'
import type { PastedImageAttachment } from './components/Composer'
import { Conversation } from './components/Conversation'
import { DebugSettingsDialog } from './components/DebugSettingsDialog'
import { EmptySession, ErrorBanner, ProjectSetup, Splash } from './components/misc'
import { ProviderManagerDialog } from './components/ProviderManagerDialog'
import { SessionModelDialog } from './components/SessionModelDialog'
import type { SessionCreatorState } from './components/SessionModelDialog'
import { WorkspaceTree } from './components/WorkspaceTree'
import { SessionSubPanel } from './components/SessionSubPanel'
import { useComposerDrafts } from './hooks/useComposerDrafts'
import { useRunRegistry } from './hooks/useRunRegistry'
import { useSessionContentHistory } from './hooks/useSessionContentHistory'
import { useSessionSelection } from './hooks/useSessionSelection'
import { useCodexLogin, useCurrentCodexLoginProvider, useProjectIndex, useProviderSettings, useSessionIndexes, useSyncCommands, useSyncRepositories, useSyncSignals } from './hooks/useSyncApplication'
import type { ProviderUpdateTarget } from './commands/providerCommands'
import { codexUsageDomain, type CodexUsageDomain } from './domain/codexUsage'

type BackgroundCompletionNotice = {
  sessionID: string
  sessionName: string
}

function isProjectObservationError(reason: unknown, code?: ProjectIndexObservationError['code']): reason is ProjectIndexObservationError {
  return reason instanceof ProjectIndexObservationError && (code === undefined || reason.code === code)
}

function projectMutationErrorMessage(reason: unknown): string {
  if (isProjectObservationError(reason, 'timeout')) return 'Project change accepted; waiting for synchronization.'
  if (isProjectObservationError(reason, 'cancelled')) return 'Project operation was cancelled.'
  // Command errors can contain server/protocol details. Keep those below the
  // page boundary; the navigation surface only needs a safe retryable status.
  return 'Project operation failed.'
}

function sessionMutationErrorMessage(reason: unknown): string {
  if (reason && typeof reason === 'object' && 'code' in reason) {
    const code = String((reason as { code?: unknown }).code)
    if (code === 'timeout') return 'Session change accepted; waiting for synchronization.'
    if (code === 'cancelled') return 'Session operation was cancelled.'
  }
  return 'Session operation failed.'
}

function runMutationErrorMessage(reason: unknown): string {
  if (reason && typeof reason === 'object' && 'code' in reason) {
    const code = String((reason as { code?: unknown }).code)
    if (code === 'timeout') return 'Run command accepted; waiting for synchronization.'
    if (code === 'cancelled') return 'Run operation was cancelled.'
  }
  return 'Run operation failed.'
}

function providerOperationErrorMessage(reason: unknown, operation: 'save' | 'default' | 'discover' | 'login' | 'logout'): string {
  if (reason && typeof reason === 'object' && 'code' in reason) {
    const code = String((reason as { code?: unknown }).code)
    if (code === 'timeout') return operation === 'login' || operation === 'logout' ? 'Codex command accepted; waiting for login synchronization.' : 'Provider command accepted; waiting for settings synchronization.'
    if (code === 'cancelled') return 'Provider operation was cancelled.'
  }
  switch (operation) {
    case 'default': return 'The default model could not be changed.'
    case 'discover': return 'Provider model discovery failed.'
    case 'login': return 'Codex sign-in could not be started.'
    case 'logout': return 'Codex sign-out could not be completed.'
    default: return 'Provider settings could not be saved.'
  }
}

function App() {
  const projectIndex = useProjectIndex()
  const { project: projectCommands, session: sessionCommands, run: runCommands, provider: providerCommands, codexLogin: codexLoginCommands } = useSyncCommands()
  const { currentProject, currentSession, providerSettings: providerSettingsSignal, codexLoginProvider } = useSyncSignals()
  const { projectIndex: projectIndexRepository, sessionIndex: sessionIndexRepository, sessionContent: sessionContentRepository, providerSettings: providerSettingsRepository, codexLogin: codexLoginRepository } = useSyncRepositories()
  const providerSettings = useProviderSettings()
  const currentCodexProvider = useCurrentCodexLoginProvider()
  const codexLogin = useCodexLogin(currentCodexProvider ?? '')
  const projects = projectIndex.active
  const sessionIndexes = useSessionIndexes(projects.map((project) => project.id))
  const [bootstrap, setBootstrap] = useState<Bootstrap | null>(null)
  const { selectedProjectID, selectedSessionID, selectedProjectRef, setSelectedProjectID, setSelectedSessionID } = useSessionSelection()
  const selectedSessionIDRef = useRef(selectedSessionID)
  selectedSessionIDRef.current = selectedSessionID
  // viewingSessionID controls what the conversation panel displays. It follows
  // the tree selection but can diverge when the user picks a sub-session from
  // the floating sub-panel, without disturbing the tree's selected highlight.
  const [viewingSessionID, setViewingSessionID] = useState('')
  const viewingSessionIDRef = useRef(viewingSessionID)
  viewingSessionIDRef.current = viewingSessionID
  const projectObservationControllersRef = useRef(new Set<AbortController>())
  const sessionObservationControllersRef = useRef(new Set<AbortController>())
  const [recoveredRuns, setRecoveredRuns] = useState<ActiveRunDescriptor[]>([])
  const [error, setError] = useState('')
  const [showProjectForm, setShowProjectForm] = useState(false)
  const autoProjectFormRef = useRef(false)
  const projectFormUserOpenedRef = useRef(false)
  const [sessionCreator, setSessionCreator] = useState<SessionCreatorState | null>(null)
  const [providerManagerOpen, setProviderManagerOpen] = useState(false)
  const [debugSessionID, setDebugSessionID] = useState('')
  const [savingDebugSettings, setSavingDebugSettings] = useState(false)
  const [creatingSession, setCreatingSession] = useState(false)
  const creatingRootSessionRef = useRef(false)
  const [completionNotice, setCompletionNotice] = useState<BackgroundCompletionNotice | null>(null)
  const sessionIndexObservationsRef = useRef(new Map<string, SessionIndexCompletionObservation>())
  const sessionIndexNoticeKeysRef = useRef(new Set<string>())
  const [turnErrors, setTurnErrors] = useState<Record<string, { turnID: string; message: string }>>({})
  const [compactingSessionIDs, setCompactingSessionIDs] = useState<Record<string, boolean>>({})
  const [awaitingRunStartedBySession, setAwaitingRunStartedBySession] = useState<Record<string, boolean>>({})
  const { draftsBySession, updateDraft, addPastedText, removePastedText, addPastedImage, removePastedImage, clearDraft } = useComposerDrafts()
  const frontendLogging = useFrontendProtocolLogging(debugSessionID)

  // Session id → display name across every known project, so tool rows can
  // label session_* calls with human-readable targets instead of raw ids.
  const sessionNames = useMemo(() => {
    const names: Record<string, string> = {}
    for (const index of Object.values(sessionIndexes)) {
      for (const summary of index.summaries) if (summary.display_name) names[summary.session_id] = summary.display_name
    }
    return names
  }, [sessionIndexes])
  const settledRunIDsRef = useRef(new Set<string>())
  // Local admission responses bind a run id until that run's replay delivers
  // run.started. This prevents the process-wide lifecycle hint from creating
  // a transient run before the per-run replay boundary.
  const runStartedReplayBindingsRef = useRef(new Map<string, string>())
  const pendingAdmissionSessionsRef = useRef(new Set<string>())
  const markReadInFlightRef = useRef(new Set<string>())

  const onSupersedeRunRef = useRef<(sessionID: string, oldRunID: string) => void>(() => {})

  const { activeRunsRef, runningSessionIDs, addActiveRun, syncActiveRuns, updateActiveRun, queueRunEvent, flushRunEvents } = useRunRegistry({ onSupersedeRun: (sessionID, oldRunID) => onSupersedeRunRef.current(sessionID, oldRunID) })

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

  // A selected project is application state, not a page-owned subscription.
  // Session Index interest is owned by the composition root for every active
  // project; this signal only controls the opened session content resource.
  const previousProjectIDsRef = useRef<readonly string[]>([])
  const selectedProjectPositionRef = useRef(0)
  useEffect(() => {
    const previousIDs = previousProjectIDsRef.current
    const currentID = selectedProjectRef.current
    const currentIsValid = Boolean(currentID && projects.some((project) => project.id === currentID))
    let nextID = currentIsValid ? currentID : ''
    if (!currentIsValid) {
      const previousIndex = currentID ? previousIDs.indexOf(currentID) : -1
      const oldIndex = previousIndex >= 0 ? previousIndex : selectedProjectPositionRef.current
      const nextProject = projects[oldIndex] ?? projects[oldIndex - 1] ?? projects[0]
      nextID = nextProject?.id ?? ''
      selectedProjectPositionRef.current = Math.max(0, Math.min(oldIndex, Math.max(0, projects.length - 1)))
      setSelectedProjectID(nextID)
      setSelectedSessionID(nextID ? sessionIndexes[nextID]?.active[0]?.session_id ?? '' : '')
    }
    if (nextID) {
      const nextPosition = projects.findIndex((project) => project.id === nextID)
      if (nextPosition >= 0) selectedProjectPositionRef.current = nextPosition
    }
    const selectedIndex = nextID ? sessionIndexes[nextID] : undefined
    const selectedStillActive = Boolean(selectedIndex?.active.some((summary) => summary.session_id === selectedSessionIDRef.current))
    if (nextID && !selectedStillActive && selectedIndex?.status !== 'loading') {
      setSelectedSessionID(selectedIndex?.active[0]?.session_id ?? '')
    }
    if (projects.length === 0) {
      if (!projectFormUserOpenedRef.current) {
        autoProjectFormRef.current = true
        setShowProjectForm(true)
      }
    } else if (autoProjectFormRef.current && !projectFormUserOpenedRef.current) {
      autoProjectFormRef.current = false
      setShowProjectForm(false)
    }
    previousProjectIDsRef.current = projects.map((project) => project.id)
    // Publish the computed selection in the same effect pass. In particular,
    // do not publish null for one render while a deleted project is being
    // replaced by its deterministic neighbour.
    currentProject.set(nextID || null)
  }, [currentProject, projects, selectedProjectID, selectedProjectRef, sessionIndexes, setSelectedProjectID, setSelectedSessionID])

  useEffect(() => () => {
    for (const controller of projectObservationControllersRef.current) controller.abort()
    projectObservationControllersRef.current.clear()
    for (const controller of sessionObservationControllersRef.current) controller.abort()
    sessionObservationControllersRef.current.clear()
  }, [])

  const waitForProjectAuthority = useCallback(async (
    projectID: string,
    present: boolean,
    predicate?: (project: ProjectSummary | undefined) => boolean,
  ): Promise<ProjectIndexReadModel> => {
    const controller = new AbortController()
    projectObservationControllersRef.current.add(controller)
    try {
      return await projectIndexRepository.waitFor((model) => {
        if (model.status === 'loading') return false
        const project = model.summaries.find((summary) => summary.id === projectID)
        return (project !== undefined) === present && (predicate ? predicate(project) : true)
      }, { signal: controller.signal, timeoutMS: 5000 })
    } finally {
      projectObservationControllersRef.current.delete(controller)
    }
  }, [projectIndexRepository])

  const waitForSessionAuthority = useCallback(async (
    projectID: string,
    predicate: (model: SessionIndexReadModel) => boolean,
  ): Promise<SessionIndexReadModel> => {
    const controller = new AbortController()
    sessionObservationControllersRef.current.add(controller)
    try {
      return await sessionIndexRepository.waitFor(projectID, predicate, { signal: controller.signal, timeoutMS: 5000 })
    } finally {
      sessionObservationControllersRef.current.delete(controller)
    }
  }, [sessionIndexRepository])

  const bootstrapInFlightRef = useRef<Promise<ActiveRunDescriptor[]> | null>(null)
  const bootstrapApplication = useCallback(async (preserveSelection: boolean): Promise<ActiveRunDescriptor[]> => {
    if (bootstrapInFlightRef.current) return bootstrapInFlightRef.current
    const operation = (async () => {
      const bootstrapPayloadPromise = api.bootstrap()
      const activeRunsPayloadPromise = api.activeRuns()
      const projectModel = projectIndexRepository.getSnapshot().status === 'loading'
        ? await projectIndexRepository.waitFor((model) => model.status !== 'loading', { timeoutMS: 15_000 })
        : projectIndexRepository.getSnapshot()
      const [bootstrapPayload, activeRunsPayload] = await Promise.all([bootstrapPayloadPromise, activeRunsPayloadPromise])
      const authoritativeProjects = projectModel.active
      const admissionBoundSessions = new Set(runStartedReplayBindingsRef.current.values())
      const recoveredForPresentation = activeRunsPayload.runs.filter((run) =>
        !pendingAdmissionSessionsRef.current.has(run.session_id) && !admissionBoundSessions.has(run.session_id),
      )
      const currentProjectID = selectedProjectRef.current
      const currentSessionID = selectedSessionIDRef.current
      const currentProject = authoritativeProjects.find((project) => project.id === currentProjectID)
      const firstProjectID = preserveSelection && currentProject
        ? currentProject.id
        : authoritativeProjects[0]?.id ?? ''
      const firstIndex = firstProjectID ? sessionIndexRepository.getProjectReadModel(firstProjectID) : undefined
      const selectedStillVisible = Boolean(firstIndex?.active.some((summary) => summary.session_id === currentSessionID))
      const firstSessionID = preserveSelection && currentProject && selectedStillVisible
        ? currentSessionID
        : firstIndex?.active[0]?.session_id ?? ''
      setBootstrap(bootstrapPayload)
      syncActiveRuns(recoveredForPresentation)
      setRecoveredRuns(recoveredForPresentation)
      // Bootstrap is a recovery hint, not a late authority for user
      // selection. The first project/session effect already makes a
      // deterministic initial choice; never overwrite a click that raced the
      // bootstrap request.
      if (preserveSelection || !selectedProjectRef.current) {
        setSelectedProjectID(firstProjectID)
        setSelectedSessionID(firstSessionID)
      }
      return recoveredForPresentation
    })()
    bootstrapInFlightRef.current = operation
    try {
      return await operation
    } finally {
      if (bootstrapInFlightRef.current === operation) bootstrapInFlightRef.current = null
    }
  }, [projectIndexRepository, sessionIndexRepository, setSelectedProjectID, setSelectedSessionID, syncActiveRuns])

  useEffect(() => {
    void bootstrapApplication(false)
      .catch(() => setError('Application synchronization is unavailable.'))
  }, [bootstrapApplication])

  const selectedSessionRef = useRef(viewingSessionID)
  selectedSessionRef.current = viewingSessionID
  const { view: sessionContentView, historyState: sessionHistoryState, loadOlder, retry: retrySessionContent } = useSessionContentHistory(viewingSessionID, sessionContentRepository)
  const sessionDetail = sessionContentView.session
    ? sessionMetadataForConversation(sessionContentView.session, sessionContentView.history)
    : null
  const itemsPage = sessionContentView.session ? itemsPageForConversation(sessionContentView.history) : null
  const controlRunFor = (sessionID: string): ActiveRun | null =>
    activeRunsRef.current[sessionID]
      ?? (sessionID === viewingSessionID ? activeRunForConversation(sessionContentView, sessionID) : null)

  // Tree selection is the authoritative navigation source. The viewing session
  // follows it so that normal navigation (tree click, bootstrap, create) keeps
  // the conversation panel in sync. The sub-panel can then override it without
  // disturbing the tree's selected highlight.
  useEffect(() => { setViewingSessionID(selectedSessionID) }, [selectedSessionID])
  useEffect(() => { currentSession.set(viewingSessionID || null) }, [currentSession, viewingSessionID])

  // Completion notices are a page read-model derived from Session Index
  // transitions.  Lifecycle/run streams remain content adapters and are not a
  // second source of navigation notifications.
  useEffect(() => {
    const seen = new Set<string>()
    let notice: BackgroundCompletionNotice | undefined
    for (const [projectID, index] of Object.entries(sessionIndexes)) {
      for (const summary of index.summaries) {
        const key = `${projectID}\u0000${summary.session_id}`
        seen.add(key)
        const previous = sessionIndexObservationsRef.current.get(key)
        if (!notice && isBackgroundSessionCompletionTransition(previous, summary, viewingSessionID)) {
          const noticeKey = `${projectID}\u0000${sessionIndexCompletionNoticeKey(summary)}`
          if (!sessionIndexNoticeKeysRef.current.has(noticeKey)) {
            sessionIndexNoticeKeysRef.current.add(noticeKey)
            notice = { sessionID: summary.session_id, sessionName: summary.display_name || `Session ${summary.session_id.slice(-6)}` }
          }
        }
        sessionIndexObservationsRef.current.set(key, {
          status: summary.status,
          hasUnreadResult: summary.has_unread_result,
          runID: summary.run_id,
        })
      }
    }
    for (const key of sessionIndexObservationsRef.current.keys()) {
      if (!seen.has(key)) sessionIndexObservationsRef.current.delete(key)
    }
    if (notice) setCompletionNotice(notice)
  }, [sessionIndexes, viewingSessionID])

  // Reading a completed result is a typed command followed by an index
  // observation.  The command ack never clears the badge locally; the
  // Session Index publication does.
  useEffect(() => {
    if (!viewingSessionID) return
    let target: { projectID: string; runID: string } | undefined
    for (const [projectID, index] of Object.entries(sessionIndexes)) {
      const summary = index.summaries.find((item) => item.session_id === viewingSessionID)
      if (summary?.has_unread_result && summary.run_id) {
        target = { projectID, runID: summary.run_id }
        break
      }
    }
    if (!target || markReadInFlightRef.current.has(viewingSessionID)) return
    markReadInFlightRef.current.add(viewingSessionID)
    void sessionCommands.markRead(viewingSessionID, target.runID, target.projectID)
      .then(() => waitForSessionAuthority(target!.projectID, (index) => {
        const summary = index.summaries.find((item) => item.session_id === viewingSessionID)
        return summary?.has_unread_result === false
      }))
      .catch((reason: unknown) => setError(sessionMutationErrorMessage(reason)))
      .finally(() => markReadInFlightRef.current.delete(viewingSessionID))
  }, [sessionCommands, sessionIndexes, waitForSessionAuthority, viewingSessionID])

  // The legacy run stream is retained only as a run-control adapter. Content
  // and history are owned by session_content; settling a run must not patch a
  // page cache or trigger a REST refresh.
  const removeRun = useCallback((sessionID: string, runID: string) => {
    const run = activeRunsRef.current[sessionID]
    if (!run || run.id !== runID) return
    setRecoveredRuns((current) => current.filter((r) => r.run_id !== runID))
    updateActiveRun(sessionID, runID, () => null)
  }, [updateActiveRun])

  onSupersedeRunRef.current = useCallback((sessionID: string, oldRunID: string) => {
    removeRun(sessionID, oldRunID)
  }, [removeRun])

  useEffect(() => {
    if (!completionNotice) return
    const timer = window.setTimeout(() => setCompletionNotice(null), 5000)
    return () => window.clearTimeout(timer)
  }, [completionNotice])

  const createProject = async (root: string, displayName: string) => {
    try {
      const result = await projectCommands.createProject(root, displayName)
      await waitForProjectAuthority(result.project_id, true)
      setSelectedProjectID(result.project_id)
      setSelectedSessionID('')
      projectFormUserOpenedRef.current = false
      autoProjectFormRef.current = false
      setShowProjectForm(false)
    } catch (reason) {
      setError(projectMutationErrorMessage(reason))
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
    } catch {
      setSessionCreator(null)
      setError('Session model options are unavailable.')
    }
  }, [selectedProjectID])

  const createSession = async (projectID: string, model: SessionModelOption) => {
    if (!projectID || creatingSession) return
    setCreatingSession(true)
    try {
      const result = await sessionCommands.create(projectID, {
        provider: model.provider,
        modelProfile: model.model_profile,
        reasoningLevel: sessionCreator?.reasoningLevel ?? model.default_reasoning_level ?? '',
        fullAccess: sessionCreator?.fullAccess ?? false,
      })
      setSelectedProjectID(projectID)
      await waitForSessionAuthority(projectID, (index) => index.active.some((summary) => summary.session_id === result.session_id))
      setSelectedSessionID(result.session_id)
      projectFormUserOpenedRef.current = false
      autoProjectFormRef.current = false
      setShowProjectForm(false)
      setSessionCreator(null)
    } catch (reason) {
      setError(sessionMutationErrorMessage(reason))
    } finally {
      setCreatingSession(false)
    }
  }

  const openProviderManager = useCallback(() => {
    setError('')
    setProviderManagerOpen(true)
    providerSettingsSignal.set({ settingsEnabled: true, modelSelectionNeeded: false })
  }, [providerSettingsSignal])

  const closeProviderManager = useCallback(() => {
    setProviderManagerOpen(false)
    providerSettingsSignal.set({ settingsEnabled: false, modelSelectionNeeded: false })
    codexLoginProvider.set(null)
  }, [codexLoginProvider, providerSettingsSignal])

  const saveProvider = useCallback(async (provider: string, target: ProviderUpdateTarget, existing: boolean) => {
    const previous = providerSettingsRepository.captureAuthority()
    try {
      let changed = false
      if (existing) {
        changed = (await providerCommands.update(provider, target)).changed
      } else {
        changed = (await providerCommands.createProvider(provider, target)).changed
      }
      if (changed) await providerSettingsRepository.waitForProviderPublication(provider, previous, { timeoutMS: 5000 })
    } catch (reason) {
      throw new Error(providerOperationErrorMessage(reason, 'save'))
    }
  }, [providerCommands, providerSettingsRepository])

  const setProviderDefault = useCallback(async (provider: string, model: string) => {
    try {
      await providerCommands.setDefault(provider, model)
      await providerSettingsRepository.waitFor((settings) => settings.status === 'ready' && settings.defaultProvider === provider && settings.defaultModel === model, { timeoutMS: 5000 })
    } catch (reason) {
      throw new Error(providerOperationErrorMessage(reason, 'default'))
    }
  }, [providerCommands, providerSettingsRepository])

  const discoverProviderModels = useCallback(async (provider: string): Promise<readonly string[]> => {
    try {
      const result = await providerCommands.discoverModels(provider)
      return result.models
    } catch (reason) {
      throw new Error(providerOperationErrorMessage(reason, 'discover'))
    }
  }, [providerCommands])

  const startCodexLogin = useCallback(async (provider: string) => {
    const previous = codexLoginRepository.getSnapshot(provider)
    try {
      await codexLoginCommands.startCodexLogin(provider)
      await codexLoginRepository.waitFor(provider, (model) => model.status === 'ready' && model.login?.status === 'pending' && (model !== previous || previous.login?.status === 'pending'), { timeoutMS: 10_000 })
    } catch (reason) {
      throw new Error(providerOperationErrorMessage(reason, 'login'))
    }
  }, [codexLoginCommands, codexLoginRepository])

  const clearCodexLogin = useCallback(async (provider: string) => {
    const previous = codexLoginRepository.getSnapshot(provider)
    try {
      await codexLoginCommands.clearCodexLogin(provider)
      await codexLoginRepository.waitFor(provider, (model) => model.status === 'ready' && model.login?.status === 'signed_out' && (model !== previous || previous.login?.status === 'signed_out'), { timeoutMS: 10_000 })
    } catch (reason) {
      throw new Error(providerOperationErrorMessage(reason, 'logout'))
    }
  }, [codexLoginCommands, codexLoginRepository])

  const refreshCodexUsage = useCallback(async (provider: string): Promise<CodexUsageDomain> => {
    try {
      const usage = await api.codexUsage(provider)
      return codexUsageDomain(usage)
    } catch {
      throw new Error('Codex usage is temporarily unavailable.')
    }
  }, [])

  const selectProject = useCallback((projectID: string) => {
    selectedProjectPositionRef.current = Math.max(0, projects.findIndex((project) => project.id === projectID))
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionIndexes[projectID]?.active[0]?.session_id ?? '')
    projectFormUserOpenedRef.current = false
    autoProjectFormRef.current = false
    setShowProjectForm(false)
  }, [projects, sessionIndexes, setSelectedProjectID, setSelectedSessionID])

  const selectSession = useCallback((projectID: string, sessionID: string) => {
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionID)
    projectFormUserOpenedRef.current = false
    autoProjectFormRef.current = false
    setShowProjectForm(false)
  }, [setSelectedProjectID, setSelectedSessionID])

  const renameProject = useCallback(async (project: ProjectSummary) => {
    const displayName = window.prompt('Rename project', projectName(project))
    if (displayName === null || displayName.trim() === project.display_name) return
    if (!displayName.trim()) {
      setError('Project name cannot be empty')
      return
    }
    try {
      await projectCommands.renameProject(project.id, displayName.trim())
      await waitForProjectAuthority(project.id, true, (current) => current?.display_name === displayName.trim())
    } catch (reason) {
      setError(projectMutationErrorMessage(reason))
    }
  }, [projectCommands, waitForProjectAuthority])

  const deleteProject = useCallback(async (project: ProjectSummary) => {
    const index = sessionIndexes[project.id]
    const activeSessions = index?.active ?? []
    const archivedSessions = index?.archived ?? []
    if (activeSessions.some((session) => session.status === 'running' || activeRunsRef.current[session.session_id]?.status === 'running')) return
    const sessionCount = activeSessions.length + archivedSessions.length
    const message = `Permanently delete "${projectName(project)}" and ${sessionCount} saved ${sessionCount === 1 ? 'session' : 'sessions'}? All session history and attachments for this project will be removed. This action cannot be undone.`
    if (!window.confirm(message)) return
    let archiveAcknowledged = false
    try {
      await projectCommands.archiveProject(project.id)
      await waitForProjectAuthority(project.id, true, (current) => current?.archived === true)
      archiveAcknowledged = true
      await projectCommands.deleteProject(project.id)
      await waitForProjectAuthority(project.id, false)
    } catch (reason) {
      // Preserve the old safe rollback, but keep it on the typed command path.
      // It is only attempted after the archive itself was observed in the
      // authoritative index; an unknown delete outcome is not treated as a
      // reason to guess at replica state.
      const reasonCode = typeof reason === 'object' && reason !== null && 'code' in reason
        ? String((reason as { code?: unknown }).code)
        : ''
      const outcomeUnknown = reasonCode === 'outcome_unknown' || reasonCode === 'timeout' || reasonCode === 'transport'
      if (archiveAcknowledged && !outcomeUnknown) {
        try {
          await projectCommands.restoreProject(project.id)
          await waitForProjectAuthority(project.id, true, (current) => current?.archived === false)
        } catch {
          // Preserve the original delete/observation error.
        }
      }
      setError(projectMutationErrorMessage(reason))
    }
  }, [activeRunsRef, projectCommands, sessionIndexes, waitForProjectAuthority])

  const renameSession = useCallback(async (session: SessionNavigation) => {
    const displayName = window.prompt('Rename session', sessionName(session))
    if (displayName === null || displayName.trim() === session.display_name) return
    if (!displayName.trim()) { setError('Session name cannot be empty'); return }
    try {
      await sessionCommands.rename(session.id, displayName.trim())
      await waitForSessionAuthority(session.project_id, (index) => index.summaries.some((summary) => summary.session_id === session.id && summary.display_name === displayName.trim()))
      if (selectedSessionRef.current === session.id) {
        await sessionContentRepository.waitFor(session.id, (view) => view.session?.display_name === displayName.trim(), { timeoutMS: 5000 })
      }
    } catch (reason) { setError(sessionMutationErrorMessage(reason)) }
  }, [selectedSessionRef, sessionCommands, sessionContentRepository, waitForSessionAuthority])

  const toggleFullAccess = useCallback(async (session: Session) => {
    try {
      const fullAccess = !session.full_access
      await sessionCommands.setFullAccess(session.id, fullAccess)
      if (selectedSessionRef.current === session.id) await sessionContentRepository.waitFor(session.id, (view) => view.session?.full_access === fullAccess, { timeoutMS: 5000 })
    } catch (reason) { setError(sessionMutationErrorMessage(reason)) }
  }, [selectedSessionRef, sessionCommands, sessionContentRepository])

  const openDebugSettings = useCallback(() => {
    if (viewingSessionID) setDebugSessionID(viewingSessionID)
  }, [viewingSessionID])

  const saveDebugSettings = useCallback(async (sessionID: string, settings: SessionDebugSettings) => {
    setSavingDebugSettings(true)
    try {
      await sessionCommands.setDebug(sessionID, settings.request_bodies)
      if (selectedSessionRef.current === sessionID) await sessionContentRepository.waitFor(sessionID, (view) => view.session?.debug.request_bodies === settings.request_bodies, { timeoutMS: 5000 })
      setDebugSessionID('')
    } catch (reason) {
      setError(sessionMutationErrorMessage(reason))
      throw reason
    } finally { setSavingDebugSettings(false) }
  }, [selectedSessionRef, sessionCommands, sessionContentRepository])

  const archiveSession = useCallback(async (session: SessionNavigation) => {
    const index = sessionIndexes[session.project_id]
    const projectSessions = [...(index?.active.map(navigationSession) ?? []), ...(index?.archived.map(navigationSession) ?? [])]
    const subtreeIDs = [session.id, ...sessionDescendantIDs(projectSessions, session.id)]
    const busyIDs = new Set(projectSessions.filter((item) => item.status === 'running').map((item) => item.id))
    if (subtreeIDs.some((id) => busyIDs.has(id) || activeRunsRef.current[id]?.status === 'running')) return
    const childCount = subtreeIDs.length - 1
    const childNote = childCount > 0 ? ` ${childCount} child ${childCount === 1 ? 'session' : 'sessions'} will also be archived.` : ''
    if (!window.confirm(`Archive "${sessionName(session)}"? It will be hidden from the current list.${childNote}`)) return
    try {
      await sessionCommands.archive(session.id)
      await waitForSessionAuthority(session.project_id, (next) => next.summaries.some((summary) => summary.session_id === session.id && summary.archived))
      if (selectedSessionRef.current === session.id) {
        const next = sessionIndexRepository.getProjectReadModel(session.project_id).active[0]
        setSelectedSessionID(next?.session_id ?? '')
      }
    } catch (reason) { setError(sessionMutationErrorMessage(reason)) }
  }, [activeRunsRef, sessionIndexRepository, sessionIndexes, selectedSessionRef, sessionCommands, setSelectedSessionID, waitForSessionAuthority])

  const restoreSession = useCallback(async (session: SessionNavigation) => {
    try {
      await sessionCommands.restore(session.id)
      await waitForSessionAuthority(session.project_id, (index) => index.active.some((summary) => summary.session_id === session.id))
      setSelectedProjectID(session.project_id)
      setSelectedSessionID(session.id)
      setShowProjectForm(false)
    } catch (reason) { setError(sessionMutationErrorMessage(reason)) }
  }, [sessionCommands, setSelectedProjectID, setSelectedSessionID, waitForSessionAuthority])

  const deleteSession = useCallback(async (session: SessionNavigation) => {
    const index = sessionIndexes[session.project_id]
    const projectSessions = [...(index?.active.map(navigationSession) ?? []), ...(index?.archived.map(navigationSession) ?? [])]
    const subtreeIDs = [session.id, ...sessionDescendantIDs(projectSessions, session.id)]
    const initialArchivedByID = new Map(projectSessions.map((item) => [item.id, item.archived]))
    const busyIDs = new Set(projectSessions.filter((item) => item.status === 'running').map((item) => item.id))
    if (subtreeIDs.some((id) => busyIDs.has(id) || activeRunsRef.current[id]?.status === 'running')) return
    const childCount = subtreeIDs.length - 1
    const childNote = childCount > 0 ? ` ${childCount} child ${childCount === 1 ? 'session' : 'sessions'} will also be permanently deleted.` : ''
    if (!window.confirm(`Permanently delete "${sessionName(session)}"? This action cannot be undone.${childNote}`)) return
    const subtreeIsArchived = (next: SessionIndexReadModel): boolean => subtreeIDs.every((sessionID) => {
      const summary = next.summaries.find((item) => item.session_id === sessionID)
      return summary !== undefined && summary.archived
    })
    let archiveAcknowledged = session.archived
    try {
      // RemoveSession deliberately enforces archive-first on the server.  The
      // command ack is not the replica: do not issue delete until the index
      // has published the effective archived state for every descendant.
      if (!session.archived) {
        await sessionCommands.archive(session.id)
        await waitForSessionAuthority(session.project_id, subtreeIsArchived)
        archiveAcknowledged = true
      }
      await sessionCommands.deleteSession(session.id)
      await waitForSessionAuthority(session.project_id, (next) => !next.summaries.some((summary) => subtreeIDs.includes(summary.session_id)))
      if (subtreeIDs.includes(selectedSessionRef.current)) {
        const next = sessionIndexRepository.getProjectReadModel(session.project_id).active[0]
        setSelectedProjectID(session.project_id)
        setSelectedSessionID(next?.session_id ?? '')
      }
    } catch (reason) {
      // Once archive authority is known, an explicit later command failure is
      // safe to compensate.  A timeout/transport/outcome_unknown can mean the
      // server already applied delete, so guessing with restore could recreate
      // or mutate a resource after a successful operation.
      const reasonCode = reason && typeof reason === 'object' && 'code' in reason
        ? String((reason as { code?: unknown }).code)
        : ''
      const outcomeUnknown = reasonCode === 'outcome_unknown' || reasonCode === 'timeout' || reasonCode === 'transport'
      if (archiveAcknowledged && !session.archived && !outcomeUnknown) {
        try {
          await sessionCommands.restore(session.id)
          await waitForSessionAuthority(session.project_id, (next) => subtreeIDs.every((sessionID) => {
            const summary = next.summaries.find((item) => item.session_id === sessionID)
            return summary !== undefined && summary.archived === (initialArchivedByID.get(sessionID) ?? false)
          }))
        } catch {
          // Preserve the original delete error; the authority remains the
          // source of truth if compensation itself cannot be observed.
        }
      }
      setError(sessionMutationErrorMessage(reason))
    }
  }, [activeRunsRef, sessionIndexRepository, sessionIndexes, selectedSessionRef, sessionCommands, setSelectedProjectID, setSelectedSessionID, waitForSessionAuthority])

  // Session Index owns navigation metadata and status. Lifecycle SSE and the
  // per-run stream are now retained only as the explicit run-control adapter:
  // they admit/replay/control runs, but never write Session Content or the
  // navigation read model. A later run-control cutover can remove this block.
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
        // Durable item publication is owned by session_content. The legacy
        // run stream remains a control/replay adapter and cannot write a
        // second content reducer or projection cache.
        break
      case 'run.resync_required':
        sessionContentRepository.retry(sessionID)
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
        if (settledStatus === 'failed') {
          // turn.failed usually landed first with the real reason; fallback
          // for a late-attach run-control replay.
          setTurnErrors((current) => current[sessionID]
            ? current
            : { ...current, [sessionID]: { turnID: String(event.turn_id ?? ''), message: String(event.message ?? 'Run failed') } })
        }
        // Session Content receives the settlement/transient barrier itself.
        // Removing this run only affects the transitional control adapter.
        if (!settledRun || settledRun.id === runID) removeRun(sessionID, runID)
        break
      }
    }
  }, [activeRunsRef, addActiveRun, flushRunEvents, queueRunEvent, removeRun, sessionContentRepository, setAwaitingRunStarted, updateActiveRun])

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
        if (viewingSessionIDRef.current === sessionID) setError('Run synchronization is unavailable.')
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
    // Session lifecycle frames are deliberately not merged into the
    // navigation state. Session Index is the only authority for list,
    // archive, unread, and status fields.
    if (event.type === 'session.created' || event.type === 'session.updated' || event.type === 'session.archived' || event.type === 'session.deleted') return

    if (event.type === 'run.started') {
      if (sessionID && runID) {
        // A lifecycle hint may race a local POST. It never creates a
        // navigation status transition or a transient run. Local admission
        // waits for its run replay; background runs are discovered through
        // the authoritative active-run registry and Session Index.
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
  }, [bootstrapApplication, handleRunEvent])

  const lifecycleEventHandlerRef = useRef<(event: LifecycleEvent) => Promise<void>>(async () => {})
  lifecycleEventHandlerRef.current = handleLifecycleEvent
  const reconcileLifecycleRef = useRef<() => Promise<void>>(async () => {})
  reconcileLifecycleRef.current = async () => {
    try {
      await bootstrapApplication(true)
    } catch (reason) {
      setError('Application synchronization is unavailable.')
      throw reason
    }
  }

  useEffect(() => {
    const controller = new AbortController()
    void streamLifecycle(
      (event) => lifecycleEventHandlerRef.current(event),
      { signal: controller.signal, onReconnect: () => reconcileLifecycleRef.current() },
    ).catch((reason: unknown) => {
      if (!controller.signal.aborted) setError('Application synchronization is unavailable.')
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
      // Text admission uses the typed control-plane command. The image path
      // remains a narrowly scoped transition until attachments have their own
      // Blob upload command; neither path patches session content locally.
      const started = imageInputs.length > 0
        ? await api.startRun(sessionID, content, imageInputs)
        : await runCommands.startRun(sessionID, content)
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
      setError(runMutationErrorMessage(reason))
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
    const listedSource = sessionDetail?.id === sourceSessionID ? sessionDetail : null
    if (!listedSource) {
      setError('The current session is still loading; try /new again')
      return false
    }
    creatingRootSessionRef.current = true
    try {
      const source = listedSource
      const cwd = source.cwd ?? source.created_cwd
      // Session Content fields are an authority projection, not a legacy
      // REST creation DTO.  Only values that are actually present are sent;
      // omission deliberately delegates provider/model/config/reasoning
      // defaults to the server.
      const options: SessionCreateOptions = {
        ...(cwd !== undefined ? { cwd } : {}),
        ...(source.config_path !== undefined ? { configPath: source.config_path } : {}),
        ...(source.provider !== undefined ? { provider: source.provider } : {}),
        ...(source.model_profile !== undefined ? { modelProfile: source.model_profile } : {}),
        ...(source.reasoning_level !== undefined ? { reasoningLevel: source.reasoning_level } : {}),
        fullAccess: source.full_access,
      }
      const created = await sessionCommands.create(source.project_id, options)
      await waitForSessionAuthority(source.project_id, (index) => index.active.some((summary) => summary.session_id === created.session_id))
      setSelectedProjectID(source.project_id)
      setSelectedSessionID(created.session_id)
      setShowProjectForm(false)
      return true
    } catch (reason) {
      setError(sessionMutationErrorMessage(reason))
      return false
    } finally {
      creatingRootSessionRef.current = false
    }
  }

  const sendMessage = async (content: string, images: PastedImageAttachment[]): Promise<boolean> => {
    if (!viewingSessionID || (!content.trim() && images.length === 0)) return false
    const sessionID = viewingSessionID
    if (content.trim() === '/new' && images.length === 0) {
      return createRootSession(sessionID)
    }
    const activeRun = controlRunFor(sessionID)
    if (activeRun && activeRun.status === 'running') {
      // Append to the in-flight run: the message is queued and injected into
      // the active turn at the next safe checkpoint, or sent as a follow-up
      // turn. It is never sent as a new run here. The queued state arrives via
      // the run.prompt_queue stream event; no local echo is added.
      if (!content.trim()) return false
      try {
        await runCommands.appendPrompt(sessionID, activeRun.id, content)
        return true
      } catch (reason) {
        setError(runMutationErrorMessage(reason))
        return false
      }
    }
    const imageInputs: ImageAttachmentInput[] = images.map((image) => ({ data_url: image.dataURL, detail: 'auto' }))
    return startNewRun(sessionID, content, imageInputs)
  }

  const continueRun = useCallback(async (): Promise<boolean> => {
    if (!viewingSessionID) return false
    const sessionID = viewingSessionID
    const activeRun = controlRunFor(sessionID)
    const detail = sessionDetail?.id === sessionID ? sessionDetail : undefined
    if (activeRun?.status === 'running' || !detail || (detail.status !== 'interrupted' && detail.status !== 'failed') || !detail.interrupted_run_id || !detail.interrupted_turn_id) {
      return false
    }
    pendingAdmissionSessionsRef.current.add(sessionID)
    try {
      const started = await runCommands.continueRun(sessionID)
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
      setError(runMutationErrorMessage(reason))
      return false
    }
  }, [clearTurnError, connectRunStream, runCommands, viewingSessionID, sessionDetail, setAwaitingRunStarted])

  const cancelRun = async () => {
    const run = controlRunFor(viewingSessionID)
    if (!run) return
    try {
      await runCommands.cancelRun(run.id)
    } catch (reason) {
      setError(runMutationErrorMessage(reason))
    }
  }

  const cancelToolCall = useCallback(async (toolCallID: string) => {
    const run = controlRunFor(viewingSessionID)
    if (!run) return
    try {
      await runCommands.cancelTool(viewingSessionID, run.id, toolCallID)
    } catch (reason) {
      setError(runMutationErrorMessage(reason))
    }
  }, [runCommands, viewingSessionID])

  const removeQueuedPrompt = async (promptID: string) => {
    const run = controlRunFor(viewingSessionID)
    if (!run) return
    try {
      await runCommands.removePrompt(viewingSessionID, run.id, promptID)
    } catch (reason) {
      setError(runMutationErrorMessage(reason))
    }
  }

  // Steer/move mutations only call the API; the updated queue arrives via the
  // run.prompt_queue stream event, keeping the server the single source of
  // truth for queue order.
  const setQueuedPromptSteer = async (promptID: string, steer: boolean) => {
    const run = controlRunFor(viewingSessionID)
    if (!run) return
    try {
      await runCommands.steerPrompt(viewingSessionID, run.id, promptID, steer)
    } catch (reason) {
      setError(runMutationErrorMessage(reason))
    }
  }

  const moveQueuedPrompt = async (promptID: string, direction: 'up' | 'down') => {
    const run = controlRunFor(viewingSessionID)
    if (!run) return
    try {
      await runCommands.movePrompt(viewingSessionID, run.id, promptID, direction === 'up' ? -1 : 1)
    } catch (reason) {
      setError(runMutationErrorMessage(reason))
    }
  }

  const compactSession = async () => {
    if (!viewingSessionID || sessionDetail?.status === 'running' || controlRunFor(viewingSessionID)?.status === 'running') return
    const sessionID = viewingSessionID
    setCompactingSessionIDs((current) => ({ ...current, [sessionID]: true }))
    try {
      const result = await sessionCommands.compact(sessionID)
      await sessionContentRepository.waitFor(sessionID, (view) =>
        view.compaction.checkpoints.some((checkpoint) => checkpoint.id === result.compaction_id || checkpoint.summary_item_id === result.summary_item_id)
        || view.history.items.some((item) => item.key.item_id === result.summary_item_id),
        { timeoutMS: 5000 },
      )
    } catch (reason) {
      setError(sessionMutationErrorMessage(reason))
    } finally {
      setCompactingSessionIDs((current) => {
        const next = { ...current }
        delete next[sessionID]
        return next
      })
    }
  }

  const selectedProject = projects.find((project) => project.id === selectedProjectID) ?? null
  const viewingSessionSummary = Object.values(sessionIndexes)
    .flatMap((index) => index.summaries)
    .find((summary) => summary.session_id === viewingSessionID)
  const selectedProjectSessions = selectedProjectID
    ? (sessionIndexes[selectedProjectID]?.active.map(navigationSession) ?? [])
    : []
  const subPanelContext = viewingSessionID ? sessionSubPanelContext(selectedProjectSessions, viewingSessionID) : null
  // session_content subscription_event is the opened session's transient
  // content authority. The run registry below remains only for control
  // admission/replay and background run visibility.
  const selectedActiveRun = activeRunForConversation(sessionContentView, viewingSessionID)
  useEffect(() => {
    if (debugSessionID && debugSessionID !== viewingSessionID) setDebugSessionID('')
  }, [debugSessionID, viewingSessionID])
  const debugSession = debugSessionID
    ? sessionDetail?.id === debugSessionID
      ? sessionDetail
      : null
    : null
  const indexedRunningSessionIDs = Object.values(sessionIndexes).flatMap((index) => index.summaries.filter((summary) => summary.status === 'running').map((summary) => summary.session_id))
  const visibleRunningSessionIDs = new Set([...indexedRunningSessionIDs, ...runningSessionIDs, ...Object.keys(compactingSessionIDs)])
  const showAddProject = useCallback(() => {
    projectFormUserOpenedRef.current = true
    autoProjectFormRef.current = false
    setShowProjectForm(true)
  }, [])
  const retryProjectIndex = useCallback(() => {
    setError('')
    projectIndexRepository.retry()
  }, [projectIndexRepository])
  const retrySessionIndex = useCallback((projectID: string) => {
    setError('')
    sessionIndexRepository.retry(projectID)
  }, [sessionIndexRepository])
  const retryProviderSettings = useCallback(() => {
    setError('')
    providerSettingsRepository.retry()
  }, [providerSettingsRepository])
  const retryCodexLogin = useCallback(() => {
    if (currentCodexProvider) codexLoginRepository.retry(currentCodexProvider)
  }, [codexLoginRepository, currentCodexProvider])

  if (projectIndex.status === 'loading') return <Splash />

  return (
    <div className="app-shell">
      <WorkspaceTree
        projects={projects}
        sessionIndexes={sessionIndexes}
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
        onRetrySessionIndex={retrySessionIndex}
        onAdd={showAddProject}
        version={bootstrap?.version ?? ''}
      />
      {subPanelContext && (
        <SessionSubPanel
          context={subPanelContext}
          viewingSessionID={viewingSessionID}
          runningSessionIDs={visibleRunningSessionIDs}
          onSelectSession={(sessionID) => setViewingSessionID(sessionID)}
          onRenameSession={renameSession}
          onArchiveSession={archiveSession}
          onDeleteSession={deleteSession}
        />
      )}
      <main className="conversation-panel">
        {projectIndex.status === 'stale' && projects.length > 0 && (
          <div className="sync-status" aria-live="polite">
            <span>Project list is offline; showing the last synchronized list.</span>
            {projectIndex.error && <button onClick={retryProjectIndex}>Retry project synchronization</button>}
          </div>
        )}
        {projectIndex.status === 'error' && projects.length === 0 && (
          <div className="error-banner" role="alert"><span>Project list is unavailable.</span><button onClick={retryProjectIndex}>Retry</button></div>
        )}
        {error && <ErrorBanner message={error} onDismiss={() => setError('')} />}
        {completionNotice && <div className="completion-notice" role="status"><span>{completionNotice.sessionName} completed in the background.</span><button onClick={() => setCompletionNotice(null)} aria-label="Dismiss completion notification">×</button></div>}
        {showProjectForm ? (
          <ProjectSetup
            suggestedRoot={bootstrap?.cwd ?? ''}
            hasProjects={projects.length > 0}
            onCancel={() => {
              projectFormUserOpenedRef.current = false
              autoProjectFormRef.current = false
              setShowProjectForm(false)
            }}
            onSubmit={(root, name) => void createProject(root, name)}
          />
        ) : viewingSessionID ? (
          <Conversation
            sessionID={viewingSessionID}
            detail={sessionDetail}
            sessionIndexStatus={viewingSessionSummary?.status}
            page={itemsPage}
            activeRun={selectedActiveRun}
            admissionPending={Boolean(awaitingRunStartedBySession[viewingSessionID])}
            compacting={Boolean(compactingSessionIDs[viewingSessionID])}
			contentAvailability={sessionContentView.availability}
			historyLoading={sessionHistoryState.loading}
			historyError={sessionHistoryState.error}
			draft={draftsBySession[viewingSessionID] ?? emptyComposerDraft}
			onDraftChange={(content) => updateDraft(viewingSessionID, content)}
			onPastedTextAdd={(pastedText) => addPastedText(viewingSessionID, pastedText)}
			onPastedTextRemove={(pastedTextID) => removePastedText(viewingSessionID, pastedTextID)}
			onPastedImageAdd={(pastedImage) => addPastedImage(viewingSessionID, pastedImage)}
			onPastedImageRemove={(pastedImageID) => removePastedImage(viewingSessionID, pastedImageID)}
			onDraftClear={() => clearDraft(viewingSessionID)}
            sessionNames={sessionNames}
            turnError={turnErrors[viewingSessionID] ?? null}
            onDismissTurnError={() => clearTurnError(viewingSessionID)}
            onLoadOlder={loadOlder}
            onSend={(content, images) => sendMessage(content, images)}
            onCancel={() => void cancelRun()}
            onCancelTool={(toolCallID) => void cancelToolCall(toolCallID)}
            onContinue={() => void continueRun()}
            onRetryRefresh={retrySessionContent}
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
      {providerManagerOpen && (
        <ProviderManagerDialog
          state={providerSettings}
          codexLogin={currentCodexProvider ? codexLogin : null}
          onProviderChange={(provider) => codexLoginProvider.set(provider)}
          onSave={saveProvider}
          onSetDefault={setProviderDefault}
          onDiscoverModels={discoverProviderModels}
          onStartCodexLogin={startCodexLogin}
          onClearCodexLogin={clearCodexLogin}
          onRefreshUsage={refreshCodexUsage}
          onRetrySettings={retryProviderSettings}
          onRetryCodexLogin={retryCodexLogin}
          onClose={closeProviderManager}
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
