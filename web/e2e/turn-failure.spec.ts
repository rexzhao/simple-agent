import { expect, test, type Page, type Route } from '@playwright/test'

const project = { id: 'project-1', root: '/fixture', display_name: 'Fixture', archived: false, created_at: '', updated_at: '' }
const session = { id: 'session-1', project_id: project.id, display_name: 'Failure fixture', provider: 'fake', model_profile: 'fake', model_id: 'fake', status: 'idle', created_at: '', updated_at: '', last_used_at: '', created_cwd: '', last_seq: 0, archived: false }

const failedUserItem = {
  seq: 1, id: 'u1', turn_id: 'turn-1', created_at: new Date(1000).toISOString(), kind: 'message', visibility: 'normal', audience: 'user',
  message: { role: 'user', content: { inline: 'fix the bug' } },
}

type Gate = { release: (events: Array<Record<string, unknown>>) => void; promise: Promise<Array<Record<string, unknown>>> }
function newGate(): Gate {
  let release!: (events: Array<Record<string, unknown>>) => void
  const promise = new Promise<Array<Record<string, unknown>>>((resolve) => { release = resolve })
  return { release, promise }
}
function sse(events: Array<Record<string, unknown>>, startId = 1): string {
  return events.map((event, index) => `id: ${startId + index}\ndata: ${JSON.stringify(event)}\n\n`).join('')
}
async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

// mockApp serves a session whose first run fails on demand. `committed`
// flips the items endpoint to return the persisted user message, mirroring
// the backend saving the turn input before the model runs.
async function mockApp(page: Page, options: { gates: Gate[]; initialItems?: Array<Record<string, unknown>>; committedItems?: () => Array<Record<string, unknown>> }) {
  let connection = 0
  let runPosts = 0
  const runBodies: Array<{ content?: string; images?: unknown[]; replay_item_id?: string }> = []
  await page.route('**/api/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/api/bootstrap') return json(route, { version: 'e2e', cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' })
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [session] })
    if (url.pathname === '/api/runs/active') return json(route, { runs: [] })
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, session)
    if (url.pathname === `/api/sessions/${session.id}/items`) {
      const items = options.committedItems?.() ?? options.initialItems ?? []
      const seqs = items.map((item) => Number((item as { seq?: number }).seq ?? 0)).filter(Boolean)
      return json(route, { items, oldest_seq: seqs[0] ?? 0, newest_seq: seqs.at(-1) ?? 0, has_more_before: false, has_more_after: false })
    }
    if (url.pathname === `/api/sessions/${session.id}/runs` && request.method() === 'POST') {
      runPosts++
      runBodies.push(request.postDataJSON() as { content?: string; images?: unknown[]; replay_item_id?: string })
      return json(route, { run_id: 'run-1', session_id: session.id, status: 'running' }, 202)
    }
    if (url.pathname === '/api/runs/run-1/events') {
      connection++
      if (connection === 1) return route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse([{ type: 'turn.started', turn_id: 'turn-1' }]) })
      const gate = options.gates[Math.min(connection - 2, options.gates.length - 1)] ?? newGate()
      const events = await gate.promise
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse(events, 100 * connection) })
    }
    return json(route, { error: { code: 'not_mocked', message: `${request.method()} ${url.pathname}` } }, 404)
  })
  return { runPosts: () => runPosts, runBodies }
}

test('shows the failure reason in the conversation and resends the trailing user message', async ({ page }) => {
  const fail = newGate()
  const settle = newGate()
  const hang = newGate()
  let committed = false
  const app = await mockApp(page, {
    gates: [fail, settle, hang],
    committedItems: () => committed ? [failedUserItem] : [],
  })
  await page.goto('/')
  await page.getByPlaceholder('Send a message to SAI').fill('fix the bug')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.message.user .message-text').last()).toHaveText('fix the bug')

  // The turn fails: the reason lands in the conversation, not the global banner.
  fail.release([{ type: 'turn.failed', turn_id: 'turn-1', code: 'model_http_error', message: 'model provider returned 429 Too Many Requests: {"error":{"message":"slow down"}}' }])
  const turnError = page.locator('.turn-error')
  await expect(turnError).toBeVisible()
  await expect(turnError).toContainText('429 Too Many Requests')
  await expect(turnError).toContainText('slow down')
  await expect(page.locator('.error-banner')).toHaveCount(0)
  // The run is still attached: no resend offer yet.
  await expect(page.getByRole('button', { name: 'Resend' })).toHaveCount(0)

  // The run settles: the persisted user message remains as the tail and
  // gets a resend offer; the failure card stays.
  committed = true
  settle.release([{ type: 'run.settled', run_id: 'run-1', status: 'failed', turn_id: 'turn-1', last_seq: 1, message: 'run failed' }])
  const resend = page.getByRole('button', { name: 'Resend' })
  await expect(resend).toBeVisible()
  await expect(turnError).toBeVisible()

  // Dismissal hides the card; the resend offer stays.
  await page.getByRole('button', { name: 'Dismiss error' }).click()
  await expect(turnError).toHaveCount(0)
  await expect(resend).toBeVisible()

  // Resending replays the persisted user item without duplicating its content.
  await resend.click()
  await expect.poll(app.runPosts).toBe(2)
  expect(app.runBodies[1]?.replay_item_id).toBe(failedUserItem.id)
  expect(app.runBodies[1]?.content).toBeUndefined()
  await expect(resend).toHaveCount(0)
  await expect(turnError).toHaveCount(0)
})

test('does not offer resend when the session ends with an assistant message', async ({ page }) => {
  await mockApp(page, {
    gates: [],
    initialItems: [
      { seq: 1, id: 'u1', turn_id: 'turn-1', created_at: new Date(1000).toISOString(), kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'user', content: { inline: 'question' } } },
      { seq: 2, id: 'a1', turn_id: 'turn-1', created_at: new Date(2000).toISOString(), kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', content: { inline: 'answer' } } },
    ],
  })
  await page.goto('/')
  await expect(page.locator('.message.assistant .message-text').last()).toHaveText('answer')
  await expect(page.getByRole('button', { name: 'Resend' })).toHaveCount(0)
  await expect(page.locator('.turn-error')).toHaveCount(0)
})
