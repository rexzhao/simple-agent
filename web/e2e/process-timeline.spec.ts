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
  status: 'idle',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  last_used_at: '2026-01-01T00:00:00Z',
  created_cwd: project.root,
  last_seq: 0,
  archived: false,
  show_reasoning: true,
}

function sse(events: Array<Record<string, unknown>>, startId = 1): string {
  return events.map((event, index) => `id: ${startId + index}\ndata: ${JSON.stringify(event)}\n\n`).join('')
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

type Gate = { release: (events: Array<Record<string, unknown>>) => void; promise: Promise<Array<Record<string, unknown>>> }

function newGate(): Gate {
  let release!: (events: Array<Record<string, unknown>>) => void
  const promise = new Promise<Array<Record<string, unknown>>>((resolve) => { release = resolve })
  return { release, promise }
}

// mockApp serves the shell app and routes run event streams through gates so
// each test controls exactly when events reach the client. The first events
// connection immediately serves `initial`; later connections wait on gates.
async function mockApp(
  page: Page,
  options: {
    initial: Array<Record<string, unknown>>
    gates: Gate[]
    items?: () => unknown[]
  },
) {
  let connection = 0
  const initialEvents = [
    { type: 'run.started', run_id: 'run-main', session_id: session.id, status: 'running' },
    ...options.initial,
  ]
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    if (url.pathname === '/api/bootstrap') return json(route, { version: 'e2e', cwd: '/workspace', server_root: '/sr', config_path: '/sr/sai.yaml' })
    if (url.pathname === '/api/runs/active') return json(route, { runs: [] })
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) {
      return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [session] })
    }
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, session)
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) {
      const items = options.items?.() ?? []
      const seqs = items.map((item) => Number((item as { seq?: number }).seq ?? 0)).filter(Boolean)
      const lastSeq = seqs.at(-1) ?? 0
      return json(route, { session_id: session.id, revision: String(lastSeq), session: { ...session, last_seq: lastSeq }, history: { items, oldest_seq: seqs[0] ?? 0, newest_seq: seqs.at(-1) ?? 0, has_more_before: false, has_more_after: false } })
    }
    if (url.pathname === `/api/sessions/${session.id}/items`) {
      const items = options.items?.() ?? []
      const seqs = items.map((item) => Number((item as { seq?: number }).seq ?? 0)).filter(Boolean)
      return json(route, { items, oldest_seq: seqs[0] ?? 0, newest_seq: seqs.at(-1) ?? 0, has_more_before: false, has_more_after: false })
    }
    if (url.pathname === `/api/sessions/${session.id}/runs` && request.method() === 'POST') {
      return json(route, { run_id: 'run-main', session_id: session.id, status: 'running' }, 202)
    }
    if (url.pathname === '/api/runs/run-main/events') {
      connection++
      if (connection === 1) {
        return route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse(initialEvents) })
      }
      const gate = options.gates[Math.min(connection - 2, options.gates.length - 1)]
      const events = await gate.promise
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse(events, 100 * connection) })
    }
    return json(route, { error: { code: 'not_mocked', message: `${request.method()} ${url.pathname}` } }, 404)
  })
  await page.goto('/')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
}

function groupStates(page: Page) {
  return page.locator('details.tool-group').evaluateAll((els) =>
    els.map((el) => ({ open: (el as HTMLDetailsElement).open, summary: el.querySelector('.tool-group-summary')?.textContent })))
}

// Markers render with pointer-events: none, so a plain hit test always misses.
// Forcing pointer-events on the marker itself proves whether it is painted.
async function expectMarkerVisible(page: Page) {
  const marker = page.locator('.iteration-marker').first()
  await expect(marker).toBeAttached()
  const probe = await marker.evaluate((el) => {
    (el as HTMLElement).style.pointerEvents = 'auto'
    el.scrollIntoView({ block: 'center' })
    const rect = el.getBoundingClientRect()
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2)
    return { rect: { left: rect.left, top: rect.top, width: rect.width, height: rect.height }, hit: hit ? `${hit.tagName}.${hit.className}` : 'null', vw: window.innerWidth, vh: window.innerHeight }
  })
  console.log('marker probe:', JSON.stringify(probe))
  expect(probe.hit).toBe('I.iteration-marker')
}

