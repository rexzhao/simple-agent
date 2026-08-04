import { expect, test, type Page, type Route } from '@playwright/test'

const project = {
  id: 'project-main',
  root: '/workspace/project',
  display_name: 'Main project',
  archived: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

const session = {
  id: 'session-main',
  project_id: project.id,
  display_name: 'Primary session',
  provider: 'fake',
  model_profile: 'fast',
  model_id: 'fake-model',
  reasoning_level: 'high',
  status: 'idle',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  last_used_at: '2026-01-01T00:00:00Z',
  created_cwd: project.root,
  last_seq: 0,
  archived: false,
  context: { context_window: 32000, context_window_source: 'configured', warning_threshold_percent: 80 },
}

function itemsPage(items: unknown[] = []) {
  const sequences = items.map((item) => Number((item as { seq?: number }).seq ?? 0)).filter(Boolean)
  return {
    items,
    oldest_seq: sequences[0] ?? 0,
    newest_seq: sequences.at(-1) ?? 0,
    has_more_before: false,
    has_more_after: false,
  }
}

function messageItems(user: string, assistant: string) {
  return [
    { seq: 1, id: 'item-user', turn_id: 'turn-main', created_at: '2026-01-01T00:00:01Z', kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'user', content: { inline: user } } },
    { seq: 2, id: 'item-assistant', turn_id: 'turn-main', created_at: '2026-01-01T00:00:02Z', kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'assistant', content: { inline: assistant } } },
  ]
}

function sse(events: Array<Record<string, unknown>>): string {
  return events.map((event, index) => `id: ${index + 1}\ndata: ${JSON.stringify(event)}\n\n`).join('')
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

function commonBootstrap(pathname: string): unknown | undefined {
  if (pathname === '/api/bootstrap') return { version: 'e2e', cwd: '/workspace', server_root: '/server-root', config_path: '/server-root/sai.yaml' }
  if (pathname === '/api/runs/active') return { runs: [] }
  return undefined
}

async function mockExistingSessionApp(page: Page, handler?: (route: Route, url: URL) => Promise<boolean>, options?: { sessionLastSeq?: () => number }) {
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    // The app keeps a long-lived lifecycle stream open. Holding the route
    // prevents the client from treating an immediate EOF as a disconnect and
    // re-running a full bootstrap (which would clear transient active runs).
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    if (handler && await handler(route, url)) return
    const common = commonBootstrap(url.pathname)
    if (common) return json(route, common)
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) {
      return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [session] })
    }
    const effectiveLastSeq = options?.sessionLastSeq?.() ?? session.last_seq
    const effectiveSession = { ...session, last_seq: effectiveLastSeq }
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, effectiveSession)
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) {
      return json(route, { session_id: session.id, revision: String(effectiveLastSeq), session: effectiveSession, history: itemsPage() })
    }
    if (url.pathname === `/api/sessions/${session.id}/items`) return json(route, itemsPage())
    return json(route, { error: { code: 'not_mocked', message: `${route.request().method()} ${url.pathname} was not mocked` } }, 404)
  })
}

