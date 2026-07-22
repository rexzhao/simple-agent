import { useCallback, useEffect, useRef, useState } from 'react'
import { api, streamRun } from './api'
import type { ActiveRun, Bootstrap, ItemsPage, Project, RunEvent, Session, SessionItem, ToolActivity } from './types'

function App() {
  const [bootstrap, setBootstrap] = useState<Bootstrap | null>(null)
  const [projects, setProjects] = useState<Project[]>([])
  const [sessions, setSessions] = useState<Session[]>([])
  const [selectedProjectID, setSelectedProjectID] = useState('')
  const [selectedSessionID, setSelectedSessionID] = useState('')
  const [sessionDetail, setSessionDetail] = useState<Session | null>(null)
  const [itemsPage, setItemsPage] = useState<ItemsPage | null>(null)
  const [activeRun, setActiveRun] = useState<ActiveRun | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [showProjectForm, setShowProjectForm] = useState(false)
  const selectedSessionRef = useRef('')

  selectedSessionRef.current = selectedSessionID

  const loadProjects = useCallback(async () => {
    const payload = await api.projects()
    setProjects(payload.projects)
    setSelectedProjectID((current) => {
      if (current && payload.projects.some((project) => project.id === current)) return current
      return payload.projects[0]?.id ?? ''
    })
  }, [])

  useEffect(() => {
    let cancelled = false
    void Promise.all([api.bootstrap(), api.projects()])
      .then(([bootstrapPayload, projectsPayload]) => {
        if (cancelled) return
        setBootstrap(bootstrapPayload)
        setProjects(projectsPayload.projects)
        setSelectedProjectID(projectsPayload.projects[0]?.id ?? '')
        setShowProjectForm(projectsPayload.projects.length === 0)
      })
      .catch((reason: unknown) => setError(errorMessage(reason)))
      .finally(() => setLoading(false))
    return () => { cancelled = true }
  }, [])

  const loadSessions = useCallback(async (projectID: string, preferredSessionID = '') => {
    if (!projectID) {
      setSessions([])
      setSelectedSessionID('')
      return
    }
    const payload = await api.sessions(projectID)
    const ordered = [...payload.sessions].sort((a, b) => sessionTimestamp(b) - sessionTimestamp(a))
    setSessions(ordered)
    setSelectedSessionID((current) => {
      const preferred = preferredSessionID || current
      if (preferred && ordered.some((session) => session.id === preferred)) return preferred
      return ordered[0]?.id ?? ''
    })
  }, [])

  useEffect(() => {
    setSelectedSessionID('')
    setSessionDetail(null)
    setItemsPage(null)
    if (!selectedProjectID) {
      setSessions([])
      return
    }
    void loadSessions(selectedProjectID).catch((reason: unknown) => setError(errorMessage(reason)))
  }, [selectedProjectID, loadSessions])

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
      setShowProjectForm(false)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const createSession = async () => {
    if (!selectedProjectID) return
    try {
      const session = await api.createSession(selectedProjectID)
      await loadSessions(selectedProjectID, session.id)
      setSelectedSessionID(session.id)
    } catch (reason) {
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

  const handleRunEvent = useCallback(async (event: RunEvent) => {
    switch (event.type) {
      case 'text.delta':
        setActiveRun((run) => run ? { ...run, assistantText: run.assistantText + String(event.text ?? '') } : run)
        break
      case 'reasoning.delta':
        setActiveRun((run) => run ? { ...run, reasoningText: run.reasoningText + String(event.text ?? '') } : run)
        break
      case 'tool.requested':
      case 'tool.started':
      case 'tool.finished':
        setActiveRun((run) => run ? { ...run, tools: updateTools(run.tools, event) } : run)
        break
      case 'usage.updated':
        setActiveRun((run) => run ? { ...run, totalTokens: Number(event.total_tokens ?? 0) } : run)
        break
      case 'turn.failed':
        setActiveRun((run) => run ? { ...run, status: 'failed', error: String(event.message ?? '运行失败') } : run)
        setError(String(event.message ?? '运行失败'))
        break
      case 'run.settled': {
        const sessionID = selectedSessionRef.current
        if (String(event.status) === 'cancelled') {
          setActiveRun((run) => run ? { ...run, status: 'cancelled' } : run)
        }
        if (sessionID) {
          try {
            await refreshSession(sessionID)
          } catch (reason) {
            setError(errorMessage(reason))
          }
        }
        setActiveRun(null)
        break
      }
    }
  }, [refreshSession])

  const sendMessage = async (content: string) => {
    if (!selectedSessionID || activeRun || !content.trim()) return
    try {
      const started = await api.startRun(selectedSessionID, content)
      setActiveRun({
        id: started.run_id,
        userText: content,
        assistantText: '',
        reasoningText: '',
        tools: [],
        status: 'running',
      })
      await streamRun(started.run_id, handleRunEvent)
    } catch (reason) {
      setActiveRun(null)
      setError(errorMessage(reason))
    }
  }

  const cancelRun = async () => {
    if (!activeRun) return
    try {
      await api.cancelRun(activeRun.id)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const compactSession = async () => {
    if (!selectedSessionID || activeRun) return
    try {
      await api.compact(selectedSessionID)
      await refreshSession(selectedSessionID)
    } catch (reason) {
      setError(errorMessage(reason))
    }
  }

  const selectedProject = projects.find((project) => project.id === selectedProjectID) ?? null

  if (loading) return <Splash />

  return (
    <div className="app-shell">
      <ProjectRail
        projects={projects}
        selectedID={selectedProjectID}
        onSelect={setSelectedProjectID}
        onAdd={() => setShowProjectForm(true)}
        version={bootstrap?.version ?? ''}
      />
      <SessionRail
        project={selectedProject}
        sessions={sessions}
        selectedID={selectedSessionID}
        disabled={Boolean(activeRun)}
        onSelect={setSelectedSessionID}
        onCreate={() => void createSession()}
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
            activeRun={activeRun}
            onLoadOlder={() => void loadOlder()}
            onSend={(content) => void sendMessage(content)}
            onCancel={() => void cancelRun()}
            onCompact={() => void compactSession()}
          />
        ) : selectedProject ? (
          <EmptySession onCreate={() => void createSession()} />
        ) : (
          <ProjectSetup
            suggestedRoot={bootstrap?.cwd ?? ''}
            hasProjects={false}
            onCancel={() => undefined}
            onSubmit={(root, name) => void createProject(root, name)}
          />
        )}
      </main>
    </div>
  )
}

function ProjectRail(props: {
  projects: Project[]
  selectedID: string
  version: string
  onSelect: (id: string) => void
  onAdd: () => void
}) {
  return (
    <aside className="project-rail">
      <div className="brand"><LogoIcon /><span>SAI</span></div>
      <div className="rail-label">项目</div>
      <nav className="project-list" aria-label="项目列表">
        {props.projects.map((project) => (
          <button
            key={project.id}
            className={`project-button ${project.id === props.selectedID ? 'selected' : ''}`}
            onClick={() => props.onSelect(project.id)}
            title={project.root}
          >
            <span className="project-avatar">{projectName(project).slice(0, 1).toUpperCase()}</span>
            <span className="project-button-copy">
              <strong>{projectName(project)}</strong>
              <small>{project.root}</small>
            </span>
          </button>
        ))}
      </nav>
      <div className="project-rail-footer">
        <button className="secondary-button full" onClick={props.onAdd}><PlusIcon /> 添加项目</button>
        <span className="version">v{props.version || 'dev'} · local</span>
      </div>
    </aside>
  )
}

function SessionRail(props: {
  project: Project | null
  sessions: Session[]
  selectedID: string
  disabled: boolean
  onSelect: (id: string) => void
  onCreate: () => void
}) {
  return (
    <aside className="session-rail">
      <header className="session-header">
        <div>
          <span className="eyebrow">当前项目</span>
          <h2>{props.project ? projectName(props.project) : '未选择项目'}</h2>
        </div>
        <button className="icon-button accent" disabled={!props.project || props.disabled} onClick={props.onCreate} aria-label="新建会话"><PlusIcon /></button>
      </header>
      <div className="rail-label">最近会话</div>
      <nav className="session-list" aria-label="会话列表">
        {props.sessions.map((session) => (
          <button
            key={session.id}
            disabled={props.disabled && session.id !== props.selectedID}
            className={`session-button ${session.id === props.selectedID ? 'selected' : ''}`}
            onClick={() => props.onSelect(session.id)}
          >
            <span className="session-icon"><ChatIcon /></span>
            <span className="session-copy">
              <strong>{sessionName(session)}</strong>
              <small>{relativeTime(session.last_used_at || session.updated_at)} · {session.model_id || session.model_profile}</small>
            </span>
            {session.status === 'running' && <span className="live-dot" />}
          </button>
        ))}
        {props.project && props.sessions.length === 0 && <p className="rail-empty">还没有会话</p>}
      </nav>
    </aside>
  )
}

function Conversation(props: {
  detail: Session | null
  page: ItemsPage | null
  activeRun: ActiveRun | null
  onLoadOlder: () => void
  onSend: (content: string) => void
  onCancel: () => void
  onCompact: () => void
}) {
  const bottomRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: props.activeRun ? 'smooth' : 'auto' })
  }, [props.activeRun?.assistantText, props.activeRun?.tools.length, props.page?.newest_seq])

  return (
    <div className="conversation">
      <header className="conversation-header">
        <div>
          <h1>{props.detail ? sessionName(props.detail) : '加载中…'}</h1>
          {props.detail && <p>{props.detail.provider} / {props.detail.model_id}</p>}
        </div>
        <div className="header-actions">
          <span className={`status-pill ${props.activeRun ? 'running' : ''}`}><span />{props.activeRun ? '运行中' : '就绪'}</span>
          <button className="secondary-button" disabled={!props.detail || Boolean(props.activeRun)} onClick={props.onCompact}>压缩上下文</button>
        </div>
      </header>
      <section className="messages" aria-live="polite">
        {props.page?.has_more_before && <button className="load-older" onClick={props.onLoadOlder}>加载更早消息</button>}
        {!props.page && <MessageSkeleton />}
        {props.page?.items.map((item) => <Message key={item.id} item={item} />)}
        {props.activeRun && <ActiveRunView run={props.activeRun} />}
        {props.page && props.page.items.length === 0 && !props.activeRun && (
          <div className="conversation-empty"><SparkIcon /><h3>开始一个新任务</h3><p>描述目标、问题或需要修改的代码。</p></div>
        )}
        <div ref={bottomRef} />
      </section>
      <Composer running={Boolean(props.activeRun)} onSend={props.onSend} onCancel={props.onCancel} />
    </div>
  )
}

