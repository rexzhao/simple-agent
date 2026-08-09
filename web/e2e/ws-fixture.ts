import type { Page, Route, WebSocketRoute } from '@playwright/test'

export type WireProject = {
  id: string
  root: string
  display_name: string
  archived: boolean
  created_at: string
  updated_at: string
}

export type WireItem = Record<string, unknown>
export type WireSession = Record<string, unknown> & {
  id: string
  project_id: string
  display_name: string
  status: string
  last_seq?: number
  archived?: boolean
}

type ContentState = {
  session: WireSession
  items: WireItem[]
  historyBefore?: WireItem[]
  hasMoreBefore?: boolean
  activeRun?: ActiveRun | null
  compaction?: Record<string, unknown>
}

type ActiveRun = {
  run_id: string
  session_id: string
  turn_id?: string
  started_at: string
  status: 'running'
  recoverable: boolean
  run_epoch: string
  run_cursor: string
  replay_available: boolean
  recovery_required: boolean
}

type Command = {
  name: string
  request_id: string
  arguments: Record<string, unknown>
}

type TypedEvent = Record<string, unknown> & { type: string }

export type SyncMockOptions = {
  projects?: WireProject[]
  sessions?: WireSession[]
  contents?: Record<string, Partial<ContentState>>
  activeRuns?: Array<Record<string, unknown>> | (() => Array<Record<string, unknown>>)
  bootstrap?: { cwd?: string; server_root?: string; config_path?: string }
  onCommand?: (server: SyncMockServer, command: Command) => void | Promise<void>
}

type Resource = { type: string; id: string }
type Subscription = {
  resource: Resource
  subscriptionID: string
  sequence: number
  socketID: number
  socket: WebSocketRoute
  active: boolean
}
type MockSocket = {
  id: number
  route: WebSocketRoute
  closed: boolean
  subscriptions: Map<string, Subscription>
}
type ChangePublication = {
  resource: Resource
  stream_epoch: string
  sequence: string
  previous_sequence: string
  resource_revision: string
  operations: Record<string, unknown>[]
}

const timestamp = '2026-01-01T00:00:00Z'