test('connects a first project, creates a session, and commits a streamed run', async ({ page }) => {
  let projects: typeof project[] = []
  let sessions: typeof session[] = []
  let savedItems: unknown[] = []
  let eventConnections = 0
  let finishRun!: () => void
  const finishRunGate = new Promise<void>((resolve) => { finishRun = resolve })

  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    const common = commonBootstrap(url.pathname)
    if (common) return json(route, common)
    if (url.pathname === '/api/projects' && request.method() === 'GET') return json(route, { projects })
    if (url.pathname === '/api/projects' && request.method() === 'POST') {
      projects = [project]
      return json(route, { project, created: true }, 201)
    }
    if (url.pathname === `/api/projects/${project.id}/sessions` && request.method() === 'GET') {
      return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : sessions })
    }
    if (url.pathname === `/api/projects/${project.id}/models`) {
      return json(route, {
        default_provider: 'fake',
        default_model: 'fast',
        models: [{ provider: 'fake', model_profile: 'fast', model_id: 'fake-model', reasoning_levels: ['low', 'high'], default_reasoning_level: 'high' }],
      })
    }
    if (url.pathname === `/api/projects/${project.id}/sessions` && request.method() === 'POST') {
      sessions = [session]
      return json(route, session, 201)
    }
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, { ...session, last_seq: savedItems.length })
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) return json(route, { session_id: session.id, revision: String(savedItems.length), session: { ...session, last_seq: savedItems.length }, history: itemsPage(savedItems) })
    if (url.pathname === `/api/sessions/${session.id}/items`) return json(route, itemsPage(savedItems))
    if (url.pathname === `/api/sessions/${session.id}/runs` && request.method() === 'POST') {
      return json(route, { run_id: 'run-main', session_id: session.id, status: 'running' }, 202)
    }
    if (url.pathname === '/api/runs/run-main/events') {
      eventConnections++
      if (eventConnections === 1) {
        return route.fulfill({
          status: 200,
          contentType: 'text/event-stream',
          body: sse([
            { type: 'run.started', run_id: 'run-main', session_id: session.id, status: 'running' },
            { type: 'turn.started', turn_id: 'turn-main' },
            { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
            { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 1, text: 'Streamed answer' },
          ]),
        })
      }
      await finishRunGate
      savedItems = messageItems('Build the feature', 'Streamed answer')
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse([
          { type: 'turn.committed', turn_id: 'turn-main', last_seq: 2 },
          { type: 'run.settled', run_id: 'run-main', status: 'committed', turn_id: 'turn-main', last_seq: 2 },
        ]),
      })
    }
    return json(route, { error: { code: 'not_mocked', message: `${request.method()} ${url.pathname} was not mocked` } }, 404)
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Connect your first project' })).toBeVisible()
  await page.getByLabel('Project directory').fill(project.root)
  await page.getByLabel(/Display name/).fill(project.display_name)
  await page.getByRole('button', { name: 'Connect project' }).click()

  await expect(page.getByRole('heading', { name: 'No sessions yet' })).toBeVisible()
  await page.getByRole('button', { name: 'New session', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Choose a model' })).toBeVisible()
  await expect(page.getByLabel('Reasoning effort')).toHaveValue('high')
  await page.getByRole('button', { name: 'Create session' }).click()

  const composer = page.getByPlaceholder('Send a message to SAI')
  await composer.fill('Build the feature')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.message.assistant.transient')).toContainText('Streamed answer')
  await expect(page.getByText('Generating')).toBeVisible()

  finishRun()
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('Streamed answer')
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  expect(eventConnections).toBeGreaterThanOrEqual(2)
})

test('hands a durable assistant bubble through checkpointed and transient stream output', async ({ page }) => {
  let eventConnections = 0
  let releaseTail!: () => void
  let releaseSettled!: () => void
  const tailGate = new Promise<void>((resolve) => { releaseTail = resolve })
  const settledGate = new Promise<void>((resolve) => { releaseSettled = resolve })
  const assistantItem = (content: string) => ({
    seq: 2,
    id: 'assistant-stream',
    turn_id: 'turn-stream',
    agent_iteration: 1,
    created_at: '2026-01-01T00:00:02Z',
    kind: 'message',
    visibility: 'normal',
    audience: 'model',
    message: { role: 'assistant', content: { inline: content } },
  })

  await mockExistingSessionApp(page, async (route, url) => {
    const request = route.request()
    if (url.pathname === `/api/sessions/${session.id}/runs` && request.method() === 'POST') {
      await json(route, { run_id: 'run-stream', session_id: session.id, status: 'running' }, 202)
      return true
    }
    if (url.pathname !== '/api/runs/run-stream/events') return false
    eventConnections++
    if (eventConnections === 1) {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse([
          { type: 'run.started', run_id: 'run-stream', session_id: session.id, status: 'running' },
          { type: 'turn.started', turn_id: 'turn-stream' },
          { type: 'agent.iteration.started', turn_id: 'turn-stream', agent_iteration: 1 },
          {
            type: 'item.appended', session_id: session.id, run_id: 'run-stream', turn_id: 'turn-stream',
            seq: 2, revision: '2', item_id: 'assistant-stream', assistant_text_length: 1, item: assistantItem('a'),
          },
          { type: 'text.delta', turn_id: 'turn-stream', agent_iteration: 1, item_id: 'assistant-stream', text: 'a', durable_text_length: 1, durable_checkpointed: true },
        ]),
      })
      return true
    }
    if (eventConnections === 2) {
      await tailGate
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse([{ type: 'text.delta', turn_id: 'turn-stream', agent_iteration: 1, item_id: 'assistant-stream', text: 'b', durable_text_length: 1, durable_checkpointed: false }]),
      })
      return true
    }
    await settledGate
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      body: sse([
        {
          type: 'item.updated', session_id: session.id, run_id: 'run-stream', turn_id: 'turn-stream',
          seq: 3, revision: '3', item_id: 'assistant-stream', assistant_text_length: 2, item: assistantItem('ab'),
        },
        { type: 'turn.committed', turn_id: 'turn-stream', last_seq: 3 },
        { type: 'run.settled', run_id: 'run-stream', status: 'committed', turn_id: 'turn-stream', last_seq: 3 },
      ]),
    })
    return true
  })

  await page.goto('/')
  const composer = page.getByPlaceholder('Send a message to SAI')
  await composer.fill('stream durable output')
  await page.getByRole('button', { name: 'Send' }).click()

  // The append is already a visible durable row before any tail is allowed.
  await expect(page.locator('.message.assistant:not(.transient)')).toHaveCount(1)
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('a')
  await expect.poll(() => eventConnections).toBeGreaterThanOrEqual(2)

  releaseTail()
  // The uncheckpointed b is explicitly attached to the loaded item id, so it
  // extends the one durable bubble instead of creating an active bubble.
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('ab')
  await expect(page.locator('.message.assistant.transient')).toHaveCount(0)
  await expect(page.locator('.message.assistant:not(.transient)')).toHaveCount(1)

  releaseSettled()
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('ab')
  await expect(page.locator('.message.assistant:not(.transient)')).toHaveCount(1)
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
})