function Message({ item }: { item: SessionItem }) {
  const role = item.message?.role
  const text = item.message?.content?.inline || item.message?.content?.preview || ''
  if (!text) return null
  return (
    <article className={`message ${role === 'user' ? 'user' : 'assistant'}`}>
      <div className="message-avatar">{role === 'user' ? '你' : <LogoIcon />}</div>
      <div className="message-content">
        <div className="message-meta"><strong>{role === 'user' ? '你' : 'SAI'}</strong><time>{formatTime(item.created_at)}</time></div>
        <div className="message-text">{text}</div>
      </div>
    </article>
  )
}

function ActiveRunView({ run }: { run: ActiveRun }) {
  return (
    <>
      <article className="message user transient">
        <div className="message-avatar">你</div>
        <div className="message-content"><div className="message-meta"><strong>你</strong><span>刚刚</span></div><div className="message-text">{run.userText}</div></div>
      </article>
      <article className="message assistant transient">
        <div className="message-avatar"><LogoIcon /></div>
        <div className="message-content">
          <div className="message-meta"><strong>SAI</strong><span className="streaming-label"><i />生成中</span></div>
          {run.reasoningText && <details className="reasoning"><summary>思考过程</summary><pre>{run.reasoningText}</pre></details>}
          {run.tools.length > 0 && <div className="tool-stack">{run.tools.map((tool) => <ToolRow key={tool.id} tool={tool} />)}</div>}
          <div className="message-text assistant-stream">{run.assistantText || <span className="cursor" />}</div>
          {run.totalTokens !== undefined && <div className="token-note">本轮 {run.totalTokens.toLocaleString()} tokens</div>}
        </div>
      </article>
    </>
  )
}

