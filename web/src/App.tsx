import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { Session, SessionDebugSettings, SessionModelOption } from './types'
import type { ApplicationBootstrap } from './applicationServices'
import { ProjectIndexObservationError, type ProjectIndexReadModel, type ProjectSummary } from './repositories/projectIndex'
import { isBackgroundSessionCompletionTransition, sessionIndexCompletionNoticeKey, type SessionIndexCompletionObservation, type SessionIndexReadModel } from './repositories/sessionIndex'
import type { SessionCreateOptions } from './commands/sessionCommands'
import { copyFrontendProtocolJSONL, downloadFrontendProtocolJSONL, useFrontendProtocolLogging } from './lib/frontendProtocolLogger'
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
import { useSessionContentHistory } from './hooks/useSessionContentHistory'
import { useSessionSelection } from './hooks/useSessionSelection'
import { useCodexLogin, useCurrentCodexLoginProvider, useProjectIndex, useProviderSettings, useSessionIndexes, useSyncApplication, useSyncCommands, useSyncRepositories, useSyncSignals } from './hooks/useSyncApplication'
import type { ProviderUpdateTarget } from './commands/providerCommands'
import type { CodexUsageDomain } from './domain/codexUsage'

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

const runAuthorityResyncMessage = 'Run accepted; automatic synchronization is in progress.'

function firstActiveRootSessionID(index: SessionIndexReadModel | undefined): string {
  return index?.active.find((summary) => !summary.parent_session_id)?.session_id ?? ''
}

function rootSessionID(sessions: readonly SessionNavigation[], sessionID: string): string {
  const byID = new Map(sessions.map((session) => [session.id, session]))
  let current = byID.get(sessionID)
  if (!current) return ''
  const seen = new Set<string>()
  while (current.parent_session_id && !seen.has(current.id)) {
    seen.add(current.id)
    const parent = byID.get(current.parent_session_id)
    if (!parent) break
    current = parent
  }
  return current.id
}

function providerOperationErrorMessage(reason: unknown, operation: 'save' | 'default' | 'discover' | 'catalog' | 'login' | 'logout'): string {
  if (reason && typeof reason === 'object' && 'code' in reason) {
    const code = String((reason as { code?: unknown }).code)
    if (code === 'timeout') return operation === 'login' || operation === 'logout' ? 'Codex command accepted; waiting for login synchronization.' : 'Provider command accepted; waiting for settings synchronization.'
    if (code === 'cancelled') return 'Provider operation was cancelled.'
  }
  switch (operation) {
    case 'default': return 'The default model could not be changed.'
    case 'discover': return 'Provider model discovery failed.'
    case 'catalog': return 'Model catalog search failed.'
    case 'login': return 'Codex sign-in could not be started.'
    case 'logout': return 'Codex sign-out could not be completed.'
    default: return 'Provider settings could not be saved.'
  }
}