test('cancels an active run without persisting its transient turn', async ({ page }) => {
  let cancelRun!: () => void
  const cancelled = new Promise<void>((resolve) => { cancelRun = resolve })
  let cancelRequests = 0
  let cancelEventConnections = 0

  await mockExistingSessionApp(page, async (route, url) => {
    const request = route.request()
    if (url.pathname === `/api/sessions/${session.id}/runs` && request.method() === 'POST') {
      await json(route, { run_id: 'run-cancel', session_id: session.id, status: 'running' }, 202)
      return true
    }
    if (url.pathname === '/api/runs/run-cancel/events') {
      cancelEventConnections++
      if (cancelEventConnections === 1) {
        await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse([{ type: 'run.started', run_id: 'run-cancel', session_id: session.id, status: 'running' }]) })
        return true
      }
      await cancelled
      await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse([{ type: 'run.settled', run_id: 'run-cancel', status: 'cancelled', turn_id: 'turn-cancel' }]) })
      return true
    }
    if (url.pathname === '/api/runs/run-cancel' && request.method() === 'DELETE') {
      cancelRequests++
      cancelRun()
      await json(route, { status: 'cancelling' }, 202)
      return true
    }
    return false
  })

  await page.goto('/')
  const composer = page.getByPlaceholder('Send a message to SAI')
  await composer.fill('Wait for cancellation')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByRole('button', { name: 'Stop' })).toBeVisible()
  // Admission does not create a local user bubble. The committed item event
  // (which this cancellation fixture intentionally never sends) is the only
  // source of a rendered user message.
  await expect(page.locator('.message.user')).toHaveCount(0)

  await page.getByRole('button', { name: 'Stop' }).click()
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.user')).toHaveCount(0)
  expect(cancelRequests).toBe(1)
})