test('iteration markers are painted next to live tool groups', async ({ page }) => {
  const hold = newGate()
  await mockApp(page, {
    initial: [
      { type: 'turn.started', turn_id: 'turn-main' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 2 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' },
    ],
    gates: [hold],
  })
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  await expectMarkerVisible(page)
  // An expanded group marks each round on its first row inside the body,
  // aligned to the same edge as top-level markers (21px accounting for the
  // 10px body inset).
  const bodyMarkers = page.locator('.tool-group-body .iteration-marker')
  expect(await bodyMarkers.allTextContents()).toEqual(['1', '2'])
  const marginRight = await bodyMarkers.first().evaluate((el) => getComputedStyle(el).marginRight)
  expect(marginRight).toBe('21px')
  hold.release([])
})

test('streaming text collapses the tool group; the next batch expands again', async ({ page }) => {
  const nextBatch = newGate()
  const hold = newGate()
  await mockApp(page, {
    initial: [
      { type: 'turn.started', turn_id: 'turn-main' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 2 },
      { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 2, text: 'I have both files, now editing. ' },
    ],
    gates: [nextBatch, hold],
  })

  // While text streams and no new tool has arrived, the group must be closed.
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  await expect(page.locator('.assistant-stream')).toBeVisible()
  await page.waitForTimeout(200)
  expect(await groupStates(page)).toEqual([{ open: false, summary: 'Read 2 files' }])

  // The next tool batch flushes the output step and starts a new live group.
  nextBatch.release([
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'edit_file', arguments: '{"path":"a.ts","old":"x","new":"y"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'edit_file', is_error: false, content: '' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't4', name: 'edit_file', arguments: '{"path":"b.ts","old":"x","new":"y"}' },
  ])
  await expect(page.locator('details.tool-group')).toHaveCount(2)
  await page.waitForTimeout(200)
  expect(await groupStates(page)).toEqual([
    { open: false, summary: 'Read 2 files' },
    { open: true, summary: 'Edited 2 files' },
  ])
  hold.release([])
})

test('tool batches without intermediate text keep the live group open', async ({ page }) => {
  const hold = newGate()
  await mockApp(page, {
    initial: [
      { type: 'turn.started', turn_id: 'turn-main' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 2 },
      { type: 'reasoning.delta', turn_id: 'turn-main', agent_iteration: 2, text: 'Thinking about the next step. ' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' },
    ],
    gates: [hold],
  })
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  await page.waitForTimeout(200)
  expect(await groupStates(page)).toEqual([{ open: true, summary: 'Read 2 files · Ran 1 command' }])
  hold.release([])
})

test('rounds that begin with output keep their marker outside the expanded group', async ({ page }) => {
  const hold = newGate()
  await mockApp(page, {
    initial: [
      { type: 'turn.started', turn_id: 'turn-main' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 2 },
      // Round 2 begins with text output, so round 2's marker belongs to the
      // output step — not to the tool group that follows.
      { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 2, text: 'Round two. ' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', is_error: false, content: 'ok' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 3 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't4', name: 'shell', arguments: '{"command":"pwd"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't4', name: 'shell', is_error: false, content: 'ok' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't5', name: 'shell', arguments: '{"command":"git status"}' },
    ],
    gates: [hold],
  })
  await expect(page.locator('details.tool-group')).toHaveCount(2)
  expect(await groupStates(page)).toEqual([
    { open: false, summary: 'Read 2 files' },
    { open: true, summary: 'Ran 3 commands' },
  ])
  // The expanded group repeats no round-2 marker; it only marks round 3,
  // which actually begins inside the group.
  expect(await page.locator('.tool-group-body .iteration-marker').allTextContents()).toEqual(['3'])
  // Page order: collapsed group marker, output step marker, body marker.
  expect(await page.locator('.iteration-marker').allTextContents()).toEqual(['1', '2', '3'])
  hold.release([])
})

test('groups render collapsed with visible markers after the run settles', async ({ page }) => {
  const settle = newGate()
  let committed = false
  const turnItems = [
    { seq: 1, id: 'i1', turn_id: 'turn-main', created_at: '2026-01-01T00:00:01Z', kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'user', content: { inline: 'Run tools' } } },
    { seq: 2, id: 'i2', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:02Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }] } },
    { seq: 3, id: 'i3', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:03Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't1', content: { inline: 'a' } } },
    { seq: 4, id: 'i4', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:04Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't2', content: { inline: 'b' } } },
    { seq: 5, id: 'i5', turn_id: 'turn-main', agent_iteration: 2, created_at: '2026-01-01T00:00:05Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't3', name: 'shell', arguments: '{"command":"ls"}' }] } },
    { seq: 6, id: 'i6', turn_id: 'turn-main', agent_iteration: 2, created_at: '2026-01-01T00:00:06Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't3', content: { inline: 'ok' } } },
    { seq: 7, id: 'i7', turn_id: 'turn-main', agent_iteration: 3, created_at: '2026-01-01T00:00:07Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', content: { inline: 'All done.' } } },
  ]
  await mockApp(page, {
    initial: [
      { type: 'turn.started', turn_id: 'turn-main' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 2 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', is_error: false, content: 'ok' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 3 },
      { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 3, text: 'All done.' },
      { type: 'turn.committed', turn_id: 'turn-main', last_seq: 7 },
    ],
    gates: [settle],
    items: () => (committed ? turnItems : []),
  })
  await expect(page.locator('details.tool-group')).toHaveCount(1)

  committed = true
  settle.release([{ type: 'run.settled', run_id: 'run-main', status: 'committed', turn_id: 'turn-main', last_seq: 7 }])
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.assistant:not(.transient)').last()).toContainText('All done.')
  expect(await groupStates(page)).toEqual([{ open: false, summary: 'Read 2 files · Ran 1 command' }])
  await expectMarkerVisible(page)
  // Collapsed group markers hang 11px left of the summary so digits align
  // with the body text edge.
  const marginRight = await page.locator('.tool-group > summary > .iteration-marker').first().evaluate((el) => getComputedStyle(el).marginRight)
  expect(marginRight).toBe('11px')
})

