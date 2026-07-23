import { useCallback, useEffect, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api, streamRun } from './api'
import type { ActiveRun, ActiveRunDescriptor, Bootstrap, CodexAuthStatus, ImageAttachmentInput, ItemsPage, Project, ProviderModelSettings, ProviderSettings, ProviderSettingsDocument, ProviderSettingsInput, RunEvent, RunStep, Session, SessionImageAttachment, SessionItem, SessionModelOption, ToolActivity } from './types'

interface SessionCreatorState {
  projectID: string
  models: SessionModelOption[]
  selectedKey: string
  defaultProvider: string
  defaultModel: string
  reasoningLevel: string
  loading: boolean
}

interface ProviderManagerState {
  document: ProviderSettingsDocument | null
  loading: boolean
}

interface PastedTextAttachment {
  id: number
  content: string
}

interface PastedImageAttachment {
  id: number
  dataURL: string
  mediaType: string
  sizeBytes: number
}

interface ComposerDraft {
  content: string
  pastedTexts: PastedTextAttachment[]
  pastedImages: PastedImageAttachment[]
}

const emptyComposerDraft: ComposerDraft = { content: '', pastedTexts: [], pastedImages: [] }
const longPasteLineLimit = 10
const longPasteCharacterLimit = 1000
const maxPastedImageAttachments = 5
const maxPastedImageBytes = 4 * 1024 * 1024
const maxPastedImageTotalBytes = 12 * 1024 * 1024
const supportedPastedImageMediaTypes = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp'])
const autoScrollThresholdPX = 160