test('resyncs a recovered run from durable session history', async ({ page }) => {
  const recoveredItems = messageItems('Before refresh', 'Recovered answer')
  await mockExistingSessionApp(page, async (route, url) => {
    if (url.pathname === '/api/runs/active') {
      await json(route, { runs: [{ run_id: 'run-recovered', session_id: session.id, turn_id: 'turn-main', started_at: '2026-01-01T00:00:01Z', status: 'running' }] })
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}/items`) {
      await json(route, itemsPage(recoveredItems))
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) {
      await json(route, { session_id: session.id, revision: '2', session: { ...session, last_seq: 2 }, history: itemsPage(recoveredItems) })
      return true
    }
    if (url.pathname === '/api/runs/run-recovered/events') {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse([
          { type: 'run.resync_required', run_id: 'run-recovered', session_id: session.id, oldest_seq: 8 },
          { type: 'run.settled', run_id: 'run-recovered', status: 'committed', turn_id: 'turn-main', last_seq: 2 },
        ]),
      })
      return true
    }
    return false
  }, { sessionLastSeq: () => 2 })

  await page.goto('/')
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('Recovered answer')
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
})

test('renames sessions and projects and confirms project-wide deletion', async ({ page }) => {
  let currentProject = { ...project }
  let currentSession = { ...session }
  let projectExists = true
  let deleteConfirmation = ''

  page.on('dialog', (dialog) => {
    if (dialog.type() === 'prompt' && dialog.message() === 'Rename project') {
      void dialog.accept('Renamed project')
      return
    }
    if (dialog.type() === 'prompt' && dialog.message() === 'Rename session') {
      void dialog.accept('Renamed session')
      return
    }
    deleteConfirmation = dialog.message()
    void dialog.accept()
  })

  await mockExistingSessionApp(page, async (route, url) => {
    const request = route.request()
    if (url.pathname === '/api/projects' && request.method() === 'GET') {
      await json(route, { projects: projectExists ? [currentProject] : [] })
      return true
    }
    if (url.pathname === `/api/projects/${project.id}` && request.method() === 'PATCH') {
      const body = request.postDataJSON() as { display_name: string }
      currentProject = { ...currentProject, display_name: body.display_name }
      await json(route, currentProject)
      return true
    }
    if (url.pathname === `/api/projects/${project.id}/sessions`) {
      await json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [currentSession] })
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}` && request.method() === 'PATCH') {
      const body = request.postDataJSON() as { display_name: string }
      currentSession = { ...currentSession, display_name: body.display_name }
      await json(route, currentSession)
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}` && request.method() === 'GET') {
      await json(route, currentSession)
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) {
      await json(route, { session_id: session.id, revision: String(currentSession.last_seq), session: currentSession, history: itemsPage() })
      return true
    }
    if (url.pathname === `/api/projects/${project.id}/archive` && request.method() === 'POST') {
      await json(route, { ...currentProject, archived: true })
      return true
    }
    if (url.pathname === `/api/projects/${project.id}` && request.method() === 'DELETE') {
      projectExists = false
      await json(route, { status: 'removed', id: project.id, removed_sessions: 1 })
      return true
    }
    return false
  })

  await page.goto('/')
  await page.getByRole('button', { name: `Rename ${project.display_name}` }).click()
  await expect(page.getByText('Renamed project', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: `Rename ${session.display_name}` }).click()
  await expect(page.getByRole('heading', { name: 'Renamed session' })).toBeVisible()

  await page.getByRole('button', { name: 'Delete Renamed project', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Connect your first project' })).toBeVisible()
  expect(deleteConfirmation).toContain('1 saved session')
  expect(deleteConfirmation).toContain('All session history and attachments')
})

test('shows archived sessions and restores them to the active list', async ({ page }) => {
  let archived = false
  page.on('dialog', (dialog) => void dialog.accept())

  await mockExistingSessionApp(page, async (route, url) => {
    const request = route.request()
    if (url.pathname === `/api/projects/${project.id}/sessions`) {
      const wantsArchived = url.searchParams.get('archived') === 'true'
      await json(route, { sessions: wantsArchived === archived ? [session] : [] })
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}/archive` && request.method() === 'POST') {
      archived = true
      await json(route, { ...session, archived: true })
      return true
    }
    if (url.pathname === `/api/sessions/${session.id}/restore` && request.method() === 'POST') {
      archived = false
      await json(route, session)
      return true
    }
    return false
  })

  await page.goto('/')
  await page.getByRole('button', { name: `Archive ${session.display_name}` }).click()
  await expect(page.getByRole('heading', { name: 'No sessions yet' })).toBeVisible()

  await page.getByRole('button', { name: 'Archived (1)' }).click()
  await expect(page.getByText(session.display_name)).toBeVisible()
  await page.getByRole('button', { name: `Restore ${session.display_name}` }).click()

  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Archived (1)' })).toHaveCount(0)
})
