import { useCallback, useEffect, useState } from 'react'
import { api, streamRun } from './api'
import type { ActiveRun, ActiveRunDescriptor, Bootstrap, ImageAttachmentInput, Project, RunEvent, RunStep, Session, SessionModelOption } from './types'
import { errorMessage } from './lib/format'
import { reduceRunEvent } from './lib/runEventReducer'
import { modelKey, orderSessions, processKey, sessionName } from './lib/session'
import { emptyComposerDraft } from './components/Composer'
import type { PastedImageAttachment } from './components/Composer'
import { Conversation } from './components/Conversation'
import { EmptySession, ErrorBanner, ProjectSetup, Splash } from './components/misc'
import { ProviderManagerDialog } from './components/ProviderManagerDialog'
import type { ProviderManagerState } from './components/ProviderManagerDialog'
import { SessionModelDialog } from './components/SessionModelDialog'
import type { SessionCreatorState } from './components/SessionModelDialog'
import { WorkspaceTree } from './components/WorkspaceTree'
import { useComposerDrafts } from './hooks/useComposerDrafts'
import { useRunRegistry } from './hooks/useRunRegistry'
import { useSessionHistory } from './hooks/useSessionHistory'
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
  const [recoveredRuns, setRecoveredRuns] = useState<ActiveRunDescriptor[]>([])
	const [recentStepsByTurn, setRecentStepsByTurn] = useState<Record<string, RunStep[]>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showProjectForm, setShowProjectForm] = useState(false)
  const [sessionCreator, setSessionCreator] = useState<SessionCreatorState | null>(null)
  const [providerManager, setProviderManager] = useState<ProviderManagerState | null>(null)
  const [creatingSession, setCreatingSession] = useState(false)
  const [completionNotice, setCompletionNotice] = useState<BackgroundCompletionNotice | null>(null)
  const { draftsBySession, updateDraft, addPastedText, removePastedText, addPastedImage, removePastedImage, clearDraft } = useComposerDrafts()
  const { activeRunsBySession, activeRunsRef, runningSessionIDs, addActiveRun, updateActiveRun, queueRunEvent, flushRunEvents } = useRunRegistry()

  const loadProjects = useCallback(async () => {
    const payload = await api.projects()
    setProjects(payload.projects)
    setSessionsByProject((current) => Object.fromEntries(
      payload.projects.map((project) => [project.id, current[project.id] ?? []]),
    ))
    setArchivedSessionsByProject((current) => Object.fromEntries(
      payload.projects.map((project) => [project.id, current[project.id] ?? []]),
    ))
    setSelectedProjectID((current) => {
      if (current && payload.projects.some((project) => project.id === current)) return current
      return payload.projects[0]?.id ?? ''
    })
    return payload.projects
  }, [])

  useEffect(() => {
    let cancelled = false
    void Promise.all([api.bootstrap(), api.projects(), api.activeRuns()])
      .then(async ([bootstrapPayload, projectsPayload, activeRunsPayload]) => {
        const sessionEntries = await Promise.all(projectsPayload.projects.map(async (project) => {
          const [activePayload, archivedPayload] = await Promise.all([api.sessions(project.id), api.sessions(project.id, true)])
          return [project.id, orderSessions(activePayload.sessions), orderSessions(archivedPayload.sessions)] as const
        }))
        if (cancelled) return
        const sessionMap = Object.fromEntries(sessionEntries.map(([projectID, sessions]) => [projectID, sessions]))
        const archivedSessionMap = Object.fromEntries(sessionEntries.map(([projectID, , sessions]) => [projectID, sessions]))
        const recovered = activeRunsPayload.runs.filter((run) => projectsPayload.projects.some((project) => sessionMap[project.id]?.some((session) => session.id === run.session_id)))
        const recoveredProject = recovered.length > 0
          ? projectsPayload.projects.find((project) => sessionMap[project.id]?.some((session) => session.id === recovered[0].session_id))
          : null
        const firstProjectID = recoveredProject?.id ?? projectsPayload.projects[0]?.id ?? ''
        const firstSessionID = recovered[0]?.session_id ?? sessionMap[firstProjectID]?.[0]?.id ?? ''
        setBootstrap(bootstrapPayload)
        setProjects(projectsPayload.projects)
        setSessionsByProject(sessionMap)
        setArchivedSessionsByProject(archivedSessionMap)
        setSelectedProjectID(firstProjectID)
        setSelectedSessionID(firstSessionID)
        setRecoveredRuns(recovered)
        setShowProjectForm(projectsPayload.projects.length === 0)
      })
      .catch((reason: unknown) => setError(errorMessage(reason)))
      .finally(() => setLoading(false))
    return () => { cancelled = true }
  }, [])

  const loadSessions = useCallback(async (projectID: string, preferredSessionID = '') => {
    if (!projectID) {
      setSelectedSessionID('')
      return []
    }
    const [payload, archivedPayload] = await Promise.all([api.sessions(projectID), api.sessions(projectID, true)])
    const ordered = orderSessions(payload.sessions)
    setSessionsByProject((current) => ({ ...current, [projectID]: ordered }))
    setArchivedSessionsByProject((current) => ({ ...current, [projectID]: orderSessions(archivedPayload.sessions) }))
    if (selectedProjectRef.current === projectID) {
      setSelectedSessionID((current) => {
        const preferred = preferredSessionID || current
        if (preferred && ordered.some((session) => session.id === preferred)) return preferred
        return ordered[0]?.id ?? ''
      })
    }
    return ordered
  }, [])

  const reportError = useCallback((reason: unknown) => setError(errorMessage(reason)), [])
  const { sessionDetail, itemsPage, selectedSessionRef, refreshSession, loadOlder } =
    useSessionHistory(selectedSessionID, loadSessions, reportError)

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
    setSessionCreator({ projectID, models: [], selectedKey: '', defaultProvider: '', defaultModel: '', reasoningLevel: '', loading: true })
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
      const session = await api.createSession(projectID, model.provider, model.model_profile, sessionCreator?.reasoningLevel ?? model.default_reasoning_level ?? '')
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

  const archiveSession = useCallback(async (session: Session) => {
    if (session.status === 'running' || Boolean(activeRunsRef.current[session.id]) || !window.confirm(`Archive "${sessionName(session)}"? It will be hidden from the current list.`)) return
    try {
      await api.archiveSession(session.id)
      await loadSessions(session.project_id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }, [activeRunsRef, loadSessions])

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
    if (session.status === 'running' || Boolean(activeRunsRef.current[session.id]) || !window.confirm(`Permanently delete "${sessionName(session)}"? This action cannot be undone.`)) return
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
  }, [activeRunsRef, loadSessions])

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
      case 'run.prompt_queue':
      case 'run.prompt_appended':
      case 'agent.iteration.started':
      case 'tool.requested':
      case 'tool.started':
      case 'tool.finished':
      case 'usage.updated':
        update((run) => reduceRunEvent(run, event))
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
        setError(String(event.message ?? 'Run failed'))
        break
      case 'run.settled': {
        const settledRun = activeRunsRef.current[sessionID]
        if (!settledRun || settledRun.id !== runID) return
        if (String(event.status) === 'cancelled') {
          update((run) => ({ ...run, status: 'cancelled' }))
        }
        setRecoveredRuns((current) => current.filter((run) => run.run_id !== runID))
        const turnID = String(event.turn_id ?? settledRun.turnID ?? '')
        if (turnID && settledRun.steps.length > 0) {
          setRecentStepsByTurn((current) => ({ ...current, [processKey(sessionID, turnID)]: settledRun.steps }))
        }
        let settledSession: Session | null = null
        try {
          settledSession = await refreshSession(sessionID)
        } catch (reason) {
          setError(errorMessage(reason))
        }
        if (String(event.status) === 'committed' && selectedSessionRef.current !== sessionID) {
          setCompletionNotice({
            sessionID,
            sessionName: settledSession ? sessionName(settledSession) : `Session ${sessionID.slice(-6)}`,
          })
        }
        update(() => null)
        break
      }
    }
  }, [flushRunEvents, queueRunEvent, refreshSession, updateActiveRun])

  useEffect(() => {
    if (recoveredRuns.length === 0) return
    for (const run of recoveredRuns) {
      if (activeRunsRef.current[run.session_id]) continue
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
      void streamRun(run.run_id, (event) => handleRunEvent(run.session_id, run.run_id, event)).catch(async (reason: unknown) => {
        try {
          await refreshSession(run.session_id)
        } catch {
          // Preserve the stream error below.
        }
        updateActiveRun(run.session_id, run.run_id, () => null)
        setRecoveredRuns((current) => current.filter((item) => item.run_id !== run.run_id))
        setError(errorMessage(reason))
      })
    }
  }, [addActiveRun, handleRunEvent, recoveredRuns, refreshSession, updateActiveRun])

  const sendMessage = async (content: string, images: PastedImageAttachment[]): Promise<boolean> => {
    if (!selectedSessionID || (!content.trim() && images.length === 0)) return false
    const sessionID = selectedSessionID
    const activeRun = activeRunsRef.current[sessionID]
    if (activeRun) {
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
      void streamRun(started.run_id, (event) => handleRunEvent(sessionID, started.run_id, event)).catch((reason: unknown) => {
        const runID = activeRunsRef.current[sessionID]?.id
        if (runID) updateActiveRun(sessionID, runID, () => null)
        setError(errorMessage(reason))
      })
      return true
    } catch (reason) {
      const runID = activeRunsRef.current[sessionID]?.id
      if (runID) updateActiveRun(sessionID, runID, () => null)
      setError(errorMessage(reason))
      return false
    }
  }

  const cancelRun = async () => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.cancelRun(run.id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const removeQueuedPrompt = async (promptID: string) => {
    const run = activeRunsRef.current[selectedSessionID]
    if (!run) return
    try {
      await api.removeRunMessage(run.id, promptID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const compactSession = async () => {
    if (!selectedSessionID || sessionDetail?.status === 'running' || activeRunsRef.current[selectedSessionID]) return
    try {
      await api.compact(selectedSessionID)
      await refreshSession(selectedSessionID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const selectedProject = projects.find((project) => project.id === selectedProjectID) ?? null
  const selectedActiveRun = activeRunsBySession[selectedSessionID] ?? null
  const otherSessionsRunning = Object.keys(activeRunsBySession).some((sessionID) => sessionID !== selectedSessionID)
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
		runningSessionIDs={runningSessionIDs}
        onSelectProject={selectProject}
        onSelectSession={selectSession}
        onCreateSession={openSessionCreator}
        onManageProviders={openProviderManager}
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
            detail={sessionDetail}
            page={itemsPage}
			activeRun={selectedActiveRun}
			draft={draftsBySession[selectedSessionID] ?? emptyComposerDraft}
			onDraftChange={(content) => updateDraft(selectedSessionID, content)}
			onPastedTextAdd={(pastedText) => addPastedText(selectedSessionID, pastedText)}
			onPastedTextRemove={(pastedTextID) => removePastedText(selectedSessionID, pastedTextID)}
			onPastedImageAdd={(pastedImage) => addPastedImage(selectedSessionID, pastedImage)}
			onPastedImageRemove={(pastedImageID) => removePastedImage(selectedSessionID, pastedImageID)}
			onDraftClear={() => clearDraft(selectedSessionID)}
			otherSessionsRunning={otherSessionsRunning}
					recentStepsByTurn={recentStepsByTurn}
            onLoadOlder={loadOlder}
            onSend={(content, images) => sendMessage(content, images)}
            onCancel={() => void cancelRun()}
            onRemoveQueuedPrompt={(promptID) => void removeQueuedPrompt(promptID)}
            onCompact={() => void compactSession()}
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
    </div>
  )
}

export default App
