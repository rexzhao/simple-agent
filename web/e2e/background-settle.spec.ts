import { expect, test, type Route } from '@playwright/test'

const project = {
  id: 'project-main',
  root: '/workspace/project',
  display_name: 'Main project',
  archived: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

const baseSession = {
  project_id: project.id,
  provider: 'fake',
  model_profile: 'fast',
  model_id: 'fake-model',
  reasoning_level: 'high',
  status: 'idle',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  created_cwd: project.root,
  archived: false,
}

// session-main is the most recently used, so it is selected at bootstrap
// while session-bg settles in the background.
const sessionMain = { ...baseSession, id: 'session-main', display_name: 'Main session', last_seq: 0, last_used_at: '2026-01-02T00:00:00Z' }
const sessionBg = { ...baseSession, id: 'session-bg', display_name: 'Background session', last_seq: 0, last_used_at: '2026-01-01T00:00:00Z' }

const bgItems = [
  { seq: 1, id: 'item-bg-user', turn_id: 'turn-bg', created_at: '2026-01-01T00:00:01Z', kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'user', content: { inline: 'Background task' } } },
  { seq: 2, id: 'item-bg-assistant', turn_id: 'turn-bg', created_at: '2026-01-01T00:00:02Z', kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'assistant', content: { inline: 'Background answer' } } },
]

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

function sse(events: Array<Record<string, unknown>>): string {
  return events.map((event, index) => `id: ${index + 1}\ndata: ${JSON.stringify(event)}\n\n`).join('')
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

test('a run settling on a background session reconciles without a manual refresh', async ({ page }) => {
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    if (url.pathname === '/api/bootstrap') return json(route, { version: 'e2e', cwd: '/workspace', server_root: '/server-root', config_path: '/server-root/sai.yaml' })
    if (url.pathname === '/api/runs/active') {
      return json(route, { runs: [{ run_id: 'run-bg', session_id: sessionBg.id, turn_id: 'turn-bg', started_at: '2026-01-01T00:00:00Z', status: 'running' }] })
    }
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) {
      return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [sessionMain, sessionBg] })
    }
    if (url.pathname === '/api/runs/run-bg/events') {
      return route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: sse([
          { type: 'turn.started', turn_id: 'turn-bg' },
          { type: 'agent.iteration.started', turn_id: 'turn-bg', agent_iteration: 1 },
          { type: 'text.delta', turn_id: 'turn-bg', agent_iteration: 1, text: 'Background answer' },
          { type: 'turn.committed', turn_id: 'turn-bg', last_seq: 2 },
          { type: 'run.settled', run_id: 'run-bg', status: 'committed', turn_id: 'turn-bg', last_seq: 2 },
        ]),
      })
    }
    if (url.pathname === `/api/sessions/${sessionMain.id}`) return json(route, sessionMain)
    if (url.pathname === `/api/sessions/${sessionMain.id}/snapshot`) {
      return json(route, { session_id: sessionMain.id, revision: '0', session: sessionMain, history: itemsPage() })
    }
    if (url.pathname === `/api/sessions/${sessionMain.id}/items`) return json(route, itemsPage())
    const settledBg = { ...sessionBg, last_seq: 2 }
    if (url.pathname === `/api/sessions/${sessionBg.id}`) return json(route, settledBg)
    if (url.pathname === `/api/sessions/${sessionBg.id}/snapshot`) {
      return json(route, { session_id: sessionBg.id, revision: '2', session: settledBg, history: itemsPage(bgItems) })
    }
    if (url.pathname === `/api/sessions/${sessionBg.id}/items`) return json(route, itemsPage(bgItems))
    return json(route, { error: { code: 'not_mocked', message: `${request.method()} ${url.pathname} was not mocked` } }, 404)
  })

  await page.goto('/')
  // The main session is selected; the background run re-attaches and settles.
  await expect(page.locator('.session-tree-button', { hasText: 'Main session' })).toBeVisible()
  await page.locator('.session-tree-button', { hasText: 'Background session' }).click()

  // Regression: the background settle used to strand the run in reconciling
  // until the 60s timeout raised a "Refresh needed" banner, even though the
  // snapshot had already arrived. The run must reconcile by itself.
  await expect(page.getByText('Refresh needed')).toHaveCount(0)
  await expect(page.locator('.message.assistant.transient')).toHaveCount(0)
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('Background answer')
  await expect(page.locator('.message.user:not(.transient)')).toContainText('Background task')
})