function json(route: Route, body: unknown, status = 200) {
  return route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

function envelope(type: string, id: string, payload: unknown): string {
  return JSON.stringify({ version: 1, type, id, payload })
}

function wireItem(item: WireItem, index: number): Record<string, unknown> {
  const id = String(item.id ?? `item-${index + 1}`)
  const turnID = String(item.turn_id ?? 'turn-fixture')
  const iteration = Number(item.agent_iteration ?? 1)
  const visibility = item.visibility === 'normal' ? 'visible' : String(item.visibility ?? 'visible')
  const audience = String(item.audience ?? 'user')
  return {
    key: { turn_id: turnID, agent_iteration: iteration, item_id: id },
    seq: Number(item.seq ?? index + 1),
    created_at: String(item.created_at ?? timestamp),
    kind: String(item.kind ?? 'message'),
    visibility,
    audience,
    ...(item.status === undefined ? {} : { status: item.status }),
    ...(item.message === undefined ? {} : { message: item.message }),
  }
}

export function contentItem(item: WireItem, index = 0): Record<string, unknown> {
  return wireItem(item, index)
}

function pageItems(items: WireItem[]): Record<string, unknown> {
  return items.map((item, index) => wireItem(item, index))
}

function historyWindow(items: WireItem[], hasMoreBefore = false): Record<string, unknown> {
  const normalized = pageItems(items)
  const oldest = normalized.length ? String(normalized[0].seq) : undefined
  const newest = normalized.length ? String(normalized[normalized.length - 1].seq) : undefined
  return {
    items: normalized,
    descriptor: {
      limit: Math.max(200, normalized.length),
      ...(oldest ? { oldest_item_seq: oldest } : {}),
      ...(newest ? { newest_item_seq: newest } : {}),
      align_turn: false,
      visible_only: true,
      has_more_before: hasMoreBefore,
      has_more_after: false,
    },
  }
}

function sessionSummary(session: WireSession, revision: string, runID: string | null = null): Record<string, unknown> {
  const status = session.status === 'idle' && runID ? 'running' : String(session.status ?? 'idle')
  return {
    session_id: session.id,
    project_id: session.project_id,
    parent_session_id: session.parent_session_id ?? null,
    display_name: session.display_name,
    archived: session.archived ?? false,
    status: status === 'running' ? 'running' : status === 'failed' ? 'failed' : status === 'interrupted' ? 'interrupted' : 'idle',
    run_id: runID ?? null,
    resource_revision: revision,
    updated_at: String(session.updated_at ?? timestamp),
    has_unread_result: Boolean(session.has_unread_result ?? false),
  }
}

function fullMetadata(session: WireSession, activeRun: ActiveRun | null): Record<string, unknown> {
  const running = activeRun !== null
  const status = running ? 'running' : String(session.status ?? 'idle')
  return {
    id: session.id,
    version: Number(session.version ?? 2),
    created_at: String(session.created_at ?? timestamp),
    updated_at: String(session.updated_at ?? timestamp),
    ...(session.display_name === undefined ? {} : { display_name: session.display_name }),
    ...(session.created_by === undefined ? {} : { created_by: session.created_by }),
    archived: Boolean(session.archived ?? false),
    last_used_at: String(session.last_used_at ?? timestamp),
    ...(running ? { current_run_id: activeRun.run_id, running_run_id: activeRun.run_id, running_turn_id: activeRun.turn_id } : {}),
    ...(session.interrupted_run_id === undefined ? {} : { interrupted_run_id: session.interrupted_run_id }),
    ...(session.interrupted_turn_id === undefined ? {} : { interrupted_turn_id: session.interrupted_turn_id }),
    ...(session.latest_run_id === undefined ? {} : { latest_run_id: session.latest_run_id }),
    ...(session.last_run_id === undefined ? {} : { last_run_id: session.last_run_id }),
    ...(session.last_run_status === undefined ? {} : { last_run_status: session.last_run_status }),
    has_unread_result: Boolean(session.has_unread_result ?? false),
    provider: String(session.provider ?? 'fake'),
    model_profile: String(session.model_profile ?? 'fast'),
    model_id: String(session.model_id ?? 'fake-model'),
    project_id: session.project_id,
    cwd: String(session.cwd ?? session.created_cwd ?? '/workspace'),
    created_cwd: String(session.created_cwd ?? '/workspace'),
    config_path: String(session.config_path ?? '/workspace/sai.yaml'),
    reasoning_level: String(session.reasoning_level ?? 'high'),
    status,
    show_reasoning: Boolean(session.show_reasoning ?? false),
    full_access: Boolean(session.full_access ?? false),
    debug: { request_bodies: Boolean((session.debug as Record<string, unknown> | undefined)?.request_bodies) },
    context: session.context ?? {},
    save_tool_results: Boolean(session.save_tool_results ?? true),
    ...(session.active_history === undefined ? {} : { active_history: session.active_history }),
  }
}

export class SyncMockServer {
  readonly page: Page
  readonly projects: WireProject[]
  readonly sessions = new Map<string, WireSession>()
  readonly contents = new Map<string, ContentState>()
  private readonly activeSubscriptionsByID = new Map<string, Subscription>()
  readonly snapshotCounts = new Map<string, number>()
  readonly commands: Command[] = []
  private readonly sockets = new Map<number, MockSocket>()
  private readonly retiredSubscriptions = new Map<string, Subscription>()
  private readonly resourceSequences = new Map<string, number>()
  private readonly resourceRevisions = new Map<string, number>()
  private readonly changeHistory = new Map<string, ChangePublication[]>()
  private readonly heldChanges = new Set<string>()
  private readonly pendingChanges = new Map<string, ChangePublication[]>()
  private readonly heldSnapshots = new Set<string>()
  private readonly pendingSnapshotReleases = new Map<string, Array<() => void>>()
  private readonly runCursors = new Map<string, number>()
  private readonly promptQueues = new Map<string, Array<{ id: string; content: string; steer: boolean }>>()
  private readonly options: SyncMockOptions
  private messageID = 0
  private socketID = 0

  constructor(page: Page, options: SyncMockOptions = {}) {
    this.page = page
    this.options = options
    // Fixtures are installed once per browser context, but the spec-level
    // wire objects are shared constants. Keep command-side mutations local to
    // this bounded mock so one scenario cannot leak project/session state into
    // the next scenario in a worker.
    this.projects = (options.projects ?? []).map((project) => ({ ...project }))
    for (const session of options.sessions ?? []) this.addSessionState(session, options.contents?.[session.id])
  }

  async install(): Promise<this> {
    await this.page.addInitScript(() => window.sessionStorage.setItem('sai-capability-token', 'e2e'))
    await this.page.route('**/api/**', async (route) => {
      const url = new URL(route.request().url())
      if (url.pathname === '/api/ws-ticket' && route.request().method() === 'POST') {
        return json(route, { ticket: 'e2e-ticket', expires_at: '2099-01-01T00:00:00Z' })
      }
      if (url.pathname === '/api/bootstrap') {
        const bootstrap = this.options.bootstrap ?? {}
        return json(route, { version: 'e2e', cwd: bootstrap.cwd ?? '/workspace', server_root: bootstrap.server_root ?? '/server-root', config_path: bootstrap.config_path ?? '/server-root/sai.yaml' })
      }
      if (url.pathname.includes('/images/')) return json(route, { error: { code: 'not_mocked', message: 'image not mocked' } }, 404)
      return json(route, { error: { code: 'not_mocked', message: 'typed sync fixture handles this operation' } }, 404)
    })
    await this.page.routeWebSocket('**/api/ws*', (socket) => this.attach(socket))
    return this
  }

  private addSessionState(session: WireSession, partial: Partial<ContentState> = {}): void {
    const copy = { ...session }
    this.sessions.set(copy.id, copy)
    this.contents.set(copy.id, {
      session: copy,
      items: [...(partial.items ?? [])],
      historyBefore: [...(partial.historyBefore ?? [])],
      hasMoreBefore: partial.hasMoreBefore ?? false,
      activeRun: partial.activeRun ?? null,
      compaction: partial.compaction ?? { checkpoints: [], truncated: false },
    })
  }

  addSession(session: WireSession, partial: Partial<ContentState> = {}): void {
    this.addSessionState(session, partial)
    this.publishIndexChange(session.project_id, [{ op: 'upsert', key: session.id, value: sessionSummary(session, '0') }])
  }

  addProject(project: WireProject): void {
    if (!this.projects.some((candidate) => candidate.id === project.id)) this.projects.push(project)
    this.publishChange({ type: 'project_index', id: 'server' }, [{ op: 'upsert', key: project.id, value: project }])
  }

  updateSession(sessionID: string, patch: Record<string, unknown>, runID: string | null = null): void {
    const session = this.sessions.get(sessionID)
    if (!session) return
    Object.assign(session, patch)
    const content = this.contents.get(sessionID)
    if (content) content.session = session
    this.publishIndexChange(session.project_id, [{ op: 'upsert', key: sessionID, value: sessionSummary(session, this.currentRunID(sessionID, runID)) }])
    if (content) this.publishContentChange(sessionID, [{ op: 'metadata.replace', metadata: fullMetadata(session, content.activeRun ?? null) }])
  }

  updateSessionIndexOnly(sessionID: string, patch: Record<string, unknown>): void {
    const session = this.sessions.get(sessionID)
    if (!session) return
    Object.assign(session, patch)
    this.publishIndexChange(session.project_id, [{ op: 'upsert', key: sessionID, value: sessionSummary(session, this.nextIndexRevision(session.project_id), this.currentRunID(sessionID)) }])
  }

  private currentRunID(sessionID: string, explicit?: string | null): string | null {
    if (explicit !== undefined) return explicit
    return this.contents.get(sessionID)?.activeRun?.run_id ?? null
  }

  private resourceKey(resource: { type: string; id: string }): string {
    return `${resource.type}:${resource.id}`
  }

  private nextRevision(sessionID: string): string {
    const key = this.resourceKey({ type: 'session_content', id: sessionID })
    return String((this.resourceRevisions.get(key) ?? 0) + 1)
  }

  private nextIndexRevision(projectID: string): string {
    return String((this.resourceRevisions.get(this.resourceKey({ type: 'session_index', id: projectID })) ?? 0) + 1)
  }

  private resourceRevision(resource: Resource): string {
    return String(this.resourceRevisions.get(this.resourceKey(resource)) ?? 0)
  }

  private nextSequence(resource: { type: string; id: string }): string {
    const key = this.resourceKey(resource)
    const value = (this.resourceSequences.get(key) ?? 0) + 1
    this.resourceSequences.set(key, value)
    return String(value)
  }

  private attach(socket: WebSocketRoute): void {
    const state: MockSocket = {
      id: ++this.socketID,
      route: socket,
      closed: false,
      subscriptions: new Map(),
    }
    this.sockets.set(state.id, state)
    socket.onClose(() => {
      state.closed = true
      for (const subscription of state.subscriptions.values()) {
        subscription.active = false
        this.retiredSubscriptions.set(subscription.subscriptionID, subscription)
        this.removeActiveSubscription(subscription)
      }
      state.subscriptions.clear()
      this.sockets.delete(state.id)
    })
    socket.onMessage((raw) => {
      let message: Record<string, any>
      try { message = JSON.parse(String(raw)) as Record<string, any> } catch { return }
      const type = String(message.type ?? '')
      if (type === 'hello') {
        socket.send(envelope('welcome', this.nextID('welcome'), {
          selected_version: 1,
          connection_id: this.nextID('connection'),
          server_epoch: 'e2e-epoch',
          heartbeat_interval_ms: 60_000,
          max_message_bytes: 262_144,
        }))
      } else if (type === 'subscribe') {
        this.subscribe(state, message)
      } else if (type === 'unsubscribe') {
        const id = String(message.payload?.subscription_id ?? '')
        const subscription = state.subscriptions.get(id)
        if (subscription) {
          subscription.active = false
          state.subscriptions.delete(id)
          this.retiredSubscriptions.set(id, subscription)
          this.removeActiveSubscription(subscription)
        }
        socket.send(envelope('unsubscribed', this.nextID('unsubscribed'), { subscription_id: id }))
      } else if (type === 'ack' || type === 'ping' || type === 'pong') {
        if (type === 'ping') socket.send(envelope('pong', this.nextID('pong'), {}))
      } else if (type === 'command') {
        void this.command(socket, message)
      }
    })
  }

  private subscribe(socket: MockSocket, message: Record<string, any>): void {
    const payload = message.payload ?? {}
    const resource: Resource = { type: String(payload.resource?.type ?? ''), id: String(payload.resource?.id ?? '') }
    const subscriptionID = String(payload.subscription_id ?? '')
    const existing = this.activeSubscription(subscriptionID)
    if (existing) {
      existing.active = false
      existing.socketID !== socket.id && existing.socket.subscriptions.delete(subscriptionID)
      this.removeActiveSubscription(existing)
      this.retiredSubscriptions.set(subscriptionID, existing)
    }
    const sequence = Number(this.resourceSequences.get(this.resourceKey(resource)) ?? 0)
    const subscription: Subscription = { resource, subscriptionID, sequence, socketID: socket.id, socket: socket.route, active: true }
    this.activeSubscriptionsByID.set(subscriptionID, subscription)
    socket.subscriptions.set(subscriptionID, subscription)
    const resume = payload.resume as { stream_epoch?: string; sequence?: string } | undefined
    socket.route.send(envelope('subscribed', this.nextID('subscribed'), { subscription_id: subscriptionID, resource, stream_epoch: 'e2e-epoch', sequence: String(sequence) }))
    const snapshotKey = this.resourceKey(resource)
    if (resume?.stream_epoch === 'e2e-epoch' && resume.sequence !== undefined && Number(resume.sequence) <= sequence) {
      const replay = (this.changeHistory.get(snapshotKey) ?? []).filter((change) => Number(change.sequence) > Number(resume.sequence) && Number(change.sequence) <= sequence)
      if (replay.length === sequence - Number(resume.sequence)) {
        for (const change of replay) this.deliverChange(subscription, change)
        return
      }
    }
    void this.sendSnapshot(subscription)
  }

  private activeSubscription(subscriptionID: string): Subscription | undefined {
    const subscription = this.activeSubscriptionsByID.get(subscriptionID)
    return subscription?.active ? subscription : undefined
  }

  private activeSubscriptions(): Subscription[] {
    return [...this.activeSubscriptionsByID.values()].filter((subscription) => subscription.active && !this.sockets.get(subscription.socketID)?.closed)
  }

  private removeActiveSubscription(subscription: Subscription): void {
    if (this.activeSubscriptionsByID.get(subscription.subscriptionID) === subscription) {
      this.activeSubscriptionsByID.delete(subscription.subscriptionID)
    }
  }

  private async sendSnapshot(subscription: Subscription): Promise<void> {
    const key = this.resourceKey(subscription.resource)
    if (this.heldSnapshots.has(key)) {
      await new Promise<void>((resolve) => {
        const waiters = this.pendingSnapshotReleases.get(key) ?? []
        waiters.push(resolve)
        this.pendingSnapshotReleases.set(key, waiters)
      })
    }
    if (!subscription.active || this.sockets.get(subscription.socketID)?.closed) return
    const sequence = Number(this.resourceSequences.get(key) ?? 0)
    subscription.sequence = sequence
    this.snapshotCounts.set(key, (this.snapshotCounts.get(key) ?? 0) + 1)
    subscription.socket.send(envelope('snapshot', this.nextID('snapshot'), {
      subscription_id: subscription.subscriptionID,
      resource: subscription.resource,
      stream_epoch: 'e2e-epoch',
      sequence: String(sequence),
      resource_revision: this.resourceRevision(subscription.resource),
      content: { inline: this.snapshot(subscription.resource) },
    }))
  }

  private snapshot(resource: { type: string; id: string }): Record<string, unknown> {
    if (resource.type === 'project_index') return { projects: this.projects }
    if (resource.type === 'session_index') {
      return { sessions: [...this.sessions.values()].filter((session) => session.project_id === resource.id).map((session) => sessionSummary(session, this.resourceRevision(resource), this.currentRunID(session.id))) }
    }
    if (resource.type === 'session_content') {
      const state = this.contents.get(resource.id)
      const session = state?.session ?? this.sessions.get(resource.id)
      if (!state || !session) return { schema_version: 1, session: fullMetadata({ id: resource.id, project_id: '', display_name: '', status: 'idle' }, null), history: historyWindow([]), active_run: null, compaction: { checkpoints: [], truncated: false } }
      return {
        schema_version: 1,
        session: fullMetadata(session, state.activeRun ?? null),
        history: historyWindow(state.items, state.hasMoreBefore ?? false),
        active_run: state.activeRun ?? null,
        compaction: state.compaction ?? { checkpoints: [], truncated: false },
      }
    }
    if (resource.type === 'provider_settings') return { server_root: '/server-root', config_path: '/server-root/sai.yaml', default_provider: 'fake', default_model: 'fast', providers: [] }
    if (resource.type === 'codex_login') return { provider: resource.id, status: 'signed_out', login_id: '', user_code: '', verification_url: '', refreshable: false, error_code: '', error_message: '' }
    if (resource.type === 'model_catalog') return { models: [] }
    return {}
  }

  private async command(socket: WebSocketRoute, message: Record<string, any>): Promise<void> {
    const payload = message.payload ?? {}
    const command: Command = { name: String(payload.name ?? ''), request_id: String(payload.request_id ?? ''), arguments: (payload.arguments ?? {}) as Record<string, unknown> }
    this.commands.push(command)
    socket.send(envelope('command_accepted', this.nextID('accepted'), { request_id: command.request_id }))
    const result = await this.defaultResult(command)
    // A command first changes the authoritative mock state, then the spec
    // hook controls the resulting publication/timing. This lets a test hold
    // the actual run-start change instead of racing the admission helper.
    await this.options.onCommand?.(this, command)
    if (result === undefined) return
    socket.send(envelope('command_result', this.nextID('result'), { request_id: command.request_id, status: 'succeeded', result }))
  }

  private async defaultResult(command: Command): Promise<Record<string, unknown> | null | undefined> {
    const args = command.arguments
    switch (command.name) {
      case 'project.create': {
        const projectID = `project-${this.projects.length + 1}`
        this.addProject({ id: projectID, root: String(args.root ?? '/fixture'), display_name: String(args.display_name ?? 'Fixture'), archived: false, created_at: timestamp, updated_at: timestamp })
        return { operation_id: String(args.operation_id), project_id: projectID, created: true }
      }
      case 'project.rename': {
        const project = this.projects.find((candidate) => candidate.id === String(args.project_id))
        if (project) project.display_name = String(args.display_name)
        this.publishChange({ type: 'project_index', id: 'server' }, [{ op: 'upsert', key: String(args.project_id), value: project }])
        return { project_id: String(args.project_id), display_name: String(args.display_name) }
      }
      case 'project.archive':
      case 'project.restore': {
        const project = this.projects.find((candidate) => candidate.id === String(args.project_id))
        if (project) project.archived = command.name.endsWith('archive')
        this.publishChange({ type: 'project_index', id: 'server' }, [{ op: 'upsert', key: String(args.project_id), value: project }])
        return { project_id: String(args.project_id), archived: project?.archived ?? false }
      }
      case 'project.delete': {
        const projectID = String(args.project_id)
        const index = this.projects.findIndex((project) => project.id === projectID)
        if (index >= 0) this.projects.splice(index, 1)
        this.publishChange({ type: 'project_index', id: 'server' }, [{ op: 'remove', key: projectID }])
        for (const session of [...this.sessions.values()].filter((candidate) => candidate.project_id === projectID)) {
          this.sessions.delete(session.id)
          this.contents.delete(session.id)
          this.publishIndexChange(projectID, [{ op: 'remove', key: session.id }])
        }
        return { project_id: projectID, status: 'removed', removed_sessions: 1 }
      }
      case 'project.models.read':
        return { project_id: String(args.project_id), default_provider: 'fake', default_model: 'fast', models: [{ provider: 'fake', model_profile: 'fast', model_id: 'fake-model', reasoning_levels: ['low', 'high'], default_reasoning_level: 'high' }], blob: null }
      case 'session.create': {
        const sessionID = String(args.session_id)
        const session: WireSession = { id: sessionID, project_id: String(args.project_id), display_name: String(args.display_name ?? 'New session'), status: 'idle', archived: false, created_at: timestamp, updated_at: timestamp, last_used_at: timestamp, last_seq: 0, provider: String(args.provider ?? 'fake'), model_profile: String(args.model_profile ?? 'fast'), model_id: String(args.model_id ?? 'fake-model'), reasoning_level: String(args.reasoning_level ?? 'high'), created_cwd: '/fixture' }
        this.addSession(session)
        return { session_id: sessionID, project_id: session.project_id }
      }
      case 'session.rename':
        this.updateSession(String(args.session_id), { display_name: String(args.display_name) })
        return { session_id: String(args.session_id), display_name: String(args.display_name) }
      case 'session.archive':
      case 'session.restore': {
        const sessionID = String(args.session_id)
        this.updateSession(sessionID, { archived: command.name.endsWith('archive') })
        return { session_id: sessionID, archived: command.name.endsWith('archive') }
      }
      case 'session.delete': {
        const sessionID = String(args.session_id)
        const session = this.sessions.get(sessionID)
        this.sessions.delete(sessionID)
        this.contents.delete(sessionID)
        if (session) this.publishIndexChange(session.project_id, [{ op: 'remove', key: sessionID }])
        return { session_id: sessionID, status: 'removed', removed_sessions: 1 }
      }
      case 'session.set_full_access':
        this.updateSession(String(args.session_id), { full_access: Boolean(args.full_access) })
        return { session_id: String(args.session_id), full_access: Boolean(args.full_access) }
      case 'session.set_debug':
        this.updateSession(String(args.session_id), { debug: { request_bodies: Boolean(args.request_bodies) } })
        return { session_id: String(args.session_id), request_bodies: Boolean(args.request_bodies) }
      case 'session.history.read': {
        const sessionID = String(args.session_id)
        const state = this.contents.get(sessionID)
        const cursor = Number(args.cursor ?? 0)
        const direction = args.direction === undefined ? '' : String(args.direction)
        const limit = Number(args.limit ?? 50)
        const all = state?.items ?? []
        let selected = all
        if (direction === 'before') selected = (state?.historyBefore?.length ? state.historyBefore : all.filter((item) => Number(item.seq ?? 0) < cursor)).slice(-limit)
        if (direction === 'after') selected = all.filter((item) => Number(item.seq ?? 0) > cursor).slice(0, limit)
        selected = selected.slice(0, limit)
        const normalized = pageItems(selected)
        return { session_id: sessionID, cursor, direction, limit, align_turn: Boolean(args.align_turn ?? false), history: { items: normalized.map((item) => ({ seq: item.seq, id: item.key && (item.key as Record<string, unknown>).item_id, turn_id: (item.key as Record<string, unknown>).turn_id, agent_iteration: (item.key as Record<string, unknown>).agent_iteration, created_at: item.created_at, kind: item.kind, visibility: item.visibility, audience: item.audience, ...(item.message === undefined ? {} : { message: item.message }) })), oldest_seq: normalized.length ? Number(normalized[0].seq) : 0, newest_seq: normalized.length ? Number(normalized[normalized.length - 1].seq) : 0, has_more_before: direction === 'before' ? selected.length === limit : false, has_more_after: false }, blob: null }
      }
      case 'run.start':
        this.startRun(String(args.session_id), String(args.run_id), 'turn-fixture')
        return { session_id: String(args.session_id), run_id: String(args.run_id), status: 'running' }
      case 'run.continue':
        this.startRun(String(args.session_id), String(args.run_id), 'turn-continue')
        return { session_id: String(args.session_id), run_id: String(args.run_id), status: 'running' }
      case 'run.cancel':
        this.settleRun(this.findSessionForRun(String(args.run_id)) ?? '', String(args.run_id), 'cancelled', [])
        return { run_id: String(args.run_id), status: 'cancelled' }
      case 'run.prompt.append': {
        const runID = String(args.run_id)
        const queue = this.promptQueues.get(runID) ?? []
        const prompt = { id: `prompt-${queue.length + 1}`, content: String(args.content), steer: false }
        queue.push(prompt)
        this.promptQueues.set(runID, queue)
        this.sendTypedEvent(String(args.session_id), runID, { type: 'run.prompt_queue', prompts: queue })
        return { operation_id: String(args.operation_id), session_id: String(args.session_id), run_id: runID, accepted: true }
      }
      case 'run.prompt.remove': {
        const runID = String(args.run_id)
        const queue = this.promptQueues.get(runID) ?? []
        const index = queue.findIndex((prompt) => prompt.id === String(args.prompt_id))
        if (index >= 0) queue.splice(index, 1)
        this.sendTypedEvent(String(args.session_id), runID, { type: 'run.prompt_queue', prompts: queue })
        return { session_id: String(args.session_id), run_id: runID, prompt_id: String(args.prompt_id), removed: true }
      }
      case 'run.prompt.steer': {
        const runID = String(args.run_id)
        const queue = this.promptQueues.get(runID) ?? []
        const prompt = queue.find((candidate) => candidate.id === String(args.prompt_id))
        if (prompt) prompt.steer = Boolean(args.steer)
        queue.sort((left, right) => Number(right.steer) - Number(left.steer))
        this.sendTypedEvent(String(args.session_id), runID, { type: 'run.prompt_queue', prompts: queue })
        return { session_id: String(args.session_id), run_id: runID, prompt_id: String(args.prompt_id), steer: Boolean(args.steer) }
      }
      case 'run.prompt.move':
        return { session_id: String(args.session_id), run_id: String(args.run_id), prompt_id: String(args.prompt_id), moved: false }
      case 'run.tool.cancel':
        return { session_id: String(args.session_id), run_id: String(args.run_id), tool_call_id: String(args.tool_call_id), cancelled: true }
      case 'session.mark_read':
        return { session_id: String(args.session_id), run_id: String(args.run_id), marked_read: true }
      case 'session.compact':
        return { session_id: String(args.session_id), status: 'completed', compacted: false }
      default:
        return {}
    }
  }

  private findSessionForRun(runID: string): string | undefined {
    return [...this.contents.values()].find((content) => content.activeRun?.run_id === runID)?.session.id
  }

  startRun(sessionID: string, runID: string, turnID = 'turn-fixture'): void {
    const state = this.contents.get(sessionID)
    const session = this.sessions.get(sessionID)
    if (!state || !session) return
    const run: ActiveRun = { run_id: runID, session_id: sessionID, turn_id: turnID, started_at: timestamp, status: 'running', recoverable: true, run_epoch: 'run-epoch', run_cursor: '0', replay_available: false, recovery_required: false }
    state.activeRun = run
    this.runCursors.set(runID, 0)
    Object.assign(session, { status: 'running', last_run_id: runID, latest_run_id: runID })
    this.publishIndexChange(session.project_id, [{ op: 'upsert', key: sessionID, value: sessionSummary(session, this.nextIndexRevision(session.project_id), runID) }])
    this.publishContentChange(sessionID, [
      { op: 'metadata.replace', metadata: fullMetadata(session, run) },
      { op: 'active_run.replace', active_run: run },
    ])
    this.sendTypedEvent(sessionID, runID, { type: 'run.started', turn_id: turnID, status: 'running' })
  }

  sendEvents(sessionID: string, runID: string, events: TypedEvent[]): void {
    for (const event of events) this.sendTypedEvent(sessionID, runID, event)
  }

  publishSessionContentOperations(sessionID: string, operations: Record<string, unknown>[]): void {
    this.publishContentChange(sessionID, operations)
  }

  publishResourceOperations(resource: Resource, operations: Record<string, unknown>[]): void {
    this.publishChange(resource, operations)
  }

  snapshotCount(resource: { type: string; id: string }): number {
    return this.snapshotCounts.get(this.resourceKey(resource)) ?? 0
  }

  activeSubscriptionID(resource: Resource): string | undefined {
    return this.activeSubscriptions().find((subscription) => this.resourceKey(subscription.resource) === this.resourceKey(resource))?.subscriptionID
  }

  retiredSubscriptionID(resource: Resource): string | undefined {
    return [...this.retiredSubscriptions.values()].reverse().find((subscription) => this.resourceKey(subscription.resource) === this.resourceKey(resource))?.subscriptionID
  }

  isSubscriptionActive(subscriptionID: string): boolean {
    return this.activeSubscription(subscriptionID) !== undefined
  }

  activeSubscriptionCount(resource?: Resource): number {
    return this.activeSubscriptions().filter((subscription) => !resource || this.resourceKey(subscription.resource) === this.resourceKey(resource)).length
  }

  delaySnapshots(resource: Resource): void {
    this.heldSnapshots.add(this.resourceKey(resource))
  }

  releaseSnapshots(resource: Resource): void {
    const key = this.resourceKey(resource)
    this.heldSnapshots.delete(key)
    const waiters = this.pendingSnapshotReleases.get(key) ?? []
    this.pendingSnapshotReleases.delete(key)
    for (const release of waiters) release()
  }

  holdChanges(resource: Resource): void {
    this.heldChanges.add(this.resourceKey(resource))
  }

  releaseChanges(resource: Resource, order: 'ordered' | 'reverse' = 'ordered'): void {
    const key = this.resourceKey(resource)
    this.heldChanges.delete(key)
    const pending = this.pendingChanges.get(key) ?? []
    this.pendingChanges.delete(key)
    const changes = order === 'reverse' ? [...pending].reverse() : pending
    for (const change of changes) {
      for (const subscription of this.activeSubscriptions()) {
        if (this.resourceKey(subscription.resource) === key) this.deliverChange(subscription, change)
      }
    }
  }

  closeSockets(): void {
    for (const socket of [...this.sockets.values()]) void socket.route.close({ code: 1000, reason: 'fixture disconnect' })
  }

  sendTypedEvent(sessionID: string, runID: string, event: TypedEvent): void {
    const requestedCursor = event.run_cursor === undefined ? undefined : String(event.run_cursor)
    const cursor = requestedCursor ?? String((this.runCursors.get(runID) ?? 0) + 1)
    this.runCursors.set(runID, Number(cursor))
    const payloadEvent: Record<string, unknown> = { session_id: sessionID, run_id: runID, run_cursor: cursor, ...event }
    if (event.type === 'run.settled' && event.durable_settlement_watermark === undefined) {
      payloadEvent.durable_settlement_watermark = { resource_revision: this.resourceRevision({ type: 'session_content', id: sessionID }), run_cursor: cursor, verified: false, covered_items: [] }
    }
    for (const subscription of this.activeSubscriptions()) {
      if (subscription.resource.type === 'session_content' && subscription.resource.id === sessionID) {
        this.deliverTypedEvent(subscription, payloadEvent)
      }
    }
  }

  /** Sends a frame that was already in flight when the owning subscription was released. */
  sendLateTypedEvent(subscriptionID: string, sessionID: string, runID: string, event: TypedEvent): boolean {
    const subscription = this.retiredSubscriptions.get(subscriptionID)
    if (!subscription || subscription.active) return false
    const payloadEvent = { session_id: sessionID, run_id: runID, run_cursor: String(event.run_cursor ?? 1), ...event }
    if (subscription.socketID === 0 || this.sockets.get(subscription.socketID)?.closed) return false
    subscription.socket.send(envelope('subscription_event', this.nextID('late-event'), { subscription_id: subscriptionID, resource: subscription.resource, event: payloadEvent }))
    return true
  }

  /** Sends a deliberately stale/in-flight resource frame bypassing normal publication. */
  sendLateChange(subscriptionID: string, sequence: string, previousSequence: string, operations: Record<string, unknown>[]): boolean {
    const subscription = this.retiredSubscriptions.get(subscriptionID) ?? this.activeSubscription(subscriptionID)
    if (!subscription || this.sockets.get(subscription.socketID)?.closed) return false
    subscription.socket.send(envelope('change', this.nextID('late-change'), {
      subscription_id: subscriptionID,
      resource: subscription.resource,
      stream_epoch: 'e2e-epoch',
      sequence,
      previous_sequence: previousSequence,
      resource_revision: sequence,
      operations,
    }))
    return true
  }

  settleRun(
    sessionID: string,
    runID: string,
    status: 'committed' | 'failed' | 'interrupted' | 'cancelled',
    items: WireItem[] = [],
    metadataPatch: Record<string, unknown> = {},
    failure?: { code: string; message: string },
  ): void {
    const state = this.contents.get(sessionID)
    const session = this.sessions.get(sessionID)
    if (!state || !session) return
    const settlementCursor = String((this.runCursors.get(runID) ?? 0) + 1)
    const terminalCoveredItems = status === 'failed' || status === 'interrupted' || status === 'cancelled'
      ? items.map((item) => ({ turn_id: String(item.turn_id ?? 'turn-fixture'), agent_iteration: Number(item.agent_iteration ?? 1), item_id: String(item.id ?? ''), run_cursor: settlementCursor }))
      : []
    if (failure) this.sendTypedEvent(sessionID, runID, { type: 'turn.failed', turn_id: String(metadataPatch.interrupted_turn_id ?? state.activeRun?.turn_id ?? 'turn-fixture'), code: failure.code, message: failure.message })
    this.sendTypedEvent(sessionID, runID, { type: 'run.settled', status, durable_settlement_watermark: { resource_revision: this.nextRevision(sessionID), run_cursor: String((this.runCursors.get(runID) ?? 0) + 1), verified: status !== 'committed' && terminalCoveredItems.length > 0, covered_items: terminalCoveredItems } })
    const priorItems = state.historyBefore ?? []
    const seen = new Set(items.map((item) => String(item.id ?? '')))
    state.items = [...priorItems.filter((item) => !seen.has(String(item.id ?? ''))), ...items]
    state.activeRun = null
    Object.assign(session, { status: status === 'failed' ? 'failed' : status === 'interrupted' ? 'interrupted' : 'idle', last_run_id: runID, latest_run_id: runID, last_run_status: status, last_seq: items.reduce((max, item) => Math.max(max, Number(item.seq ?? 0)), 0), ...metadataPatch })
    this.publishContentChange(sessionID, [{ op: 'history.window.replace', window: historyWindow(state.items) }, { op: 'active_run.clear' }, { op: 'metadata.replace', metadata: fullMetadata(session, null) }])
    const indexRunID = status === 'failed' || status === 'interrupted' ? runID : null
    this.publishIndexChange(session.project_id, [{ op: 'upsert', key: sessionID, value: sessionSummary(session, this.nextIndexRevision(session.project_id), indexRunID) }])
  }

  /** Replaces only the next snapshot, modeling a recovery/resnapshot barrier. */
  recoverRunFromSnapshot(sessionID: string, runID: string, items: WireItem[]): void {
    const state = this.contents.get(sessionID)
    const session = this.sessions.get(sessionID)
    if (!state || !session || state.activeRun?.run_id !== runID) return
    state.items = [...items]
    state.activeRun = { ...state.activeRun, run_cursor: String(this.runCursors.get(runID) ?? 0), recovery_required: false, replay_available: false }
    Object.assign(session, { status: 'running', last_seq: items.reduce((max, item) => Math.max(max, Number(item.seq ?? 0)), 0) })
    const key = this.resourceKey({ type: 'session_content', id: sessionID })
    this.resourceRevisions.set(key, (this.resourceRevisions.get(key) ?? 0) + 1)
  }

  private publishIndexChange(projectID: string, operations: Record<string, unknown>[]): void {
    this.publishChange({ type: 'session_index', id: projectID }, operations)
  }

  private publishContentChange(sessionID: string, operations: Record<string, unknown>[]): void {
    this.publishChange({ type: 'session_content', id: sessionID }, operations)
  }

  private publishChange(resource: Resource, operations: Record<string, unknown>[]): void {
    const key = this.resourceKey(resource)
    const previous = this.resourceSequences.get(key) ?? 0
    const sequence = previous + 1
    this.resourceSequences.set(key, sequence)
    const revision = (this.resourceRevisions.get(key) ?? 0) + 1
    this.resourceRevisions.set(key, revision)
    const change: ChangePublication = {
      resource: { ...resource },
      stream_epoch: 'e2e-epoch',
      sequence: String(sequence),
      previous_sequence: String(previous),
      resource_revision: String(revision),
      operations: operations.map((operation) => JSON.parse(JSON.stringify(operation)) as Record<string, unknown>),
    }
    const history = this.changeHistory.get(key) ?? []
    history.push(change)
    this.changeHistory.set(key, history)
    if (this.heldChanges.has(key)) {
      const pending = this.pendingChanges.get(key) ?? []
      pending.push(change)
      this.pendingChanges.set(key, pending)
      return
    }
    for (const subscription of this.activeSubscriptions()) {
      if (this.resourceKey(subscription.resource) === key) this.deliverChange(subscription, change)
    }
  }

  private deliverChange(subscription: Subscription, change: ChangePublication): void {
    if (!subscription.active || this.sockets.get(subscription.socketID)?.closed) return
    subscription.socket.send(envelope('change', this.nextID('change'), {
      subscription_id: subscription.subscriptionID,
      resource: subscription.resource,
      stream_epoch: change.stream_epoch,
      sequence: change.sequence,
      previous_sequence: change.previous_sequence,
      resource_revision: change.resource_revision,
      operations: change.operations,
    }))
  }

  private deliverTypedEvent(subscription: Subscription, event: Record<string, unknown>): void {
    if (!subscription.active || this.sockets.get(subscription.socketID)?.closed) return
    subscription.socket.send(envelope('subscription_event', this.nextID('event'), {
      subscription_id: subscription.subscriptionID,
      resource: subscription.resource,
      event,
    }))
  }

  private nextID(prefix: string): string {
    this.messageID += 1
    return `${prefix}-${this.messageID}`
  }
}

export async function installSyncMock(page: Page, options: SyncMockOptions = {}): Promise<SyncMockServer> {
  return new SyncMockServer(page, options).install()
}

export function messageItem(seq: number, role: 'user' | 'assistant' | 'tool', content: string, overrides: Record<string, unknown> = {}): WireItem {
  return { seq, id: `item-${seq}`, turn_id: 'turn-fixture', agent_iteration: 1, created_at: new Date(seq * 1000).toISOString(), kind: 'message', visibility: 'visible', audience: role === 'user' ? 'user' : 'model', message: role === 'tool' ? { role: 'tool', tool_call_id: `tool-${seq}`, content: { inline: content } } : { role, content: { inline: content } }, ...overrides }
}