function App() {
  const [bootstrap, setBootstrap] = useState<Bootstrap | null>(null)
  const [projects, setProjects] = useState<Project[]>([])
  const [sessionsByProject, setSessionsByProject] = useState<Record<string, Session[]>>({})
  const [selectedProjectID, setSelectedProjectID] = useState('')
  const [selectedSessionID, setSelectedSessionID] = useState('')
  const [sessionDetail, setSessionDetail] = useState<Session | null>(null)
  const [itemsPage, setItemsPage] = useState<ItemsPage | null>(null)
  const [activeRunsBySession, setActiveRunsBySession] = useState<Record<string, ActiveRun>>({})
  const [draftsBySession, setDraftsBySession] = useState<Record<string, ComposerDraft>>({})
  const [recoveredRuns, setRecoveredRuns] = useState<ActiveRunDescriptor[]>([])
	const [recentStepsByTurn, setRecentStepsByTurn] = useState<Record<string, RunStep[]>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showProjectForm, setShowProjectForm] = useState(false)
  const [sessionCreator, setSessionCreator] = useState<SessionCreatorState | null>(null)
  const [providerManager, setProviderManager] = useState<ProviderManagerState | null>(null)
  const [creatingSession, setCreatingSession] = useState(false)
  const selectedProjectRef = useRef('')
  const selectedSessionRef = useRef('')
	const activeRunsRef = useRef<Record<string, ActiveRun>>({})
  const updateDraft = useCallback((sessionID: string, content: string) => {
    setDraftsBySession((current) => {
      const draft = current[sessionID] ?? emptyComposerDraft
      if (draft.content === content) return current
      return { ...current, [sessionID]: { ...draft, content } }
    })
  }, [])
  const addPastedText = useCallback((sessionID: string, pastedText: PastedTextAttachment) => {
    setDraftsBySession((current) => {
      const draft = current[sessionID] ?? emptyComposerDraft
      return { ...current, [sessionID]: { ...draft, pastedTexts: [...draft.pastedTexts, pastedText] } }
    })
  }, [])
  const removePastedText = useCallback((sessionID: string, pastedTextID: number) => {
    setDraftsBySession((current) => {
      const draft = current[sessionID]
      if (!draft || !draft.pastedTexts.some((pastedText) => pastedText.id === pastedTextID)) return current
      return { ...current, [sessionID]: { ...draft, pastedTexts: draft.pastedTexts.filter((pastedText) => pastedText.id !== pastedTextID) } }
    })
  }, [])
  const addPastedImage = useCallback((sessionID: string, pastedImage: PastedImageAttachment) => {
    setDraftsBySession((current) => {
      const draft = current[sessionID] ?? emptyComposerDraft
      if (draft.pastedImages.length >= maxPastedImageAttachments) return current
      return { ...current, [sessionID]: { ...draft, pastedImages: [...draft.pastedImages, pastedImage] } }
    })
  }, [])
  const removePastedImage = useCallback((sessionID: string, pastedImageID: number) => {
    setDraftsBySession((current) => {
      const draft = current[sessionID]
      if (!draft || !draft.pastedImages.some((pastedImage) => pastedImage.id === pastedImageID)) return current
      return { ...current, [sessionID]: { ...draft, pastedImages: draft.pastedImages.filter((pastedImage) => pastedImage.id !== pastedImageID) } }
    })
  }, [])
  const clearDraft = useCallback((sessionID: string) => {
    setDraftsBySession((current) => {
      if (!current[sessionID]) return current
      return { ...current, [sessionID]: emptyComposerDraft }
    })
  }, [])
	const addActiveRun = useCallback((run: ActiveRun) => {
		const next = { ...activeRunsRef.current, [run.sessionID]: run }
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
		activeRunsRef.current = next
		setActiveRunsBySession(next)
	}, [])

  selectedProjectRef.current = selectedProjectID
  selectedSessionRef.current = selectedSessionID

  const loadProjects = useCallback(async () => {
    const payload = await api.projects()
    setProjects(payload.projects)
    setSessionsByProject((current) => Object.fromEntries(
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
          const payload = await api.sessions(project.id)
          return [project.id, orderSessions(payload.sessions)] as const
        }))
        if (cancelled) return
        const sessionMap = Object.fromEntries(sessionEntries)
        const recovered = activeRunsPayload.runs.filter((run) => projectsPayload.projects.some((project) => sessionMap[project.id]?.some((session) => session.id === run.session_id)))
        const recoveredProject = recovered.length > 0
          ? projectsPayload.projects.find((project) => sessionMap[project.id]?.some((session) => session.id === recovered[0].session_id))
          : null
        const firstProjectID = recoveredProject?.id ?? projectsPayload.projects[0]?.id ?? ''
        const firstSessionID = recovered[0]?.session_id ?? sessionMap[firstProjectID]?.[0]?.id ?? ''
        setBootstrap(bootstrapPayload)
        setProjects(projectsPayload.projects)
        setSessionsByProject(sessionMap)
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
    const payload = await api.sessions(projectID)
    const ordered = orderSessions(payload.sessions)
    setSessionsByProject((current) => ({ ...current, [projectID]: ordered }))
    if (selectedProjectRef.current === projectID) {
      setSelectedSessionID((current) => {
        const preferred = preferredSessionID || current
        if (preferred && ordered.some((session) => session.id === preferred)) return preferred
        return ordered[0]?.id ?? ''
      })
    }
    return ordered
  }, [])

  const refreshSession = useCallback(async (sessionID: string) => {
    if (!sessionID) return
    const [detail, page] = await Promise.all([api.session(sessionID), api.items(sessionID)])
    if (selectedSessionRef.current === sessionID) {
      setSessionDetail(detail)
      setItemsPage(page)
    }
    if (detail.project_id) await loadSessions(detail.project_id, sessionID)
  }, [loadSessions])

  useEffect(() => {
    if (!selectedSessionID) {
      setSessionDetail(null)
      setItemsPage(null)
      return
    }
    setItemsPage(null)
    void refreshSession(selectedSessionID).catch((reason: unknown) => setError(errorMessage(reason)))
  }, [selectedSessionID, refreshSession])

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

  const openSessionCreator = async (projectID = selectedProjectID) => {
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
  }

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

  const openProviderManager = async () => {
    setProviderManager({ document: null, loading: true })
    try {
      const document = await api.providerSettings()
      setProviderManager((current) => current ? { document, loading: false } : current)
    } catch (reason) {
      setProviderManager(null)
      setError(errorMessage(reason))
    }
  }

  const selectProject = (projectID: string) => {
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionsByProject[projectID]?.[0]?.id ?? '')
    setShowProjectForm(false)
  }

  const selectSession = (projectID: string, sessionID: string) => {
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionID)
    setShowProjectForm(false)
  }

  const removeSessionFromTree = (session: Session) => {
    const remaining = (sessionsByProject[session.project_id] ?? []).filter((item) => item.id !== session.id)
    setSessionsByProject((current) => ({ ...current, [session.project_id]: remaining }))
    if (selectedSessionID === session.id) {
      setSelectedProjectID(session.project_id)
      setSelectedSessionID(remaining[0]?.id ?? '')
      setSessionDetail(null)
      setItemsPage(null)
    }
  }

  const archiveSession = async (session: Session) => {
    if (session.status === 'running' || Boolean(activeRunsRef.current[session.id]) || !window.confirm(`归档“${sessionName(session)}”？归档后会从当前列表隐藏。`)) return
    try {
      await api.archiveSession(session.id)
      removeSessionFromTree(session)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const deleteSession = async (session: Session) => {
    if (session.status === 'running' || Boolean(activeRunsRef.current[session.id]) || !window.confirm(`永久删除“${sessionName(session)}”？此操作无法撤销。`)) return
    try {
      await api.archiveSession(session.id)
      await api.deleteSession(session.id)
      removeSessionFromTree(session)
    } catch (reason) {
      try {
        await loadSessions(session.project_id)
      } catch {
        // Preserve the original operation error.
      }
      setError(errorMessage(reason))
    }
  }

  const loadOlder = async () => {
    if (!selectedSessionID || !itemsPage?.has_more_before || !itemsPage.oldest_seq) return
    try {
      const older = await api.items(selectedSessionID, itemsPage.oldest_seq)
      setItemsPage({
        items: [...older.items, ...itemsPage.items],
        oldest_seq: older.oldest_seq,
        newest_seq: itemsPage.newest_seq,
        has_more_before: older.has_more_before,
        has_more_after: false,
      })
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const handleRunEvent = useCallback(async (sessionID: string, runID: string, event: RunEvent) => {
    const update = (updater: (run: ActiveRun) => ActiveRun | null) => updateActiveRun(sessionID, runID, updater)
    switch (event.type) {
      case 'turn.started':
        update((run) => ({ ...run, turnID: String(event.turn_id ?? '') }))
        break
      case 'agent.iteration.started':
        update((run) => {
          const agentIteration = Number(event.agent_iteration ?? 0)
          if (agentIteration <= 0) return run
          return {
            ...run,
            agentIteration,
            assistantText: '',
            steps: appendModelOutput(run.steps, run.assistantText, run.agentIteration),
          }
        })
        break
      case 'text.delta':
        update((run) => ({ ...run, assistantText: run.assistantText + String(event.text ?? '') }))
        break
      case 'reasoning.delta':
        update((run) => ({ ...run, steps: appendReasoning(run.steps, String(event.text ?? ''), Number(event.agent_iteration ?? run.agentIteration)) }))
        break
      case 'tool.requested':
        update((run) => ({
          ...run,
          assistantText: '',
          steps: updateToolStep(appendModelOutput(run.steps, run.assistantText, Number(event.agent_iteration ?? run.agentIteration)), event, Number(event.agent_iteration ?? run.agentIteration)),
        }))
        break
      case 'tool.started':
      case 'tool.finished':
        update((run) => ({ ...run, steps: updateToolStep(run.steps, event, Number(event.agent_iteration ?? run.agentIteration)) }))
        break
      case 'usage.updated':
        update((run) => ({
          ...run,
          inputTokens: Number(event.input_tokens ?? 0),
          totalTokens: Number(event.total_tokens ?? 0),
          cachedTokens: Number(event.cached_tokens ?? 0),
          cacheWriteTokens: Number(event.cache_write_tokens ?? 0),
          reasoningTokens: Number(event.reasoning_tokens ?? 0),
        }))
        break
      case 'run.resync_required':
        try {
          await refreshSession(sessionID)
        } catch (reason) {
          setError(errorMessage(reason))
        }
        break
      case 'turn.failed':
        update((run) => ({ ...run, status: 'failed', error: String(event.message ?? '运行失败') }))
        setError(String(event.message ?? '运行失败'))
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
        try {
          await refreshSession(sessionID)
        } catch (reason) {
          setError(errorMessage(reason))
        }
        update(() => null)
        break
      }
    }
  }, [refreshSession, updateActiveRun])

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
    if (!selectedSessionID || activeRunsRef.current[selectedSessionID] || (!content.trim() && images.length === 0)) return false
    const sessionID = selectedSessionID
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

  if (loading) return <Splash />

  return (
    <div className="app-shell">
      <WorkspaceTree
        projects={projects}
        sessionsByProject={sessionsByProject}
        selectedProjectID={selectedProjectID}
        selectedSessionID={selectedSessionID}
		runningSessionIDs={new Set(Object.keys(activeRunsBySession))}
        onSelectProject={selectProject}
        onSelectSession={selectSession}
        onCreateSession={(projectID) => void openSessionCreator(projectID)}
        onManageProviders={() => void openProviderManager()}
        onArchiveSession={(session) => void archiveSession(session)}
        onDeleteSession={(session) => void deleteSession(session)}
        onAdd={() => setShowProjectForm(true)}
        version={bootstrap?.version ?? ''}
      />
      <main className="conversation-panel">
        {error && <ErrorBanner message={error} onDismiss={() => setError('')} />}
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
            onLoadOlder={() => void loadOlder()}
            onSend={(content, images) => sendMessage(content, images)}
            onCancel={() => void cancelRun()}
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

function WorkspaceTree(props: {
  projects: Project[]
  sessionsByProject: Record<string, Session[]>
  selectedProjectID: string
  selectedSessionID: string
  runningSessionIDs: ReadonlySet<string>
  version: string
  onSelectProject: (id: string) => void
  onSelectSession: (projectID: string, sessionID: string) => void
  onCreateSession: (projectID: string) => void
  onManageProviders: () => void
  onArchiveSession: (session: Session) => void
  onDeleteSession: (session: Session) => void
  onAdd: () => void
}) {
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(new Set())
  const toggleProject = (projectID: string) => {
    setExpandedProjects((current) => {
      const next = new Set(current)
      if (next.has(projectID)) next.delete(projectID)
      else next.add(projectID)
      return next
    })
  }

  return (
    <aside className="project-rail">
      <div className="brand"><LogoIcon /><span>SAI</span><button className="brand-settings" onClick={props.onManageProviders} aria-label="管理 Server Root 配置" title="Server Root 配置"><SettingsIcon /></button></div>
      <div className="rail-label">项目与会话</div>
      <nav className="project-tree" aria-label="项目和会话树">
        {props.projects.map((project) => {
          const sessions = props.sessionsByProject[project.id] ?? []
          const expanded = expandedProjects.has(project.id)
		  const collapsedSessions = sessions.slice(0, 3)
		  const visibleSessions = expanded
			? sessions
			: sessions.filter((session) => collapsedSessions.some((item) => item.id === session.id) || props.runningSessionIDs.has(session.id))
          return (
            <section className="project-tree-group" key={project.id}>
              <div className={`project-tree-header ${project.id === props.selectedProjectID ? 'selected' : ''}`}>
                <button className="project-node" onClick={() => props.onSelectProject(project.id)} title={project.root}>
                  <span className="project-avatar">{projectName(project).slice(0, 1).toUpperCase()}</span>
                  <span className="project-button-copy">
                    <strong>{projectName(project)}</strong>
                    <small>{project.root}</small>
                  </span>
                  <span className="project-session-count">{sessions.length}</span>
                </button>
                <button
                  className="tree-icon-button"
                  onClick={() => props.onCreateSession(project.id)}
                  aria-label={`在 ${projectName(project)} 中新建会话`}
                  title="新建会话"
                ><PlusIcon /></button>
              </div>
              <div className="session-tree" role="group" aria-label={`${projectName(project)} 的会话`}>
                {visibleSessions.map((session) => (
                  <div className={`session-tree-row ${session.id === props.selectedSessionID ? 'selected' : ''}`} key={session.id}>
                    <button
                      className="session-tree-button"
                      onClick={() => props.onSelectSession(project.id, session.id)}
                    >
                      <span className="session-icon"><ChatIcon /></span>
                      <span className="session-copy">
                        <strong>{sessionName(session)}</strong>
                        <small>{relativeTime(session.last_used_at || session.updated_at)} · {session.model_id || session.model_profile}</small>
                      </span>
					  {(session.status === 'running' || props.runningSessionIDs.has(session.id)) && <span className="live-dot" />}
                    </button>
                    <div className="session-tree-actions">
                      <button disabled={session.status === 'running' || props.runningSessionIDs.has(session.id)} onClick={() => props.onArchiveSession(session)} aria-label={`归档 ${sessionName(session)}`} title="归档"><ArchiveIcon /></button>
                      <button className="danger" disabled={session.status === 'running' || props.runningSessionIDs.has(session.id)} onClick={() => props.onDeleteSession(session)} aria-label={`删除 ${sessionName(session)}`} title="删除"><TrashIcon /></button>
                    </div>
                  </div>
                ))}
                {sessions.length === 0 && <p className="tree-empty">暂无会话</p>}
                {(expanded ? sessions.length > 3 : sessions.length > visibleSessions.length) && (
                  <button className="tree-expand-button" onClick={() => toggleProject(project.id)}>
                    <ChevronIcon expanded={expanded} />
					{expanded ? '收起' : `展开另外 ${sessions.length - visibleSessions.length} 个会话`}
                  </button>
                )}
              </div>
            </section>
          )
        })}
      </nav>
      <div className="project-rail-footer">
		<button className="secondary-button full" onClick={props.onAdd}><PlusIcon /> 添加项目</button>
        <span className="version">v{props.version || 'dev'} · local</span>
      </div>
    </aside>
  )
}

function Conversation(props: {
  detail: Session | null
  page: ItemsPage | null
  activeRun: ActiveRun | null
	draft: ComposerDraft
	onDraftChange: (content: string) => void
	onPastedTextAdd: (pastedText: PastedTextAttachment) => void
	onPastedTextRemove: (pastedTextID: number) => void
	onPastedImageAdd: (pastedImage: PastedImageAttachment) => void
	onPastedImageRemove: (pastedImageID: number) => void
	onDraftClear: () => void
	otherSessionsRunning: boolean
	recentStepsByTurn: Record<string, RunStep[]>
  onLoadOlder: () => void
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
  onCompact: () => void
}) {
  const bottomRef = useRef<HTMLDivElement>(null)
	const messagesRef = useRef<HTMLElement>(null)
	const followOutputRef = useRef(true)
	useEffect(() => {
		followOutputRef.current = true
		bottomRef.current?.scrollIntoView({ behavior: 'auto' })
	}, [props.detail?.id])
  useEffect(() => {
		if (followOutputRef.current) bottomRef.current?.scrollIntoView({ behavior: 'auto' })
	}, [props.activeRun?.assistantText, props.activeRun?.steps, props.page?.newest_seq])
	const updateFollowOutput = () => {
		const messages = messagesRef.current
		if (!messages) return
		followOutputRef.current = messages.scrollHeight - messages.scrollTop - messages.clientHeight <= autoScrollThresholdPX
	}
	const visibleItems = props.activeRun?.turnID
		? (props.page?.items ?? []).filter((item) =>
			item.turn_id !== props.activeRun?.turnID || (props.activeRun?.restored && item.message?.role === 'user'))
		: (props.page?.items ?? [])

  return (
    <div className="conversation">
      <header className="conversation-header">
        <div className="conversation-heading">
          <h1>{props.detail ? sessionName(props.detail) : '加载中…'}</h1>
		  {props.detail && (
			<div className="conversation-meta">
			  <p>{props.detail.provider} / {props.detail.model_id}</p>
			  <ContextUsage context={props.detail.context} activeInputTokens={props.activeRun?.inputTokens} />
			</div>
		  )}
        </div>
        <div className="header-actions">
		  <span className={`status-pill ${props.activeRun || props.otherSessionsRunning ? 'running' : ''}`}><span />{props.activeRun ? '运行中' : props.otherSessionsRunning ? '其他会话运行中' : '就绪'}</span>
		  <button className="secondary-button" disabled={!props.detail || props.detail.status === 'running' || Boolean(props.activeRun)} onClick={props.onCompact}>压缩上下文</button>
        </div>
      </header>
      <section ref={messagesRef} className="messages" aria-live="polite" onScroll={updateFollowOutput}>
        {props.page?.has_more_before && <button className="load-older" onClick={props.onLoadOlder}>加载更早消息</button>}
        {!props.page && <MessageSkeleton />}
				{buildConversationEntries(visibleItems, props.detail?.id ?? '', props.recentStepsByTurn).map((entry) => entry.kind === 'message'
					? <Message key={entry.item.id} item={entry.item} sessionID={props.detail?.id ?? ''} />
					: <HistoricalProcess key={entry.id} entry={entry} />)}
        {props.activeRun && <ActiveRunView run={props.activeRun} />}
		{props.page && visibleItems.length === 0 && !props.activeRun && (
          <div className="conversation-empty"><SparkIcon /><h3>开始一个新任务</h3><p>描述目标、问题或需要修改的代码。</p></div>
        )}
        <div ref={bottomRef} />
      </section>
	  <Composer
		draft={props.draft}
		onContentChange={props.onDraftChange}
		onPastedTextAdd={props.onPastedTextAdd}
		onPastedTextRemove={props.onPastedTextRemove}
		onPastedImageAdd={props.onPastedImageAdd}
		onPastedImageRemove={props.onPastedImageRemove}
		onDraftClear={props.onDraftClear}
		running={Boolean(props.activeRun)}
		blocked={false}
		onSend={props.onSend}
		onCancel={props.onCancel}
	  />
    </div>
  )
}

function ContextUsage(props: { context: Session['context']; activeInputTokens?: number }) {
	const context = props.context
	const contextWindow = Number(context?.context_window ?? 0)
	if (contextWindow <= 0) return null

	const liveInputTokens = Number(props.activeInputTokens ?? 0)
	const recordedInputTokens = Number(context?.last_input_tokens ?? 0)
	const requestEstimate = Number(context?.last_request_tokens ?? 0)
	const usedTokens = liveInputTokens > 0 ? liveInputTokens : recordedInputTokens > 0 ? recordedInputTokens : requestEstimate
	const usageEstimated = liveInputTokens <= 0 && (recordedInputTokens <= 0 || context?.last_usage_source !== 'provider')
	const percent = usedTokens > 0 ? usedTokens / contextWindow * 100 : 0
	const warningThreshold = Number(context?.warning_threshold_percent ?? 80)
	const tone = percent >= 100 ? 'critical' : percent >= warningThreshold ? 'warning' : ''
	const progress = Math.min(100, Math.max(0, percent))
	const percentLabel = `${usageEstimated && usedTokens > 0 ? '约 ' : ''}${Math.round(percent)}%`
	const usageSource = usedTokens <= 0 ? '尚无使用数据' : usageEstimated ? '使用量为本地估算' : '使用量来自模型返回值'
	const windowSource = context?.context_window_source === 'configured' ? '窗口来自模型配置' : '窗口为默认估算值'
	const cacheDetails = [
		Number(context?.last_cached_tokens ?? 0) > 0 ? `缓存命中 ${Number(context?.last_cached_tokens).toLocaleString()}` : '',
		Number(context?.last_cache_write_tokens ?? 0) > 0 ? `缓存写入 ${Number(context?.last_cache_write_tokens).toLocaleString()}` : '',
		Number(context?.last_reasoning_tokens ?? 0) > 0 ? `推理 ${Number(context?.last_reasoning_tokens).toLocaleString()}` : '',
	].filter(Boolean).join('；')
	const title = `上下文：${usedTokens.toLocaleString()} / ${contextWindow.toLocaleString()} tokens（${percent.toFixed(1)}%）\n${usageSource}；${windowSource}${cacheDetails ? `\n${cacheDetails} tokens` : ''}`

	return (
		<div className={`context-usage ${tone}`} title={title}>
			<div className="context-usage-copy">
				<span>Context</span>
				<strong>{formatTokenCount(usedTokens)} / {formatTokenCount(contextWindow)}</strong>
				<small>{percentLabel}</small>
			</div>
			<div
				className="context-progress"
				role="progressbar"
				aria-label="上下文使用量"
				aria-valuemin={0}
				aria-valuemax={contextWindow}
				aria-valuenow={Math.min(usedTokens, contextWindow)}
			>
				<i style={{ width: `${progress}%` }} />
			</div>
		</div>
	)
}

function Message({ item, sessionID }: { item: SessionItem; sessionID: string }) {
  const role = item.message?.role
  const text = item.message?.content?.inline || item.message?.content?.preview || ''
  const images = item.message?.images ?? []
	const [copyStatus, setCopyStatus] = useState<'idle' | 'copied' | 'error'>('idle')
  if (!text && images.length === 0) return null
	const copyMessage = async () => {
		try {
			await copyText(text)
			setCopyStatus('copied')
			window.setTimeout(() => setCopyStatus('idle'), 1600)
		} catch {
			setCopyStatus('error')
			window.setTimeout(() => setCopyStatus('idle'), 2200)
		}
	}
  return (
    <article className={`message ${role === 'user' ? 'user' : 'assistant'}`}>
      <div className="message-avatar">{role === 'user' ? '你' : <LogoIcon />}</div>
      <div className="message-content">
        <div className="message-meta"><strong>{role === 'user' ? '你' : 'SAI'}</strong><time>{formatTime(item.created_at)}</time></div>
        {role === 'user' && text && <div className="message-text">{text}</div>}
        {role === 'user' && images.length > 0 && <StoredImageAttachments sessionID={sessionID} images={images} />}
        {role !== 'user' && text && <MarkdownMessage text={text} />}
		{role === 'assistant' && (
			<div className="message-tools" aria-label="消息操作">
				<button className="message-tool-button" onClick={() => void copyMessage()} title="复制完整输出">
					<CopyIcon />{copyStatus === 'copied' ? '已复制' : copyStatus === 'error' ? '复制失败' : '复制'}
				</button>
			</div>
		)}
      </div>
    </article>
  )
}

function StoredImageAttachments(props: { sessionID: string; images: SessionImageAttachment[] }) {
  return (
    <div className="message-image-grid" aria-label="已附加图片">
      {props.images.map((image) => <StoredImageAttachment key={image.hash} sessionID={props.sessionID} image={image} />)}
    </div>
  )
}

function StoredImageAttachment(props: { sessionID: string; image: SessionImageAttachment }) {
  const [dataURL, setDataURL] = useState('')
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    void api.sessionImage(props.sessionID, props.image.hash)
      .then(blobAsDataURL)
      .then((url) => {
        if (active) setDataURL(url)
      })
      .catch(() => {
        if (active) setFailed(true)
      })
    return () => { active = false }
  }, [props.image.hash, props.sessionID])

  if (failed) return <div className="message-image-unavailable">图片不可用</div>
  if (!dataURL) return <div className="message-image-loading">加载图片…</div>
  return <img className="message-image" src={dataURL} alt={`已附加图片（${props.image.media_type}）`} />
}

function ActiveRunView({ run }: { run: ActiveRun }) {
  return (
    <>
      {(run.userText || (run.userImages?.length ?? 0) > 0) && (
        <article className="message user transient">
          <div className="message-avatar">你</div>
          <div className="message-content">
            <div className="message-meta"><strong>你</strong><span>刚刚</span></div>
            {run.userText && <div className="message-text">{run.userText}</div>}
            {(run.userImages?.length ?? 0) > 0 && (
              <div className="message-image-grid" aria-label="已附加图片">
                {run.userImages?.map((image, index) => <img className="message-image" src={image.data_url} alt={`待发送图片 #${index + 1}`} key={`${image.data_url}-${index}`} />)}
              </div>
            )}
          </div>
        </article>
      )}
      <article className="message assistant transient">
        <div className="message-avatar"><LogoIcon /></div>
        <div className="message-content">
          <div className="message-meta"><strong>SAI</strong><span className="streaming-label"><i />生成中</span></div>
					{run.steps.length > 0 && <ProcessTimeline steps={run.steps} />}
          {run.assistantText ? <MarkdownMessage text={run.assistantText} streaming /> : <div className="message-text assistant-stream"><span className="cursor" /></div>}
			{run.totalTokens !== undefined && (
				<div className="token-note">
					本轮 {run.totalTokens.toLocaleString()} tokens
					{Boolean(run.cachedTokens) && ` · 缓存命中 ${run.cachedTokens?.toLocaleString()}`}
					{Boolean(run.cacheWriteTokens) && ` · 缓存写入 ${run.cacheWriteTokens?.toLocaleString()}`}
					{Boolean(run.reasoningTokens) && ` · 推理 ${run.reasoningTokens?.toLocaleString()}`}
				</div>
			)}
        </div>
      </article>
    </>
  )
}

const markdownComponents: Components = {
  a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

function MarkdownMessage({ text, streaming = false }: { text: string; streaming?: boolean }) {
  return (
    <div className={`message-text markdown-body ${streaming ? 'assistant-stream' : ''}`}>
      <Markdown remarkPlugins={[remarkGfm]} components={markdownComponents} skipHtml>{text}</Markdown>
    </div>
  )
}

type ConversationEntry =
	| { kind: 'message'; item: SessionItem }
	| { kind: 'process'; id: string; createdAt: string; steps: RunStep[] }

function buildConversationEntries(items: SessionItem[], sessionID: string, recentStepsByTurn: Record<string, RunStep[]>): ConversationEntry[] {
	const entries: ConversationEntry[] = []
	let steps: RunStep[] = []
	let processCreatedAt = ''
	let processTurnID = ''
	let agentIteration = 0
	const emittedRecentTurns = new Set<string>()

	const flushProcess = (turnID = processTurnID) => {
		const recentKey = turnID ? processKey(sessionID, turnID) : ''
		const recentSteps = recentKey && !emittedRecentTurns.has(recentKey) ? recentStepsByTurn[recentKey] : undefined
		const displayedSteps = recentSteps?.length ? recentSteps : steps
		if (displayedSteps.length > 0) {
			entries.push({ kind: 'process', id: `process-${sessionID}-${turnID || displayedSteps[0].id}`, createdAt: processCreatedAt, steps: displayedSteps })
		}
		if (recentKey && recentSteps?.length) emittedRecentTurns.add(recentKey)
		steps = []
		processCreatedAt = ''
		processTurnID = ''
	}

	for (const item of items) {
		const role = item.message?.role
		const text = itemText(item)
		if (role === 'user') {
			flushProcess(processTurnID)
			processTurnID = item.turn_id || ''
			agentIteration = 0
			entries.push({ kind: 'message', item })
			continue
		}
		if (role === 'assistant' && (item.message?.tool_calls?.length ?? 0) > 0) {
			agentIteration = item.agent_iteration || agentIteration + 1
			if (!processCreatedAt) processCreatedAt = item.created_at
			if (!processTurnID) processTurnID = item.turn_id || ''
			if (text) steps.push({ kind: 'output', id: `${item.id}-output`, text, iteration: agentIteration })
			for (const toolCall of item.message?.tool_calls ?? []) {
				steps.push({
					kind: 'tool',
					id: toolCall.id,
					name: toolCall.name,
					iteration: agentIteration,
					arguments: toolCall.arguments,
					status: 'requested',
				})
			}
			continue
		}
		if (role === 'tool') {
			agentIteration = item.agent_iteration || agentIteration || 1
			if (!processCreatedAt) processCreatedAt = item.created_at
			if (!processTurnID) processTurnID = item.turn_id || ''
			const toolCallID = item.message?.tool_call_id || item.id
			const index = steps.findIndex((step) => step.kind === 'tool' && step.id === toolCallID)
			const status: ToolActivity['status'] = item.message?.is_error || item.status === 'error' ? 'error' : item.status === 'pending' ? 'requested' : 'finished'
			if (index >= 0) {
				const tool = steps[index] as ToolActivity
				steps[index] = { ...tool, result: text, status }
			} else {
				steps.push({ kind: 'tool', id: toolCallID, name: 'tool', iteration: agentIteration, result: text, status })
			}
			continue
		}
		if (!processCreatedAt) processCreatedAt = item.created_at
		flushProcess(item.turn_id || processTurnID)
		if (text) entries.push({ kind: 'message', item })
	}
	flushProcess(processTurnID)
	return entries
}

function HistoricalProcess({ entry }: { entry: Extract<ConversationEntry, { kind: 'process' }> }) {
	const reasoningCount = entry.steps.filter((step) => step.kind === 'reasoning').length
	const outputCount = entry.steps.filter((step) => step.kind === 'output').length
	const toolCount = entry.steps.filter((step) => step.kind === 'tool').length
	const iterationCount = new Set(entry.steps.map((step) => step.iteration)).size
	const summary = [`${iterationCount} 轮`, reasoningCount > 0 ? `${reasoningCount} 段思考` : '', outputCount > 0 ? `${outputCount} 段中间输出` : '', toolCount > 0 ? `${toolCount} 次工具调用` : ''].filter(Boolean).join(' · ')
	return (
		<article className="message assistant process-message">
			<div className="message-avatar"><LogoIcon /></div>
			<div className="message-content">
				<details className="process-card">
					<summary><span>执行过程</span><small>{summary}</small><time>{formatTime(entry.createdAt)}</time></summary>
					<ProcessTimeline steps={entry.steps} />
				</details>
			</div>
		</article>
	)
}

function ProcessTimeline({ steps }: { steps: RunStep[] }) {
	const iterations = groupProcessSteps(steps)
	return (
		<div className="process-iterations">
			{iterations.map((iteration) => (
				<section className="process-iteration" key={iteration.number}>
					<div className="process-iteration-title">第 {iteration.number} 轮</div>
					<div className="process-timeline">
						{iteration.steps.map((step) => step.kind === 'reasoning'
							? <div className="reasoning-step" key={step.id}><span>{step.label || '思考过程'}</span><pre>{step.text}</pre></div>
							: step.kind === 'output'
								? <div className="model-output-step" key={step.id}><span>Agent 中间输出</span><pre>{step.text}</pre></div>
								: <ToolRow key={step.id} tool={step} />)}
					</div>
				</section>
			))}
		</div>
	)
}

function ToolRow({ tool }: { tool: ToolActivity }) {
	const argumentsObject = parseToolArguments(tool.arguments)
	const target = toolTarget(tool.name, argumentsObject)
	const command = tool.name === 'shell' ? stringField(argumentsObject, 'command') : ''
	const oldText = tool.name === 'edit_file' ? stringField(argumentsObject, 'old') : ''
	const newText = tool.name === 'edit_file' ? stringField(argumentsObject, 'new') : ''
	const showEditDiff = tool.name === 'edit_file' && Boolean(oldText)
	const showResult = Boolean(tool.result) && (tool.name !== 'edit_file' || tool.status === 'error')
	const showDetails = Boolean(command || showEditDiff || showResult)
	const header = <><ToolIcon /><strong>{toolDisplayName(tool.name)}</strong>{target && <code title={target}>{target}</code>}<small>{toolStatus(tool.status)}</small></>
	const details = (
		<div className="tool-details">
			{command && <div><span>命令</span><pre>{command}</pre></div>}
			{showEditDiff && <EditFileDiff path={target} oldText={oldText} newText={newText} />}
			{showResult && <div><span>{tool.name === 'edit_file' ? '错误详情' : '输出'}</span><pre>{tool.result}</pre></div>}
		</div>
	)
	if (!showDetails) {
		return <div className={`tool-row ${tool.status}`}><div className="tool-row-header">{header}</div></div>
	}
	return (
		<details className={`tool-row ${tool.status} expandable`}>
			<summary className="tool-row-header">{header}</summary>
			{details}
		</details>
  )
}

function EditFileDiff(props: { path: string; oldText: string; newText: string }) {
	const lines = editFileDiffLines(props.oldText, props.newText)
	return (
		<div className="tool-edit-diff">
			<span>变更</span>
			<pre aria-label={`编辑 ${props.path} 的差异`}><span className="diff-meta">{`--- ${props.path}\n+++ ${props.path}\n@@\n`}</span>{lines.map((line, index) => <span className={`diff-${line.kind}`} key={`${line.kind}-${index}`}>{`${diffPrefix(line.kind)}${line.text}${index < lines.length - 1 ? '\n' : ''}`}</span>)}</pre>
		</div>
	)
}

type EditDiffLine = { kind: 'context' | 'removed' | 'added'; text: string }

function editFileDiffLines(oldText: string, newText: string): EditDiffLine[] {
	const oldLines = splitEditDiffLines(oldText)
	const newLines = splitEditDiffLines(newText)
	let prefixLength = 0
	for (; prefixLength < oldLines.length && prefixLength < newLines.length && oldLines[prefixLength] === newLines[prefixLength]; prefixLength++) {
		// Shared prefix is displayed as diff context.
	}
	let suffixLength = 0
	for (; suffixLength < oldLines.length - prefixLength && suffixLength < newLines.length - prefixLength && oldLines[oldLines.length - suffixLength - 1] === newLines[newLines.length - suffixLength - 1]; suffixLength++) {
		// Shared suffix is displayed as diff context.
	}
	return [
		...oldLines.slice(0, prefixLength).map((text) => ({ kind: 'context' as const, text })),
		...oldLines.slice(prefixLength, oldLines.length - suffixLength).map((text) => ({ kind: 'removed' as const, text })),
		...newLines.slice(prefixLength, newLines.length - suffixLength).map((text) => ({ kind: 'added' as const, text })),
		...oldLines.slice(oldLines.length - suffixLength).map((text) => ({ kind: 'context' as const, text })),
	]
}

function splitEditDiffLines(text: string): string[] {
	return text === '' ? [] : text.split('\n')
}

function diffPrefix(kind: EditDiffLine['kind']): string {
	return kind === 'removed' ? '-' : kind === 'added' ? '+' : ' '
}

function Composer(props: {
  draft: ComposerDraft
  onContentChange: (content: string) => void
  onPastedTextAdd: (pastedText: PastedTextAttachment) => void
  onPastedTextRemove: (pastedTextID: number) => void
  onPastedImageAdd: (pastedImage: PastedImageAttachment) => void
  onPastedImageRemove: (pastedImageID: number) => void
  onDraftClear: () => void
  running: boolean
  blocked: boolean
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
}) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const nextPastedTextID = useRef(1)
  const nextPastedImageID = useRef(1)
  const [imageError, setImageError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const composerDisabled = props.running || props.blocked || submitting
  const resizeTextarea = useCallback(() => {
    const textarea = textareaRef.current
    if (!textarea) return
    textarea.style.height = 'auto'
    textarea.style.height = `${textarea.scrollHeight}px`
  }, [])

  useEffect(() => {
    resizeTextarea()
  }, [props.draft.content, resizeTextarea])

  useEffect(() => {
    window.addEventListener('resize', resizeTextarea)
    return () => window.removeEventListener('resize', resizeTextarea)
  }, [resizeTextarea])

  const submit = async () => {
    if (props.running || props.blocked || submitting) return
    const content = [...props.draft.pastedTexts.map((pastedText) => pastedText.content), props.draft.content]
      .filter((part) => part.trim())
      .join('\n\n')
      .trim()
    if (!content && props.draft.pastedImages.length === 0) return
    setSubmitting(true)
    try {
      if (await props.onSend(content, props.draft.pastedImages)) {
        props.onDraftClear()
        setImageError('')
      }
    } catch (reason) {
      setImageError(errorMessage(reason))
    } finally {
      setSubmitting(false)
    }
  }

  const addClipboardImages = async (files: File[]) => {
    setImageError('')
    if (props.draft.pastedImages.length+files.length > maxPastedImageAttachments) {
      setImageError(`最多可附加 ${maxPastedImageAttachments} 张图片`)
      return
    }
    let totalBytes = props.draft.pastedImages.reduce((total, image) => total + image.sizeBytes, 0)
    for (const file of files) {
      if (!supportedPastedImageMediaTypes.has(file.type)) {
        setImageError(`不支持 ${file.type || '未知'} 图片格式`)
        return
      }
      if (file.size === 0) {
        setImageError('不能附加空图片')
        return
      }
      if (file.size > maxPastedImageBytes) {
        setImageError(`单张图片不能超过 ${formatBytes(maxPastedImageBytes)}`)
        return
      }
      totalBytes += file.size
      if (totalBytes > maxPastedImageTotalBytes) {
        setImageError(`图片总大小不能超过 ${formatBytes(maxPastedImageTotalBytes)}`)
        return
      }
    }
    try {
      for (const file of files) {
        props.onPastedImageAdd({
          id: nextPastedImageID.current++,
          dataURL: await blobAsDataURL(file),
          mediaType: file.type,
          sizeBytes: file.size,
        })
      }
    } catch (reason) {
      setImageError(errorMessage(reason))
    }
  }

  const placeholder = props.running
    ? 'SAI 正在执行…'
    : props.blocked
      ? '另一个会话正在执行，可切回查看进度'
      : props.draft.pastedTexts.length > 0
        ? '在粘贴文本后补充说明'
        : '给 SAI 发送消息'

  return (
    <div className="composer-wrap">
      {props.draft.pastedTexts.length > 0 && (
        <div className="pasted-text-attachments" aria-label="待发送的粘贴文本">
          {props.draft.pastedTexts.map((pastedText, index) => (
            <div className="pasted-text-attachment" key={pastedText.id}>
              <span className="pasted-text-attachment-icon"><PaperclipIcon /></span>
              <span className="pasted-text-attachment-copy">
                <strong>粘贴文本 #{index + 1}</strong>
                <small>{pastedTextSummary(pastedText.content)}</small>
              </span>
              <button
                type="button"
                className="pasted-text-attachment-remove"
                disabled={composerDisabled}
                onClick={() => props.onPastedTextRemove(pastedText.id)}
                aria-label={`移除粘贴文本 #${index + 1}`}
                title="移除"
              >×</button>
            </div>
          ))}
        </div>
      )}
      {props.draft.pastedImages.length > 0 && (
        <div className="pasted-image-attachments" aria-label="待发送的图片">
          {props.draft.pastedImages.map((image, index) => (
            <div className="pasted-image-attachment" key={image.id}>
              <img src={image.dataURL} alt={`待发送图片 #${index + 1}`} />
              <span><strong>图片 #{index + 1}</strong><small>{image.mediaType} · {formatBytes(image.sizeBytes)}</small></span>
              <button
                type="button"
                className="pasted-text-attachment-remove"
                disabled={composerDisabled}
                onClick={() => props.onPastedImageRemove(image.id)}
                aria-label={`移除图片 #${index + 1}`}
                title="移除"
              >×</button>
            </div>
          ))}
        </div>
      )}
      {imageError && <div className="pasted-image-error" role="alert">{imageError}</div>}
      <div className="composer">
        <textarea
          ref={textareaRef}
          value={props.draft.content}
		  disabled={composerDisabled}
          rows={1}
		  placeholder={placeholder}
          onChange={(event) => props.onContentChange(event.target.value)}
          onPaste={(event) => {
            const imageFiles = Array.from(event.clipboardData.items)
              .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
              .map((item) => item.getAsFile())
              .filter((file): file is File => file !== null)
            if (imageFiles.length > 0) {
              event.preventDefault()
              void addClipboardImages(imageFiles)
              return
            }
            const pastedText = event.clipboardData.getData('text/plain').replace(/\r\n?/g, '\n')
            if (!isLongPastedText(pastedText)) return
            event.preventDefault()
            props.onPastedTextAdd({ id: nextPastedTextID.current++, content: pastedText })
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              void submit()
            }
          }}
        />
        {props.running ? (
          <button className="stop-button" onClick={props.onCancel}><StopIcon /> 停止</button>
        ) : (
		  <button className="send-button" disabled={(!props.draft.content.trim() && props.draft.pastedTexts.length === 0 && props.draft.pastedImages.length === 0) || composerDisabled} onClick={() => void submit()} aria-label="发送"><SendIcon /></button>
        )}
      </div>
      <div className="composer-hint"><span>{props.draft.pastedImages.length > 0 ? '图片将与消息一起发送' : props.draft.pastedTexts.length > 0 ? '粘贴文本会先发送，补充说明随后附加' : 'Enter 发送 · Shift+Enter 换行 · 可粘贴图片'}</span><span>本地运行</span></div>
    </div>
  )
}

function isLongPastedText(content: string): boolean {
  return content.split('\n').length > longPasteLineLimit || content.length > longPasteCharacterLimit
}

function pastedTextSummary(content: string): string {
  const lineCount = content.split('\n').length
  return lineCount > longPasteLineLimit ? `${lineCount.toLocaleString()} 行` : `${content.length.toLocaleString()} 字符`
}

function SessionModelDialog(props: {
  project?: Project
  state: SessionCreatorState
  creating: boolean
  onSelect: (key: string) => void
  onReasoningLevel: (level: string) => void
  onCancel: () => void
  onCreate: (model: SessionModelOption) => void
}) {
  const selected = props.state.models.find((model) => modelKey(model) === props.state.selectedKey)
  return (
    <div className="model-dialog-backdrop">
      <section className="model-dialog" role="dialog" aria-modal="true" aria-labelledby="model-dialog-title">
        <div className="model-dialog-header">
          <div className="model-dialog-icon"><ChatIcon /></div>
          <div>
            <span className="eyebrow">新建会话</span>
            <h2 id="model-dialog-title">选择模型</h2>
            <p>{props.project ? `${projectName(props.project)} · ${props.project.root}` : '加载 Server Root 配置'}</p>
          </div>
          <button className="model-dialog-close" disabled={props.creating} onClick={props.onCancel} aria-label="关闭">×</button>
        </div>
        <div className="model-choice-list">
          {props.state.loading ? (
            <div className="model-choice-loading"><i /><i /><i /></div>
          ) : props.state.models.length > 0 ? props.state.models.map((model) => {
            const selectedModel = modelKey(model) === props.state.selectedKey
            const isDefault = model.provider === props.state.defaultProvider && model.model_profile === props.state.defaultModel
            return (
              <button
                type="button"
                className={`model-choice ${selectedModel ? 'selected' : ''}`}
                disabled={props.creating}
                aria-pressed={selectedModel}
                onClick={() => props.onSelect(modelKey(model))}
                key={modelKey(model)}
              >
                <span className="model-choice-mark">{selectedModel ? '✓' : ''}</span>
                <span className="model-choice-copy">
                  <span><strong>{model.model_profile}</strong>{isDefault && <small>默认</small>}</span>
                  <code>{model.provider} / {model.model_id}</code>
                </span>
              </button>
            )
          }) : (
            <p className="model-choice-empty">Server Root 配置中没有可用模型。</p>
          )}
        </div>
        {selected && (selected.reasoning_levels?.length ?? 0) > 0 && (
          <label className="reasoning-choice">
            <span>推理强度</span>
            <select value={props.state.reasoningLevel || selected.default_reasoning_level || ''} disabled={props.creating} onChange={(event) => props.onReasoningLevel(event.target.value)}>
              {selected.reasoning_levels?.map((level) => <option value={level} key={level}>{reasoningLevelLabel(level)}</option>)}
            </select>
            <small>界面使用统一等级，实际请求值由该模型的 reasoning config 映射。</small>
          </label>
        )}
        <div className="model-dialog-actions">
          <button className="secondary-button" disabled={props.creating} onClick={props.onCancel}>取消</button>
          <button className="primary-button" disabled={!selected || props.creating} onClick={() => selected && props.onCreate(selected)}>
            {props.creating ? '创建中…' : '创建会话'}
          </button>
        </div>
      </section>
    </div>
  )
}

interface EditableProviderModel {
  profile: string
  id: string
  type: string
  supportsImages: boolean
  contextWindow: string
  inputLimit: string
  outputLimit: string
  parametersJSON: string
  reasoningParameter: string
  reasoningDefault: string
  reasoningLevelsJSON: string
}

interface ProviderDraft {
  existingName: string
  name: string
  baseURL: string
  apiKey: string
  keepAPIKey: boolean
  apiKeyConfigured: boolean
  authFile: string
  requestTimeout: string
  models: EditableProviderModel[]
}

function ProviderManagerDialog(props: {
  state: ProviderManagerState
  onDocument: (document: ProviderSettingsDocument) => void
  onClose: () => void
  onError: (message: string) => void
}) {
  const [draft, setDraft] = useState<ProviderDraft | null>(null)
  const [saving, setSaving] = useState(false)
  const [discovering, setDiscovering] = useState(false)
  const [discoveredModels, setDiscoveredModels] = useState<string[]>([])
  const [codexAuth, setCodexAuth] = useState<CodexAuthStatus | null>(null)
  const document = props.state.document

  const selectProvider = useCallback((provider: ProviderSettings) => {
    setDraft(providerDraft(provider))
    setCodexAuth(provider.codex_auth ?? null)
    setDiscoveredModels([])
  }, [])

  useEffect(() => {
    if (!document || draft) return
    const provider = document.providers.find((item) => item.name === document.default_provider) ?? document.providers[0]
    if (provider) selectProvider(provider)
    else setDraft(emptyProviderDraft())
  }, [document, draft, selectProvider])

  useEffect(() => {
    if (codexAuth?.status !== 'pending' || !draft?.existingName) return
    const timer = window.setInterval(() => {
      void api.codexLoginStatus(draft.existingName)
        .then(setCodexAuth)
        .catch((reason: unknown) => props.onError(errorMessage(reason)))
    }, 1500)
    return () => window.clearInterval(timer)
  }, [codexAuth?.status, draft?.existingName, props.onError])

  const save = async () => {
    if (!draft || saving) return
    setSaving(true)
    try {
      const input = providerInput(draft)
      const updated = draft.existingName
        ? await api.updateProvider(draft.existingName, input)
        : await api.createProvider(input)
      props.onDocument(updated)
      const saved = updated.providers.find((provider) => provider.name === input.name)
      if (saved) selectProvider(saved)
    } catch (reason) {
      props.onError(errorMessage(reason))
    } finally {
      setSaving(false)
    }
  }

  const discoverModels = async () => {
    if (!draft?.existingName || discovering) return
    setDiscovering(true)
    try {
      const result = await api.discoverProviderModels(draft.existingName)
      setDiscoveredModels(result.models)
    } catch (reason) {
      props.onError(errorMessage(reason))
    } finally {
      setDiscovering(false)
    }
  }

  const setDefault = async (profile: string) => {
    if (!draft?.existingName) return
    try {
      props.onDocument(await api.updateProviderDefault(draft.existingName, profile))
    } catch (reason) {
      props.onError(errorMessage(reason))
    }
  }

  const startCodexLogin = async () => {
    if (!draft?.existingName) return
    try {
      setCodexAuth(await api.startCodexLogin(draft.existingName))
    } catch (reason) {
      props.onError(errorMessage(reason))
    }
  }

  const clearCodexLogin = async () => {
    if (!draft?.existingName || !window.confirm('退出当前 Server Root 的 Codex 登录？')) return
    try {
      await api.clearCodexLogin(draft.existingName)
      setCodexAuth({ status: 'signed_out' })
    } catch (reason) {
      props.onError(errorMessage(reason))
    }
  }

  const updateModel = (index: number, patch: Partial<EditableProviderModel>) => {
    setDraft((current) => current ? { ...current, models: current.models.map((model, modelIndex) => modelIndex === index ? { ...model, ...patch } : model) } : current)
  }

  const usesCodex = draft?.models.some((model) => model.type === 'openai-codex') ?? false
  const savedCodexProvider = document?.providers.find((provider) => provider.name === draft?.existingName)?.models.some((model) => model.type === 'openai-codex') ?? false

  return (
    <div className="model-dialog-backdrop provider-dialog-backdrop">
      <section className="provider-dialog" role="dialog" aria-modal="true" aria-labelledby="provider-dialog-title">
        <header className="provider-dialog-header">
          <div>
            <span className="eyebrow">Server Root 配置</span>
            <h2 id="provider-dialog-title">Provider 与模型</h2>
            <p>{document ? `${document.server_root} · ${document.config_path}` : '读取当前 Server Root'}</p>
          </div>
          <button className="model-dialog-close" disabled={saving} onClick={props.onClose} aria-label="关闭">×</button>
        </header>
        {props.state.loading || !document || !draft ? (
          <div className="provider-loading">读取 Server Root 配置…</div>
        ) : (
          <div className="provider-dialog-body">
            <aside className="provider-list">
              {document.providers.map((provider) => (
                <button className={draft.existingName === provider.name ? 'selected' : ''} onClick={() => selectProvider(provider)} key={provider.name}>
                  <strong>{provider.name}</strong>
                  <small>{provider.models.length} 个模型</small>
                </button>
              ))}
              <button className={!draft.existingName ? 'selected add-provider' : 'add-provider'} onClick={() => { setDraft(emptyProviderDraft()); setCodexAuth(null); setDiscoveredModels([]) }}><PlusIcon /> 新增 Provider</button>
            </aside>
            <div className="provider-editor">
              <section className="settings-section">
                <div className="settings-section-title"><h3>连接配置</h3>{draft.existingName && <code>{draft.existingName}.yaml</code>}</div>
                <div className="settings-grid">
                  <label>名称<input value={draft.name} disabled={Boolean(draft.existingName)} onChange={(event) => setDraft({ ...draft, name: event.target.value })} placeholder="openai" /></label>
                  <label>请求超时<input value={draft.requestTimeout} onChange={(event) => setDraft({ ...draft, requestTimeout: event.target.value })} placeholder="60s" /></label>
                  <label className="wide">Base URL<input value={draft.baseURL} onChange={(event) => setDraft({ ...draft, baseURL: event.target.value })} placeholder="https://api.openai.com/v1" /></label>
                  <label className="wide">API Key / 环境变量<input value={draft.apiKey} onChange={(event) => setDraft({ ...draft, apiKey: event.target.value, keepAPIKey: false })} placeholder={draft.apiKeyConfigured ? '已配置；留空并保留下方选项不会修改' : '$OPENAI_API_KEY'} /></label>
                  {draft.apiKeyConfigured && <label className="checkbox-field wide"><input type="checkbox" checked={draft.keepAPIKey} onChange={(event) => setDraft({ ...draft, keepAPIKey: event.target.checked })} /> 保留当前 API Key</label>}
                </div>
              </section>

              {usesCodex && (
                <section className="settings-section codex-auth-card">
                  <div className="settings-section-title"><h3>Codex 登录态</h3><span className={`auth-status ${codexAuth?.status ?? 'signed_out'}`}>{codexAuthLabel(codexAuth?.status)}</span></div>
                  {codexAuth?.account_id && <p>账户：<code>{codexAuth.account_id}</code></p>}
                  {codexAuth?.expires_at && <p>过期时间：{new Date(codexAuth.expires_at).toLocaleString('zh-CN')}</p>}
                  {codexAuth?.status === 'pending' && <div className="device-login"><strong>{codexAuth.user_code}</strong><button className="secondary-button" onClick={() => void copyText(codexAuth.user_code ?? '')}>复制验证码</button>{codexAuth.verification_url && <a href={codexAuth.verification_url} target="_blank" rel="noreferrer">打开登录页面</a>}</div>}
                  {codexAuth?.message && <p className="settings-error">{codexAuth.message}</p>}
                  <div className="inline-actions">
                    {codexAuth?.status !== 'pending' && codexAuth?.status !== 'signed_in' && <button className="primary-button" disabled={!savedCodexProvider} onClick={() => void startCodexLogin()}>登录 Codex</button>}
                    {(codexAuth?.status === 'signed_in' || codexAuth?.status === 'expired') && <button className="secondary-button" onClick={() => void clearCodexLogin()}>退出登录</button>}
                    {!savedCodexProvider && <small>请先保存 Codex Provider，再开始登录。</small>}
                  </div>
                </section>
              )}

              <section className="settings-section">
                <div className="settings-section-title">
                  <div><h3>模型列表</h3><p>推理等级使用统一显示名，映射值可以是字符串、数字、布尔值或对象。</p></div>
                  <div className="inline-actions">
                    <button className="secondary-button compact" disabled={!draft.existingName || discovering} onClick={() => void discoverModels()}>{discovering ? '获取中…' : '从 Provider 获取'}</button>
                    <button className="secondary-button compact" onClick={() => setDraft({ ...draft, models: [...draft.models, emptyProviderModel()] })}><PlusIcon /> 添加模型</button>
                  </div>
                </div>
                <div className="provider-models">
                  {draft.models.map((model, index) => {
                    const isDefault = draft.existingName === document.default_provider && model.profile === document.default_model
                    const reasoningLevels = reasoningLevelOptions(model.reasoningLevelsJSON)
                    return <article className="provider-model-card" key={`${index}-${model.profile}`}>
                      <div className="provider-model-heading"><strong>{model.profile || `模型 ${index + 1}`}</strong><div className="inline-actions">{isDefault ? <span className="default-badge">默认</span> : <button className="plain-button" disabled={!draft.existingName || !model.profile} onClick={() => void setDefault(model.profile)}>设为默认</button>}<button className="plain-button danger" disabled={draft.models.length === 1} onClick={() => setDraft({ ...draft, models: draft.models.filter((_, modelIndex) => modelIndex !== index) })}>移除</button></div></div>
                      <div className="settings-grid model-grid">
                        {discoveredModels.length > 0 && <label className="wide model-catalog-select">从模型列表选择<select value={discoveredModels.includes(model.id) ? model.id : ''} onChange={(event) => {
                          const selectedID = event.target.value
                          if (selectedID) updateModel(index, { id: selectedID, profile: !model.profile || model.profile === model.id ? selectedID : model.profile })
                        }}><option value="">请选择模型（已获取 {discoveredModels.length} 个）</option>{discoveredModels.map((modelID) => <option value={modelID} key={modelID}>{modelID}</option>)}</select></label>}
                        <label>配置名<input value={model.profile} onChange={(event) => updateModel(index, { profile: event.target.value })} placeholder="gpt-5.5" /></label>
                        <label>模型 ID<input value={model.id} onChange={(event) => updateModel(index, { id: event.target.value })} placeholder="也可以手动输入" /></label>
                        <label>API 类型<select value={model.type || 'openai-chat'} onChange={(event) => updateModel(index, { type: event.target.value })}><option value="openai-chat">OpenAI Chat</option><option value="openai-responses">OpenAI Responses</option><option value="openai-codex">OpenAI Codex</option><option value="anthropic-messages">Anthropic Messages</option></select></label>
                        <label className="checkbox-field"><input type="checkbox" checked={model.supportsImages} onChange={(event) => updateModel(index, { supportsImages: event.target.checked })} /> 支持图片输入</label>
                        <label>Context Window<input type="number" min="0" value={model.contextWindow} onChange={(event) => updateModel(index, { contextWindow: event.target.value })} placeholder="400000" /></label>
                        <label>Input Limit<input type="number" min="0" value={model.inputLimit} onChange={(event) => updateModel(index, { inputLimit: event.target.value })} placeholder="272000" /></label>
                        <label>Output Limit<input type="number" min="0" value={model.outputLimit} onChange={(event) => updateModel(index, { outputLimit: event.target.value })} placeholder="128000" /></label>
                        <label className="wide">其他请求参数（JSON）<textarea value={model.parametersJSON} onChange={(event) => updateModel(index, { parametersJSON: event.target.value })} rows={3} spellCheck={false} /></label>
                      </div>
                      <details className="reasoning-config" open={Boolean(model.reasoningParameter)}>
                        <summary>Reasoning config {model.reasoningParameter ? <code>{model.reasoningParameter}</code> : <small>留空会写入 Pi 推荐默认</small>}</summary>
                        <div className="settings-grid model-grid">
                          <label>参数路径<input value={model.reasoningParameter} onChange={(event) => updateModel(index, { reasoningParameter: event.target.value })} placeholder="reasoning.effort" /></label>
                          <label>默认等级<select value={reasoningLevels.includes(model.reasoningDefault) ? model.reasoningDefault : ''} disabled={reasoningLevels.length === 0} onChange={(event) => updateModel(index, { reasoningDefault: event.target.value })}><option value="">{reasoningLevels.length === 0 ? '请先填写等级映射' : '不设置默认等级'}</option>{reasoningLevels.map((level) => <option value={level} key={level}>{reasoningLevelLabel(level)} ({level})</option>)}</select></label>
                          <label className="wide">等级映射（JSON）<textarea value={model.reasoningLevelsJSON} onChange={(event) => updateModel(index, { reasoningLevelsJSON: event.target.value })} rows={4} spellCheck={false} placeholder={'{"low":"low","high":"high"}'} /></label>
                        </div>
                      </details>
                    </article>
                  })}
                </div>
              </section>
            </div>
          </div>
        )}
        <footer className="model-dialog-actions"><button className="secondary-button" disabled={saving} onClick={props.onClose}>取消</button><button className="primary-button" disabled={!draft || saving} onClick={() => void save()}>{saving ? '保存中…' : '保存配置'}</button></footer>
      </section>
    </div>
  )
}

function providerDraft(provider: ProviderSettings): ProviderDraft {
  return {
    existingName: provider.name,
    name: provider.name,
    baseURL: provider.base_url,
    apiKey: provider.api_key ?? '',
    keepAPIKey: provider.api_key_configured,
    apiKeyConfigured: provider.api_key_configured,
    authFile: provider.auth_file ?? '',
    requestTimeout: provider.request_timeout ?? '',
    models: provider.models.map(editableProviderModel),
  }
}

function emptyProviderDraft(): ProviderDraft {
  return { existingName: '', name: '', baseURL: '', apiKey: '', keepAPIKey: false, apiKeyConfigured: false, authFile: '', requestTimeout: '60s', models: [emptyProviderModel()] }
}

function editableProviderModel(model: ProviderModelSettings): EditableProviderModel {
  return {
    profile: model.profile,
    id: model.id,
    type: model.type || 'openai-chat',
    supportsImages: model.input?.includes('image') ?? false,
    contextWindow: model.context_window ? String(model.context_window) : '',
    inputLimit: model.input_limit ? String(model.input_limit) : '',
    outputLimit: model.output_limit ? String(model.output_limit) : '',
    parametersJSON: prettyJSON(model.parameters ?? {}),
    reasoningParameter: model.reasoning_config?.parameter ?? '',
    reasoningDefault: model.reasoning_config?.default ?? '',
    reasoningLevelsJSON: prettyJSON(model.reasoning_config?.levels ?? {}),
  }
}

function emptyProviderModel(): EditableProviderModel {
  return { profile: '', id: '', type: 'openai-chat', supportsImages: false, contextWindow: '', inputLimit: '', outputLimit: '', parametersJSON: '{}', reasoningParameter: '', reasoningDefault: '', reasoningLevelsJSON: '{}' }
}

function providerInput(draft: ProviderDraft): ProviderSettingsInput {
  if (!draft.name.trim()) throw new Error('Provider 名称不能为空')
  if (!draft.baseURL.trim()) throw new Error('Base URL 不能为空')
  if (draft.models.length === 0) throw new Error('至少需要一个模型')
  return {
    name: draft.name.trim(),
    base_url: draft.baseURL.trim(),
    api_key: draft.apiKey.trim(),
    keep_api_key: draft.keepAPIKey,
    auth_file: draft.authFile.trim(),
    request_timeout: draft.requestTimeout.trim(),
    models: draft.models.map((model, index) => {
      if (!model.profile.trim() || !model.id.trim()) throw new Error(`模型 ${index + 1} 的配置名和模型 ID 不能为空`)
      const reasoningLevels = parseJSONRecord(model.reasoningLevelsJSON, `模型 ${model.profile} 的等级映射`)
      if (model.reasoningDefault.trim() && !(model.reasoningDefault.trim() in reasoningLevels)) throw new Error(`模型 ${model.profile} 的默认等级不在等级映射中`)
      return {
        profile: model.profile.trim(),
        id: model.id.trim(),
        type: model.type,
        input: model.supportsImages ? ['text', 'image'] : ['text'],
        context_window: model.contextWindow ? Number(model.contextWindow) : 0,
        input_limit: model.inputLimit ? Number(model.inputLimit) : 0,
        output_limit: model.outputLimit ? Number(model.outputLimit) : 0,
        parameters: parseJSONRecord(model.parametersJSON, `模型 ${model.profile} 的请求参数`),
        reasoning_config: {
          parameter: model.reasoningParameter.trim(),
          default: model.reasoningDefault.trim(),
          levels: reasoningLevels,
        },
      }
    }),
  }
}

function parseJSONRecord(value: string, label: string): Record<string, unknown> {
  try {
    const parsed: unknown = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('必须是 JSON 对象')
    return parsed as Record<string, unknown>
  } catch (reason) {
    throw new Error(`${label}格式错误：${errorMessage(reason)}`)
  }
}

function prettyJSON(value: Record<string, unknown>): string {
  return JSON.stringify(value, null, 2)
}

function reasoningLevelOptions(value: string): string[] {
  try {
    const parsed: unknown = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return []
    const keys = Object.keys(parsed)
    const canonical = ['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']
    return [...canonical.filter((level) => keys.includes(level)), ...keys.filter((level) => !canonical.includes(level)).sort()]
  } catch {
    return []
  }
}

function codexAuthLabel(status?: string): string {
  return { signed_out: '未登录', pending: '等待授权', signed_in: '已登录', expired: '已过期', error: '认证异常' }[status ?? 'signed_out'] ?? status ?? '未登录'
}

function ProjectSetup(props: { suggestedRoot: string; hasProjects: boolean; onCancel: () => void; onSubmit: (root: string, name: string) => void }) {
  const [root, setRoot] = useState(props.suggestedRoot)
  const [name, setName] = useState('')
  return (
    <div className="setup-screen">
      <div className="setup-card">
        <div className="setup-icon"><FolderIcon /></div>
        <span className="eyebrow">本地工作区</span>
        <h1>{props.hasProjects ? '添加另一个项目' : '连接你的第一个项目'}</h1>
        <p>项目目录只作为工作区；Provider、模型和运行配置由当前 Server Root 统一管理。</p>
        <label>项目目录<input value={root} onChange={(event) => setRoot(event.target.value)} placeholder="F:\work\project" autoFocus /></label>
        <label>显示名称 <small>可选</small><input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：Simple Agent" /></label>
        <div className="setup-actions">
          {props.hasProjects && <button className="secondary-button" onClick={props.onCancel}>取消</button>}
          <button className="primary-button" disabled={!root.trim()} onClick={() => props.onSubmit(root.trim(), name.trim())}>连接项目</button>
        </div>
      </div>
    </div>
  )
}

function EmptySession({ disabled, onCreate }: { disabled: boolean; onCreate: () => void }) {
  return <div className="setup-screen"><div className="empty-session"><ChatIcon /><h1>还没有会话</h1><p>创建会话后即可开始与项目中的 Agent 协作。</p><button className="primary-button" disabled={disabled} onClick={onCreate}><PlusIcon /> 新建会话</button></div></div>
}

function ErrorBanner({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return <div className="error-banner" role="alert"><span>{message}</span><button onClick={onDismiss} aria-label="关闭">×</button></div>
}

function Splash() {
  return <div className="splash"><div className="splash-logo"><LogoIcon /></div><span>SAI</span></div>
}

function MessageSkeleton() {
  return <div className="skeleton"><i /><i /><i /></div>
}

function appendReasoning(steps: RunStep[], text: string, iteration: number): RunStep[] {
	if (!text) return steps
	const normalizedIteration = normalizedAgentIteration(iteration)
	const last = steps[steps.length - 1]
	if (last?.kind === 'reasoning' && last.iteration === normalizedIteration) {
		return [...steps.slice(0, -1), { ...last, text: last.text + text }]
	}
	return [...steps, { kind: 'reasoning', id: `reasoning-${normalizedIteration}-${steps.length}`, text, iteration: normalizedIteration }]
}

function appendModelOutput(steps: RunStep[], text: string, iteration: number): RunStep[] {
	if (!text) return steps
	const normalizedIteration = normalizedAgentIteration(iteration)
	return [...steps, { kind: 'output', id: `output-${normalizedIteration}-${steps.length}`, text, iteration: normalizedIteration }]
}

function updateToolStep(steps: RunStep[], event: RunEvent, iteration: number): RunStep[] {
	const fields = event as Record<string, unknown>
	const id = String(fields.tool_call_id ?? '')
	const name = String(fields.name ?? 'tool')
	const status: ToolActivity['status'] = event.type === 'tool.requested'
		? 'requested'
		: event.type === 'tool.started'
			? 'running'
			: Boolean(fields.is_error) ? 'error' : 'finished'
	const index = steps.findIndex((step) => step.kind === 'tool' && step.id === id)
	const current = index >= 0 ? steps[index] as ToolActivity : null
	const tool: ToolActivity = {
		kind: 'tool',
		id: id || `${name}-${steps.length}`,
		name,
		iteration: current?.iteration ?? normalizedAgentIteration(iteration),
		arguments: String(fields.arguments ?? current?.arguments ?? '') || undefined,
		result: String(fields.content ?? current?.result ?? '') || undefined,
		status,
	}
	if (index < 0) return [...steps, tool]
	return steps.map((step, stepIndex) => stepIndex === index ? tool : step)
}

function groupProcessSteps(steps: RunStep[]): Array<{ number: number; steps: RunStep[] }> {
	const groups = new Map<number, RunStep[]>()
	for (const step of steps) {
		const iteration = normalizedAgentIteration(step.iteration)
		groups.set(iteration, [...(groups.get(iteration) ?? []), step])
	}
	const rank = (step: RunStep) => step.kind === 'reasoning' ? 0 : step.kind === 'output' ? 1 : 2
	return [...groups.entries()]
		.sort(([left], [right]) => left - right)
		.map(([number, turnSteps]) => ({ number, steps: [...turnSteps].sort((left, right) => rank(left) - rank(right)) }))
}

function normalizedAgentIteration(iteration: number): number {
	return Number.isFinite(iteration) && iteration > 0 ? iteration : 1
}

function itemText(item: SessionItem): string {
	return item.message?.content?.inline || item.message?.content?.preview || ''
}

function processKey(sessionID: string, turnID: string): string {
	return `${sessionID}:${turnID}`
}

function parseToolArguments(value?: string): Record<string, unknown> {
	if (!value) return {}
	try {
		const parsed: unknown = JSON.parse(value)
		return parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed as Record<string, unknown> : {}
	} catch {
		return {}
	}
}

function stringField(value: Record<string, unknown>, key: string): string {
	return typeof value[key] === 'string' ? value[key] : ''
}

function toolTarget(name: string, argumentsObject: Record<string, unknown>): string {
	const path = stringField(argumentsObject, 'path')
	if (name === 'list_files') return path || '.'
	if (name === 'glob_files') {
		const pattern = stringField(argumentsObject, 'pattern')
		return [path, pattern && `模式: ${pattern}`].filter(Boolean).join(' · ')
	}
	if (name === 'grep_files') {
		const query = stringField(argumentsObject, 'query')
		const mode = argumentsObject.regex === true ? '正则' : '文本'
		return [path, query && `${mode}: ${query}`].filter(Boolean).join(' · ')
	}
	if (path) return path
	if (name === 'shell') {
		const command = stringField(argumentsObject, 'command').replace(/\s+/g, ' ').trim()
		return command.length > 100 ? `${command.slice(0, 100)}…` : command
	}
	return stringField(argumentsObject, 'pattern') || stringField(argumentsObject, 'query')
}

function toolDisplayName(name: string): string {
	return {
		read_file: '读取文件',
		write_file: '写入文件',
		edit_file: '编辑文件',
		list_files: '列出文件',
		glob_files: '查找文件',
		grep_files: '搜索文件',
		shell: 'Shell',
	}[name] || name
}

function modelKey(model?: SessionModelOption): string {
  return model ? `${model.provider}\u0000${model.model_profile}` : ''
}

function reasoningLevelLabel(level: string): string {
  return { off: '关闭', minimal: '最少', low: '低', medium: '中', high: '高', xhigh: '超高', max: '最大' }[level] ?? level
}

function projectName(project: Project): string {
  if (project.display_name) return project.display_name
  return project.root.split(/[\\/]/).filter(Boolean).pop() || project.root
}

function sessionName(session: Session): string {
  return session.display_name || `会话 ${session.id.slice(-6)}`
}

function sessionTimestamp(session: Session): number {
  return new Date(session.last_used_at || session.updated_at || session.created_at).getTime()
}

function orderSessions(sessions: Session[]): Session[] {
  return [...sessions].sort((a, b) => sessionTimestamp(b) - sessionTimestamp(a))
}

function relativeTime(value: string): string {
  const timestamp = new Date(value).getTime()
  if (!Number.isFinite(timestamp)) return '刚刚'
  const seconds = Math.max(0, Math.round((Date.now() - timestamp) / 1000))
  if (seconds < 60) return '刚刚'
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  if (seconds < 604800) return `${Math.floor(seconds / 86400)} 天前`
  return new Date(value).toLocaleDateString('zh-CN')
}

function formatTokenCount(tokens: number): string {
	if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`
	if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1).replace(/\.0$/, '')}K`
	return Math.max(0, tokens).toLocaleString()
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : ''
}

function toolStatus(status: ToolActivity['status']): string {
  return { requested: '等待', running: '执行中', finished: '完成', error: '失败' }[status]
}

function errorMessage(reason: unknown): string {
  return reason instanceof Error ? reason.message : '发生未知错误'
}

function blobAsDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error ?? new Error('could not read image attachment'))
    reader.onload = () => typeof reader.result === 'string' ? resolve(reader.result) : reject(new Error('could not read image attachment'))
    reader.readAsDataURL(blob)
  })
}