function ToolRow({ tool }: { tool: ToolActivity }) {
  return <div className={`tool-row ${tool.status}`}><ToolIcon /><span>{tool.name}</span><small>{toolStatus(tool.status)}</small></div>
}

function Composer(props: { running: boolean; onSend: (content: string) => void; onCancel: () => void }) {
  const [content, setContent] = useState('')
  const submit = () => {
    if (!content.trim() || props.running) return
    props.onSend(content.trim())
    setContent('')
  }
  return (
    <div className="composer-wrap">
      <div className="composer">
        <textarea
          value={content}
          disabled={props.running}
          rows={1}
          placeholder={props.running ? 'SAI 正在执行…' : '给 SAI 发送消息'}
          onChange={(event) => setContent(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              submit()
            }
          }}
        />
        {props.running ? (
          <button className="stop-button" onClick={props.onCancel}><StopIcon /> 停止</button>
        ) : (
          <button className="send-button" disabled={!content.trim()} onClick={submit} aria-label="发送"><SendIcon /></button>
        )}
      </div>
      <div className="composer-hint"><span>Enter 发送 · Shift+Enter 换行</span><span>本地运行</span></div>
    </div>
  )
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
        <p>项目目录中应包含 <code>.agents/sai.yaml</code>，模型、工具和指令均由该配置加载。</p>
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