function App() {
  const { loadBootstrap, sessionModels, codexUsage, loadSessionImage, uploadSessionImage } = useSyncApplication()
  const projectIndex = useProjectIndex()
  const { project: projectCommands, session: sessionCommands, run: runCommands, provider: providerCommands, codexLogin: codexLoginCommands } = useSyncCommands()
  const { currentProject, currentSession, providerSettings: providerSettingsSignal, codexLoginProvider } = useSyncSignals()
  const { projectIndex: projectIndexRepository, sessionIndex: sessionIndexRepository, sessionContent: sessionContentRepository, providerSettings: providerSettingsRepository, codexLogin: codexLoginRepository } = useSyncRepositories()
  const providerSettings = useProviderSettings()
  const currentCodexProvider = useCurrentCodexLoginProvider()
  const codexLogin = useCodexLogin(currentCodexProvider ?? '')
  const projects = projectIndex.active
  const sessionIndexes = useSessionIndexes(projects.map((project) => project.id))
  const [bootstrap, setBootstrap] = useState<ApplicationBootstrap | null>(null)
  const { selectedProjectID, selectedSessionID, selectedProjectRef, setSelectedProjectID, setSelectedSessionID } = useSessionSelection()
  const selectedSessionIDRef = useRef(selectedSessionID)
  selectedSessionIDRef.current = selectedSessionID
  // viewingSessionID controls what the conversation panel displays. It follows
  // the tree selection but can diverge when the user picks a sub-session from
  // the floating sub-panel, without disturbing the tree's selected highlight.
  const [viewingSessionID, setViewingSessionID] = useState('')
  const viewingSessionIDRef = useRef(viewingSessionID)
  viewingSessionIDRef.current = viewingSessionID
  // A child selected from the floating panel may be viewed while the root
  // remains selected in the rail. This ref prevents the normal tree-selection
  // effect from replacing an explicit child view during a root change.
  const preserveViewingForSelectedSessionIDRef = useRef('')
  const projectObservationControllersRef = useRef(new Set<AbortController>())
  const sessionObservationControllersRef = useRef(new Set<AbortController>())
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
  const [turnErrors, setTurnErrors] = useState<Record<string, { turnID: string; code: string; message: string }>>({})
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
  // The command result is only an admission acknowledgement.  The selected
  // Session Content projection must publish the matching run before the page
  // considers the run visible or enables another admission.
  const pendingRunIDsRef = useRef(new Map<string, string>())
  const pendingAdmissionSessionsRef = useRef(new Set<string>())
  const pendingRunAuthorityWaitsRef = useRef(new Map<string, AbortController>())
  const markReadInFlightRef = useRef(new Set<string>())

  const runAdmissionKey = (sessionID: string, runID: string) => `${sessionID}\u0000${runID}`

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

  const isAdmissionPending = useCallback((sessionID: string) =>
    pendingAdmissionSessionsRef.current.has(sessionID)
    || pendingRunIDsRef.current.has(sessionID)
    || Boolean(awaitingRunStartedBySession[sessionID]),
  [awaitingRunStartedBySession])

  const releaseRunAdmission = useCallback((sessionID: string, runID: string): boolean => {
    if (pendingRunIDsRef.current.get(sessionID) !== runID) return false
    pendingRunIDsRef.current.delete(sessionID)
    const key = runAdmissionKey(sessionID, runID)
    const controller = pendingRunAuthorityWaitsRef.current.get(key)
    if (controller) {
      pendingRunAuthorityWaitsRef.current.delete(key)
      controller.abort()
    }
    setAwaitingRunStarted(sessionID, false)
    setError((current) => current === runAuthorityResyncMessage ? '' : current)
    return true
  }, [setAwaitingRunStarted])

  const observeRunAdmission = useCallback((sessionID: string, runID: string) => {
    if (pendingRunIDsRef.current.get(sessionID) !== runID) return
    if (sessionContentRepository.hasObservedRun(sessionID, runID)) {
      releaseRunAdmission(sessionID, runID)
      return
    }
    const key = runAdmissionKey(sessionID, runID)
    if (pendingRunAuthorityWaitsRef.current.has(key)) return
    const controller = new AbortController()
    pendingRunAuthorityWaitsRef.current.set(key, controller)
    void sessionContentRepository.waitForRunObserved(sessionID, runID, { signal: controller.signal, timeoutMS: 5000 })
      .then(() => { releaseRunAdmission(sessionID, runID) })
      .catch((reason: unknown) => {
        if (controller.signal.aborted || (reason && typeof reason === 'object' && 'code' in reason && String((reason as { code?: unknown }).code) === 'cancelled')) return
        if (pendingRunIDsRef.current.get(sessionID) !== runID) return
        // The accepted command still owns this session. Reconnect/resubscribe
        // the authority resource, but never permit a second admission merely
        // because the observation timed out.
        sessionContentRepository.retry(sessionID)
        setError(runAuthorityResyncMessage)
      })
      .finally(() => {
        if (pendingRunAuthorityWaitsRef.current.get(key) === controller) pendingRunAuthorityWaitsRef.current.delete(key)
      })
  }, [releaseRunAdmission, sessionContentRepository])

  useEffect(() => () => {
    for (const controller of pendingRunAuthorityWaitsRef.current.values()) controller.abort()
    pendingRunAuthorityWaitsRef.current.clear()
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
      setSelectedSessionID(nextID ? firstActiveRootSessionID(sessionIndexes[nextID]) : '')
    }
    if (nextID) {
      const nextPosition = projects.findIndex((project) => project.id === nextID)
      if (nextPosition >= 0) selectedProjectPositionRef.current = nextPosition
    }
    const selectedIndex = nextID ? sessionIndexes[nextID] : undefined
    const selectedStillActive = Boolean(selectedIndex?.active.some((summary) => !summary.parent_session_id && summary.session_id === selectedSessionIDRef.current))
    if (nextID && !selectedStillActive && selectedIndex?.status !== 'loading') {
      setSelectedSessionID(firstActiveRootSessionID(selectedIndex))
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

  const bootstrapInFlightRef = useRef<Promise<void> | null>(null)
  const bootstrapApplication = useCallback(async (preserveSelection: boolean): Promise<void> => {
    if (bootstrapInFlightRef.current) return bootstrapInFlightRef.current
    const operation = (async () => {
      const bootstrapPayloadPromise = loadBootstrap()
      const projectModel = projectIndexRepository.getSnapshot().status === 'loading'
        ? await projectIndexRepository.waitFor((model) => model.status !== 'loading', { timeoutMS: 15_000 })
        : projectIndexRepository.getSnapshot()
      const bootstrapPayload = await bootstrapPayloadPromise
      const authoritativeProjects = projectModel.active
      const currentProjectID = selectedProjectRef.current
      const currentSessionID = selectedSessionIDRef.current
      const currentProject = authoritativeProjects.find((project) => project.id === currentProjectID)
      const firstProjectID = preserveSelection && currentProject
        ? currentProject.id
        : authoritativeProjects[0]?.id ?? ''
      const firstIndex = firstProjectID ? sessionIndexRepository.getProjectReadModel(firstProjectID) : undefined
      const selectedStillVisible = Boolean(firstIndex?.active.some((summary) => !summary.parent_session_id && summary.session_id === currentSessionID))
      const firstSessionID = preserveSelection && currentProject && selectedStillVisible
        ? currentSessionID
        : firstActiveRootSessionID(firstIndex)
      setBootstrap(bootstrapPayload)
      // Bootstrap is a recovery hint, not a late authority for user
      // selection. The first project/session effect already makes a
      // deterministic initial choice; never overwrite a click that raced the
      // bootstrap request.
      if (preserveSelection || !selectedProjectRef.current) {
        setSelectedProjectID(firstProjectID)
        setSelectedSessionID(firstSessionID)
      }
    })()
    bootstrapInFlightRef.current = operation
    try {
      return await operation
    } finally {
      if (bootstrapInFlightRef.current === operation) bootstrapInFlightRef.current = null
    }
  }, [loadBootstrap, projectIndexRepository, sessionIndexRepository, setSelectedProjectID, setSelectedSessionID])

  useEffect(() => {
    void bootstrapApplication(false)
      .catch(() => setError('Application synchronization is unavailable.'))
  }, [bootstrapApplication])

  const selectedSessionRef = useRef(viewingSessionID)
  selectedSessionRef.current = viewingSessionID
  const { view: sessionContentView, historyState: sessionHistoryState, loadOlder, retry: retrySessionContent, retrying: retryingSessionContent } = useSessionContentHistory(viewingSessionID, sessionContentRepository)
  const sessionDetail = sessionContentView.session
    ? sessionMetadataForConversation(sessionContentView.session, sessionContentView.history)
    : null
  const itemsPage = sessionContentView.session ? itemsPageForConversation(sessionContentView.history) : null
  const controlRunFor = (sessionID: string) =>
    sessionID === viewingSessionID ? activeRunForConversation(sessionContentView, sessionID) : null

  // Tree selection is the authoritative navigation source. The viewing session
  // follows it so that normal navigation (tree click, bootstrap, create) keeps
  // the conversation panel in sync. The sub-panel can then override it without
  // disturbing the tree's selected highlight.
  useEffect(() => {
    if (preserveViewingForSelectedSessionIDRef.current && preserveViewingForSelectedSessionIDRef.current === selectedSessionID) {
      preserveViewingForSelectedSessionIDRef.current = ''
      return
    }
    preserveViewingForSelectedSessionIDRef.current = ''
    setViewingSessionID(selectedSessionID)
  }, [selectedSessionID])
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
    setSessionCreator({ projectID, models: [], selectedKey: '', defaultProvider: '', defaultModel: '', reasoningLevel: '', fullAccess: false, automaticCompaction: true, loading: true })
    try {
      const options = await sessionModels(projectID)
      const defaultModel = options.models.find((model) => model.provider === options.defaultProvider && model.model_profile === options.defaultModel)
      setSessionCreator((current) => current?.projectID === projectID ? {
        projectID,
        models: [...options.models],
        selectedKey: modelKey(defaultModel ?? options.models[0]),
        defaultProvider: options.defaultProvider,
        defaultModel: options.defaultModel,
        reasoningLevel: defaultModel?.default_reasoning_level ?? options.models[0]?.default_reasoning_level ?? '',
        fullAccess: current.fullAccess,
        automaticCompaction: current.automaticCompaction,
        loading: false,
      } : current)
    } catch {
      setSessionCreator(null)
      setError('Session model options are unavailable.')
    }
  }, [selectedProjectID, sessionModels])

  const createSession = async (projectID: string, model: SessionModelOption) => {
    if (!projectID || creatingSession) return
    setCreatingSession(true)
    try {
      const result = await sessionCommands.create(projectID, {
        provider: model.provider,
        modelProfile: model.model_profile,
        reasoningLevel: sessionCreator?.reasoningLevel ?? model.default_reasoning_level ?? '',
        fullAccess: sessionCreator?.fullAccess ?? false,
        automaticCompaction: sessionCreator?.automaticCompaction ?? true,
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

  const searchModelCatalog = useCallback(async (query: string): Promise<import('./commands/providerCommands').ModelCatalogModel[]> => {
    try {
      const result = await providerCommands.searchModelCatalog(query)
      return [...result.models]
    } catch (reason) {
      throw new Error(providerOperationErrorMessage(reason, 'catalog'))
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
      return await codexUsage(provider)
    } catch {
      throw new Error('Codex usage is temporarily unavailable.')
    }
  }, [codexUsage])

  const selectProject = useCallback((projectID: string) => {
    selectedProjectPositionRef.current = Math.max(0, projects.findIndex((project) => project.id === projectID))
    setSelectedProjectID(projectID)
    setSelectedSessionID(firstActiveRootSessionID(sessionIndexes[projectID]))
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
    if (activeSessions.some((session) => session.status === 'running' || isAdmissionPending(session.session_id))) return
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
  }, [isAdmissionPending, projectCommands, sessionIndexes, waitForProjectAuthority])

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
    const busyIDs = new Set(projectSessions.filter((item) => item.status === 'running' || isAdmissionPending(item.id)).map((item) => item.id))
    if (subtreeIDs.some((id) => busyIDs.has(id))) return
    const childCount = subtreeIDs.length - 1
    const childNote = childCount > 0 ? ` ${childCount} child ${childCount === 1 ? 'session' : 'sessions'} will also be archived.` : ''
    if (!window.confirm(`Archive "${sessionName(session)}"? It will be hidden from the current list.${childNote}`)) return
    try {
      await sessionCommands.archive(session.id)
      await waitForSessionAuthority(session.project_id, (next) => next.summaries.some((summary) => summary.session_id === session.id && summary.archived))
      if (selectedSessionRef.current === session.id) {
        const nextID = firstActiveRootSessionID(sessionIndexRepository.getProjectReadModel(session.project_id))
        setSelectedSessionID(nextID)
      }
    } catch (reason) { setError(sessionMutationErrorMessage(reason)) }
  }, [isAdmissionPending, sessionIndexRepository, sessionIndexes, selectedSessionRef, sessionCommands, setSelectedSessionID, waitForSessionAuthority])

  const restoreSession = useCallback(async (session: SessionNavigation) => {
    try {
      await sessionCommands.restore(session.id)
      await waitForSessionAuthority(session.project_id, (index) => index.active.some((summary) => summary.session_id === session.id))
      setSelectedProjectID(session.project_id)
      // Child sessions are selected from the floating panel and must not
      // become a left-rail selection. Keep the root selected while showing
      // the restored child in the conversation.
      const index = sessionIndexRepository.getProjectReadModel(session.project_id)
      const knownSessions = [...index.active, ...index.archived].map(navigationSession)
      const rootID = rootSessionID(knownSessions, session.id) || firstActiveRootSessionID(index) || session.id
      if (rootID !== selectedSessionIDRef.current) preserveViewingForSelectedSessionIDRef.current = rootID
      setSelectedSessionID(rootID)
      setViewingSessionID(session.parent_session_id ? session.id : rootID)
      setShowProjectForm(false)
    } catch (reason) { setError(sessionMutationErrorMessage(reason)) }
  }, [sessionCommands, sessionIndexRepository, setSelectedProjectID, setSelectedSessionID, waitForSessionAuthority])

  const deleteSession = useCallback(async (session: SessionNavigation) => {
    const index = sessionIndexes[session.project_id]
    const projectSessions = [...(index?.active.map(navigationSession) ?? []), ...(index?.archived.map(navigationSession) ?? [])]
    const subtreeIDs = [session.id, ...sessionDescendantIDs(projectSessions, session.id)]
    const initialArchivedByID = new Map(projectSessions.map((item) => [item.id, item.archived]))
    const busyIDs = new Set(projectSessions.filter((item) => item.status === 'running' || isAdmissionPending(item.id)).map((item) => item.id))
    if (subtreeIDs.some((id) => busyIDs.has(id))) return
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
      if (subtreeIDs.includes(viewingSessionIDRef.current)) {
        const nextIndex = sessionIndexRepository.getProjectReadModel(session.project_id)
        const remainingSessions = [...nextIndex.active, ...nextIndex.archived].map(navigationSession)
        const nextRootID = firstActiveRootSessionID(nextIndex)
        const parent = session.parent_session_id
          ? remainingSessions.find((item) => item.id === session.parent_session_id)
          : undefined
        const fallbackID = parent && !parent.archived ? parent.id : nextRootID
        const nextSelectedRootID = rootSessionID(remainingSessions, fallbackID) || nextRootID
        setSelectedProjectID(session.project_id)
        if (nextSelectedRootID !== selectedSessionIDRef.current) preserveViewingForSelectedSessionIDRef.current = nextSelectedRootID
        setSelectedSessionID(nextSelectedRootID)
        setViewingSessionID(fallbackID || nextSelectedRootID)
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
  }, [isAdmissionPending, sessionIndexRepository, sessionIndexes, sessionCommands, setSelectedProjectID, setSelectedSessionID, waitForSessionAuthority, viewingSessionIDRef])

  // Session Index owns navigation metadata. Session Content owns the selected
  // run projection; reconnect and replay are handled by the single sync
  // runtime, never by a page-owned lifecycle or per-run stream.

  // startNewRun handles text admission. The command result is never used to
  // manufacture a row: Session Content must publish the matching transient
  // projection before the pending UI gate is released.
  const startNewRun = async (sessionID: string, content: string, images: PastedImageAttachment[] = []): Promise<boolean> => {
    pendingAdmissionSessionsRef.current.add(sessionID)
    // Enter the visible barrier before the command leaves the page. This
    // closes the double-submit and destructive-action race during in-flight
    // command acknowledgement.
    setAwaitingRunStarted(sessionID, true)
    try {
      const uploaded = await Promise.all(images.map(async (image) => {
        const comma = image.dataURL.indexOf(',')
        if (comma < 0) throw new Error('Invalid image attachment')
        const raw = atob(image.dataURL.slice(comma + 1))
        const bytes = new Uint8Array(raw.length)
        for (let index = 0; index < raw.length; index++) bytes[index] = raw.charCodeAt(index)
        return uploadSessionImage(sessionID, new Blob([bytes], { type: image.mediaType }))
      }))
      const started = await runCommands.startRun(sessionID, content, { images: uploaded })
      if (!started.run_id || started.session_id !== sessionID) {
        throw new Error('Run admission response did not include the requested session and run_id')
      }
      pendingAdmissionSessionsRef.current.delete(sessionID)
      pendingRunIDsRef.current.set(sessionID, started.run_id)
      observeRunAdmission(sessionID, started.run_id)
      clearTurnError(sessionID)
      return true
    } catch (reason) {
      pendingAdmissionSessionsRef.current.delete(sessionID)
      pendingRunIDsRef.current.delete(sessionID)
      setAwaitingRunStarted(sessionID, false)
      setError(runMutationErrorMessage(reason))
      return false
    }
  }

  // /new creates a user root through the configured-session command facade. Capture the
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
    if (isAdmissionPending(sessionID)) return false
    const activeRun = controlRunFor(sessionID)
    if (activeRun && activeRun.status === 'running') {
      // Append to the in-flight run: the message is queued and injected into
      // the active turn at the next safe checkpoint, or sent as a follow-up
      // turn. It is never sent as a new run here. The queued state arrives via
      // the run.prompt_queue stream event; no local echo is added.
      if (images.length > 0) {
        setError('Images cannot be appended while a run is active; wait for it to finish.')
        return false
      }
      if (!content.trim()) return false
      try {
        await runCommands.appendPrompt(sessionID, activeRun.id, content)
        return true
      } catch (reason) {
        setError(runMutationErrorMessage(reason))
        return false
      }
    }
    return startNewRun(sessionID, content, images)
  }

  const continueRun = useCallback(async (): Promise<boolean> => {
    if (!viewingSessionID) return false
    const sessionID = viewingSessionID
    const activeRun = controlRunFor(sessionID)
    const detail = sessionDetail?.id === sessionID ? sessionDetail : undefined
    if (isAdmissionPending(sessionID) || activeRun?.status === 'running' || !detail || (detail.status !== 'interrupted' && detail.status !== 'failed') || !detail.interrupted_run_id || !detail.interrupted_turn_id) {
      return false
    }
    pendingAdmissionSessionsRef.current.add(sessionID)
    setAwaitingRunStarted(sessionID, true)
    try {
      const started = await runCommands.continueRun(sessionID)
      if (!started.run_id || started.session_id !== sessionID) {
        throw new Error('Run admission response did not include the requested session and run_id')
      }
      pendingAdmissionSessionsRef.current.delete(sessionID)
      pendingRunIDsRef.current.set(sessionID, started.run_id)
      observeRunAdmission(sessionID, started.run_id)
      clearTurnError(sessionID)
      return true
    } catch (reason) {
      pendingAdmissionSessionsRef.current.delete(sessionID)
      pendingRunIDsRef.current.delete(sessionID)
      setAwaitingRunStarted(sessionID, false)
      setError(runMutationErrorMessage(reason))
      return false
    }
  }, [clearTurnError, isAdmissionPending, observeRunAdmission, runCommands, sessionDetail, setAwaitingRunStarted, viewingSessionID])

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

  // Steer/move mutations only call the typed command facade; the updated queue arrives via the
  // run.prompt_queue stream event, keeping the server the single source of truth for queue order.
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
    if (!viewingSessionID || isAdmissionPending(viewingSessionID) || sessionDetail?.status === 'running' || controlRunFor(viewingSessionID)?.status === 'running') return
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
    ? [
        ...(sessionIndexes[selectedProjectID]?.active.map(navigationSession) ?? []),
        ...(sessionIndexes[selectedProjectID]?.archived.map(navigationSession) ?? []),
      ]
    : []
  const subPanelContext = viewingSessionID ? sessionSubPanelContext(selectedProjectSessions, viewingSessionID) : null
  // session_content subscription_event is the opened session's transient
  // content authority. No page-local run registry or stream is consulted.
  const selectedActiveRun = activeRunForConversation(sessionContentView, viewingSessionID)
  useEffect(() => {
    if (!viewingSessionID || !sessionContentView.turnFailure) return
    const failure = sessionContentView.turnFailure
    setTurnErrors((current) => {
      const previous = current[viewingSessionID]
      if (previous?.turnID === failure.turnID && previous.code === failure.code && previous.message === failure.message) return current
      return { ...current, [viewingSessionID]: failure }
    })
  }, [sessionContentView.turnFailure, viewingSessionID])
  useEffect(() => {
    if (!viewingSessionID) return
    const expectedRunID = pendingRunIDsRef.current.get(viewingSessionID)
    if (!expectedRunID) return
    // The barrier is deliberately a repository/domain selector, not the
    // presentation-only active row. It accepts terminal/durable evidence too.
    observeRunAdmission(viewingSessionID, expectedRunID)
  }, [observeRunAdmission, sessionContentView, viewingSessionID])
  useEffect(() => {
    if (debugSessionID && debugSessionID !== viewingSessionID) setDebugSessionID('')
  }, [debugSessionID, viewingSessionID])
  const debugSession = debugSessionID
    ? sessionDetail?.id === debugSessionID
      ? sessionDetail
      : null
    : null
  const indexedRunningSessionIDs = Object.values(sessionIndexes).flatMap((index) => index.summaries.filter((summary) => summary.status === 'running').map((summary) => summary.session_id))
  const pendingAdmissionSessionIDs = Object.keys(awaitingRunStartedBySession)
  const visibleRunningSessionIDs = new Set([
    ...indexedRunningSessionIDs,
    ...pendingAdmissionSessionIDs,
    ...(selectedActiveRun?.status === 'running' && viewingSessionID ? [viewingSessionID] : []),
    ...Object.keys(compactingSessionIDs),
  ])
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
          onRestoreSession={restoreSession}
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
			refreshError={sessionContentView.error}
			refreshing={retryingSessionContent}
			historyLoading={sessionHistoryState.loading}
			historyError={sessionHistoryState.error}
			draft={draftsBySession[viewingSessionID] ?? emptyComposerDraft}
			onDraftChange={(content) => updateDraft(viewingSessionID, content)}
			onPastedTextAdd={(pastedText) => addPastedText(viewingSessionID, pastedText)}
			onPastedTextRemove={(pastedTextID) => removePastedText(viewingSessionID, pastedTextID)}
			onPastedImageAdd={(pastedImage) => addPastedImage(viewingSessionID, pastedImage)}
			onPastedImageRemove={(pastedImageID) => removePastedImage(viewingSessionID, pastedImageID)}
			onDraftClear={() => clearDraft(viewingSessionID)}
            loadSessionImage={loadSessionImage}
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
          onAutomaticCompaction={(automaticCompaction) => setSessionCreator((current) => current ? { ...current, automaticCompaction } : current)}
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
          onSearchModelCatalog={searchModelCatalog}
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