test('failed tools expand the live group but stay collapsed once settled', async ({ page }) => {
  const settle = newGate()
  let committed = false
  const turnItems = [
    { seq: 1, id: 'i1', turn_id: 'turn-main', created_at: '2026-01-01T00:00:01Z', kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'user', content: { inline: 'Run tools' } } },
    { seq: 2, id: 'i2', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:02Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }] } },
    { seq: 3, id: 'i3', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:03Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't1', content: { inline: 'a' } } },
    { seq: 4, id: 'i4', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:04Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't2', is_error: true, content: { inline: 'boom' } } },
    { seq: 5, id: 'i5', turn_id: 'turn-main', agent_iteration: 2, created_at: '2026-01-01T00:00:05Z', kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', content: { inline: 'One read failed.' } } },
  ]
  await mockApp(page, {
    initial: [
      { type: 'turn.started', turn_id: 'turn-main' },
      { type: 'agent.iteration.started', turn_id: 'turn-main', agent_iteration: 1 },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
      { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
      { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: true, content: 'boom' },
    ],
    gates: [settle],
    items: () => (committed ? turnItems : []),
  })
  // The live tail group surfaces the failure by staying expanded.
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  expect(await groupStates(page)).toEqual([{ open: true, summary: 'Read 2 files' }])

  // Once the run settles and the steps reload from history, the group
  // collapses; the failure remains discoverable via the badge.
  committed = true
  settle.release([{ type: 'run.settled', run_id: 'run-main', status: 'committed', turn_id: 'turn-main', last_seq: 5 }])
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.assistant:not(.transient)').last()).toContainText('One read failed.')
  expect(await groupStates(page)).toEqual([{ open: false, summary: 'Read 2 files' }])
  await expect(page.locator('.tool-group > summary small')).toHaveText('1 failed')
})