function EmptySession({ onCreate }: { onCreate: () => void }) {
  return <div className="setup-screen"><div className="empty-session"><ChatIcon /><h1>还没有会话</h1><p>创建会话后即可开始与项目中的 Agent 协作。</p><button className="primary-button" onClick={onCreate}><PlusIcon /> 新建会话</button></div></div>
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

function updateTools(tools: ToolActivity[], event: RunEvent): ToolActivity[] {
	const fields = event as Record<string, unknown>
	const id = String(fields.tool_call_id ?? '')
	const name = String(fields.name ?? 'tool')
  const current = tools.find((tool) => tool.id === id)
  const status: ToolActivity['status'] = event.type === 'tool.requested'
    ? 'requested'
    : event.type === 'tool.started'
      ? 'running'
			: Boolean(fields.is_error) ? 'error' : 'finished'
  if (!current) return [...tools, { id: id || `${name}-${tools.length}`, name, status }]
  return tools.map((tool) => tool.id === id ? { ...tool, name, status } : tool)
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

const LogoIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7.2 4.4 12 2l4.8 2.4 3 4.4-.2 5.2-3.2 4.2L12 22l-4.4-3.8L4.4 14l-.2-5.2 3-4.4Z"/><path d="m8 9 4-2 4 2v5l-4 3-4-3V9Z"/></svg>
const PlusIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
const ChatIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 5h14v11H9l-4 3V5Z" /></svg>
const SendIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m4 4 17 8-17 8 3-8-3-8Zm3 8h14" /></svg>
const StopIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="1" /></svg>
const ToolIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m14 6 4-4 4 4-4 4m-6 4L4 22l-2-2 8-8m8-2-8 8" /></svg>
const FolderIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h7l2 2h9v11H3V6Z" /></svg>
const SparkIcon = () => <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m12 2 1.6 6.4L20 10l-6.4 1.6L12 18l-1.6-6.4L4 10l6.4-1.6L12 2Z" /></svg>

export default App