function formatBytes(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1).replace(/\.0$/, '')} MiB`
  return `${Math.max(0, Math.ceil(bytes / 1024))} KiB`
}

async function copyText(text: string): Promise<void> {
	if (navigator.clipboard?.writeText) {
		await navigator.clipboard.writeText(text)
		return
	}
	const textarea = document.createElement('textarea')
	textarea.value = text
	textarea.style.position = 'fixed'
	textarea.style.opacity = '0'
	document.body.appendChild(textarea)
	textarea.select()
	try {
		if (!document.execCommand('copy')) throw new Error('copy command was rejected')
	} finally {
		textarea.remove()
	}
}

const LogoIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7.2 4.4 12 2l4.8 2.4 3 4.4-.2 5.2-3.2 4.2L12 22l-4.4-3.8L4.4 14l-.2-5.2 3-4.4Z"/><path d="m8 9 4-2 4 2v5l-4 3-4-3V9Z"/></svg>
const PlusIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
const ChatIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5h14v11H9l-4 3V5Z" /></svg>
const ArchiveIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16v13H4V7Zm-1-4h18v4H3V3Zm6 8h6" /></svg>
const TrashIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M9 7V4h6v3m3 0-1 14H7L6 7m4 4v6m4-6v6" /></svg>
const CopyIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="8" y="8" width="11" height="11" /><path d="M16 8V5H5v11h3" /></svg>
const PaperclipIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m8.5 12.5 6.8-6.8a3 3 0 1 1 4.2 4.2l-8.7 8.7a5 5 0 0 1-7.1-7.1l8.2-8.2" /></svg>
const ChevronIcon = ({ expanded }: { expanded: boolean }) => <svg className={expanded ? 'expanded' : ''} viewBox="0 0 24 24" aria-hidden="true"><path d="m8 10 4 4 4-4" /></svg>
const SendIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m4 4 17 8-17 8 3-8-3-8Zm3 8h14" /></svg>
const StopIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="1" /></svg>
const ToolIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m14 6 4-4 4 4-4 4m-6 4L4 22l-2-2 8-8m8-2-8 8" /></svg>
const FolderIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h7l2 2h9v11H3V6Z" /></svg>
const SparkIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m12 2 1.6 6.4L20 10l-6.4 1.6L12 18l-1.6-6.4L4 10l6.4-1.6L12 2Z" /></svg>
const SettingsIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3" /><path d="M19 13.5v-3l-2-.7-.7-1.7.9-1.9-2.1-2.1-1.9.9-1.7-.7-.7-2h-3l-.7 2-1.7.7-1.9-.9-2.1 2.1.9 1.9-.7 1.7-2 .7v3l2 .7.7 1.7-.9 1.9 2.1 2.1 1.9-.9 1.7.7.7 2h3l.7-2 1.7-.7 1.9.9 2.1-2.1-.9-1.9.7-1.7 2-.7Z" /></svg>

export default App
