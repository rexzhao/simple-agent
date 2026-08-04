import { expect, test, type Page, type Route } from '@playwright/test'

const project = { id: 'project-1', root: '/fixture', display_name: 'Fixture', archived: false, created_at: '', updated_at: '' }
const session = { id: 'session-1', project_id: project.id, display_name: 'History fixture', provider: 'fake', model_profile: 'fake', model_id: 'fake', status: 'idle', created_at: '', updated_at: '', last_used_at: '', created_cwd: '', last_seq: 150, archived: false }

function items(from: number, to: number) {
  return Array.from({ length: to - from + 1 }, (_, index) => {
    const seq = from + index
    const long = seq % 7 === 0
    return {
      seq, id: `item-${seq}`, created_at: new Date(seq * 1000).toISOString(), kind: 'message', visibility: 'normal', audience: 'user',
      message: { role: seq % 2 ? 'user' : 'assistant', content: { inline: long ? `message-${seq}\n\n${'A deliberately tall history line. '.repeat(35)}` : `message-${seq}` } },
    }
  })
}

async function mockApp(page: Page, olderGate?: Promise<void>) {
  let olderCalls = 0
  await page.route('**/api/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    let body: unknown
    if (url.pathname === '/api/bootstrap') body = { version: 'e2e', cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' }
    else if (url.pathname === '/api/projects') body = { projects: [project] }
    else if (url.pathname === `/api/projects/${project.id}/sessions`) body = { sessions: url.searchParams.get('archived') === 'true' ? [] : [session] }
    else if (url.pathname === '/api/runs/active') body = { runs: [] }
    else if (url.pathname === `/api/sessions/${session.id}`) body = session
    else if (url.pathname === `/api/sessions/${session.id}/snapshot`) body = { session_id: session.id, revision: String(session.last_seq), session, history: { items: items(101, 150), oldest_seq: 101, newest_seq: 150, has_more_before: true, has_more_after: false } }
    else if (url.pathname === `/api/sessions/${session.id}/items`) {
      if (url.searchParams.has('before_seq')) {
        olderCalls++
        await olderGate
        body = { items: items(51, 100), oldest_seq: 51, newest_seq: 100, has_more_before: false, has_more_after: false }
      } else body = { items: items(101, 150), oldest_seq: 101, newest_seq: 150, has_more_before: true, has_more_after: false }
    } else return route.fulfill({ status: 404, json: {} })
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
  })
  return () => olderCalls
}

async function twoFrames(page: Page) {
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
}

test('loads older history only on click and preserves the viewport anchor', async ({ page }) => {
  let release!: () => void
  const gate = new Promise<void>((resolve) => { release = resolve })
  const olderCalls = await mockApp(page, gate)
  await page.goto('/')
  const messages = page.locator('.messages')
  const anchor = page.getByText('message-101', { exact: true })

  // Scrolling to the top must not page older history by itself.
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(anchor).toBeAttached()
  await twoFrames(page)
  // Virtuoso may finish its initial measurement by adjusting the scroll
  // position. Re-apply the real top gesture and poll the anchor at that point,
  // rather than assuming the first mount stayed in the DOM.
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect.poll(async () => messages.evaluate((element) => element.scrollTop)).toBe(0)
  await expect.poll(async () => anchor.count()).toBeGreaterThan(0)
  expect(olderCalls()).toBe(0)

  const before = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect.poll(olderCalls).toBe(1)
  release()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  // The old first row is the logical anchor. The newly prepended rows may be
  // above the viewport and therefore need not be mounted yet.
  await expect.poll(async () => anchor.count()).toBeGreaterThan(0)
  await twoFrames(page)
  const after = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  expect(Math.abs(after - before)).toBeLessThanOrEqual(2)
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(page.getByText('message-51', { exact: true })).toBeAttached()
  expect(olderCalls()).toBe(1)
})

test('requests history pages with turn alignment enabled', async ({ page }) => {
  const itemQueries: string[] = []
  await page.route('**/api/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    if (url.pathname === '/api/bootstrap') return json(route, { version: 'e2e', cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' })
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [session] })
    if (url.pathname === '/api/runs/active') return json(route, { runs: [] })
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, session)
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) {
      itemQueries.push('?align_turn=true')
      return json(route, { session_id: session.id, revision: String(session.last_seq), session, history: { items: items(101, 150), oldest_seq: 101, newest_seq: 150, has_more_before: true, has_more_after: false } })
    }
    if (url.pathname === `/api/sessions/${session.id}/items`) {
      itemQueries.push(url.search)
      if (url.searchParams.has('before_seq')) {
        return json(route, { items: items(51, 100), oldest_seq: 51, newest_seq: 100, has_more_before: false, has_more_after: false })
      }
      return json(route, { items: items(101, 150), oldest_seq: 101, newest_seq: 150, has_more_before: true, has_more_after: false })
    }
    return json(route, { error: { code: 'not_mocked', message: `${route.request().method()} ${url.pathname}` } }, 404)
  })
  await page.goto('/')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  // The latest page (initial load/refresh) and the older page (click) must
  // both ask the server for turn-aligned pages.
  expect(itemQueries.length).toBeGreaterThanOrEqual(2)
  for (const query of itemQueries) {
    expect(new URLSearchParams(query).get('align_turn')).toBe('true')
  }
})

test('virtualized rows do not cause a second jump when older messages become visible', async ({ page }) => {
  await mockApp(page, Promise.resolve())
  await page.goto('/')
  const messages = page.locator('.messages')
  const anchor = page.getByText('message-101', { exact: true })
  const mountedRow = page.locator('.message').filter({ hasText: 'message-150' }).first()
  await expect(mountedRow).toBeAttached()
  await expect(mountedRow).not.toHaveCSS('content-visibility', 'auto')
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(anchor).toBeAttached()
  const before = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  await expect.poll(async () => anchor.count()).toBeGreaterThan(0)
  await twoFrames(page)
  const after = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  expect(Math.abs(after - before)).toBeLessThanOrEqual(2)
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(page.getByText('message-51', { exact: true })).toBeAttached()
})

// --- Streaming harness ----------------------------------------------------

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

const runDescriptor = { run_id: 'run-1', session_id: session.id, turn_id: 'turn-1', started_at: new Date(0).toISOString(), status: 'running' }

// mockStreamingApp serves the shell app plus a controllable run event stream.
// The first events connection immediately serves `initialEvents`; later
// connections (the client reconnects when a stream ends) wait on gates.
async function mockStreamingApp(
  page: Page,
  options: {
    initialEvents: Array<Record<string, unknown>>
    gates: Gate[]
    activeRuns?: () => Array<Record<string, unknown>>
    latestItems?: () => Array<Record<string, unknown>>
    hasMoreBefore?: boolean
  },
) {
  let connection = 0
  await page.route('**/api/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    if (url.pathname === '/api/bootstrap') return json(route, { version: 'e2e', cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' })
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [session] })
    if (url.pathname === '/api/runs/active') return json(route, { runs: options.activeRuns?.() ?? [] })
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, session)
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) {
      const latest = options.latestItems?.() ?? items(101, 150)
      const seqs = latest.map((item) => Number((item as { seq?: number }).seq ?? 0)).filter(Boolean)
      return json(route, { session_id: session.id, revision: String(seqs.at(-1) ?? session.last_seq), session: { ...session, last_seq: seqs.at(-1) ?? session.last_seq }, history: { items: latest, oldest_seq: seqs[0] ?? 0, newest_seq: seqs.at(-1) ?? 0, has_more_before: options.hasMoreBefore ?? true, has_more_after: false } })
    }
    if (url.pathname === `/api/sessions/${session.id}/items`) {
      if (url.searchParams.has('before_seq')) {
        return json(route, { items: items(51, 100), oldest_seq: 51, newest_seq: 100, has_more_before: false, has_more_after: false })
      }
      const latest = options.latestItems?.() ?? items(101, 150)
      const seqs = latest.map((item) => Number((item as { seq?: number }).seq ?? 0)).filter(Boolean)
      return json(route, { items: latest, oldest_seq: seqs[0] ?? 0, newest_seq: seqs.at(-1) ?? 0, has_more_before: options.hasMoreBefore ?? true, has_more_after: false })
    }
    if (url.pathname === `/api/sessions/${session.id}/runs` && request.method() === 'POST') {
      return json(route, { run_id: runDescriptor.run_id, session_id: session.id, status: 'running' }, 202)
    }
    if (url.pathname === `/api/runs/${runDescriptor.run_id}/events`) {
      connection++
      if (connection === 1) return route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse(options.initialEvents) })
      const gate = options.gates[Math.min(connection - 2, options.gates.length - 1)] ?? newGate()
      const events = await gate.promise
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse(events, 100 * connection) })
    }
    return json(route, { error: { code: 'not_mocked', message: `${request.method()} ${url.pathname}` } }, 404)
  })
}

const distanceFromBottom = (page: Page) =>
  page.locator('.messages').evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight)

// Virtuoso may perform a few measurement passes after a large jump. Wait for
// those passes to finish before measuring positions.
async function settleViewport(page: Page) {
  await expect.poll(async () => {
    const before = await page.locator('.messages').evaluate((element) => element.scrollTop)
    await page.waitForTimeout(120)
    const after = await page.locator('.messages').evaluate((element) => element.scrollTop)
    return Math.abs(after - before)
  }).toBeLessThanOrEqual(2)
}

// A tiny wheel gesture establishes user intent, exactly like a real scroll.
// Programmatic scrolls that follow then behave like deliberate positioning
// instead of being mistaken for the initial bottom placement.
async function interruptSettle(page: Page) {
  await page.locator('.messages').hover()
  await page.mouse.wheel(0, -1)
}

test('stops following output when the user scrolls up, resumes at the bottom', async ({ page }) => {
  const more = newGate()
  const finale = newGate()
  await mockStreamingApp(page, {
    initialEvents: [
      { type: 'turn.started', turn_id: 'turn-1' },
      { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'First chunk. ' },
    ],
    gates: [more, finale],
  })
  await page.goto('/')
  const messages = page.locator('.messages')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()

  // Start a run from the composer; the first streamed chunk renders.
  await page.getByPlaceholder('Send a message to SAI').fill('hi')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByText(/First chunk/)).toBeAttached()

  // Scroll up: output following must disengage. Loading older history needs
  // a click, so scrolling alone cannot grow the history window here.
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop -= 900 })
  await settleViewport(page)
  const scrolledTo = await messages.evaluate((element) => element.scrollTop)

  // New streamed output must not move the viewport.
  more.release([{ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'Second chunk, deliberately padded. '.repeat(20) }])
  await twoFrames(page)
  const afterGrowth = await messages.evaluate((element) => element.scrollTop)
  expect(Math.abs(afterGrowth - scrolledTo)).toBeLessThanOrEqual(4)

  // Scrolling back to the bottom re-engages following.
  await messages.evaluate((element) => { element.scrollTop = element.scrollHeight })
  await expect(page.getByText(/Second chunk/)).toBeAttached()
  await twoFrames(page)
  finale.release([{ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'Final chunk. ' }])
  await expect(page.getByText(/Final chunk/)).toBeAttached()
  await expect.poll(() => distanceFromBottom(page)).toBeLessThanOrEqual(12)
})

test('sending a message while scrolled up snaps back to the bottom', async ({ page }) => {
  await mockStreamingApp(page, {
    initialEvents: [{ type: 'turn.started', turn_id: 'turn-1' }],
    gates: [],
  })
  await page.goto('/')
  const messages = page.locator('.messages')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()

  // Deep in history, far from both edges: no load, no follow.
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop = element.scrollHeight / 2 })
  await twoFrames(page)
  expect(await distanceFromBottom(page)).toBeGreaterThan(100)

  await page.getByPlaceholder('Send a message to SAI').fill('follow up')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect.poll(() => distanceFromBottom(page)).toBeLessThanOrEqual(12)
  await expect(page.getByText('follow up')).toBeAttached()
})

test('a settling run keeps loaded older history and the viewport anchor', async ({ page }) => {
  const settle = newGate()
  let settled = false
  await mockStreamingApp(page, {
    initialEvents: [
      { type: 'turn.started', turn_id: 'turn-1' },
      { type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, text: 'Working… ' },
    ],
    gates: [settle],
    activeRuns: () => settled ? [] : [runDescriptor],
    latestItems: () => settled
      ? [...items(101, 150), ...items(151, 152).map((item) => ({ ...item, turn_id: 'turn-1' }))]
      : items(101, 150),
  })
  await page.goto('/')
  const messages = page.locator('.messages')
  const anchor = page.getByText('message-101', { exact: true })

  // Page older history in with an explicit click.
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(anchor).toBeAttached()
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  await expect(anchor).toBeAttached()
  await settleViewport(page)
  const before = await anchor.evaluate((element) => element.getBoundingClientRect().top)

  // The run settles in the background: the refresh must merge the new tail
  // into the loaded window instead of replacing it.
  settle.release([{ type: 'run.settled', turn_id: 'turn-1', status: 'committed' }])
  settled = true
  await twoFrames(page)
  await expect.poll(async () => anchor.count()).toBeGreaterThan(0)
  const after = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  expect(Math.abs(after - before)).toBeLessThanOrEqual(2)

  // The newly committed tail may be virtualized away while the viewport is
  // held on the older anchor. Mount it explicitly before asserting its text.
  await messages.evaluate((element) => { element.scrollTop = element.scrollHeight })
  await expect(page.getByText('message-152', { exact: true })).toBeAttached()
})

test('opens a tall session at the bottom without paging older history', async ({ page }) => {
  const olderCalls = await mockApp(page, Promise.resolve())
  await page.goto('/')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await settleViewport(page)
  // Initial placement must converge to the real bottom, not an estimate-only
  // position before the variable-height rows have been measured.
  expect(await distanceFromBottom(page)).toBeLessThanOrEqual(12)
  // Landing at the bottom must not load older history.
  expect(olderCalls()).toBe(0)
})

const sessionB = { ...session, id: 'session-2', display_name: 'Second session', last_seq: 3 }

async function mockTwoSessionApp(page: Page) {
  await page.route('**/api/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/events') {
      await new Promise<never>(() => {})
      return
    }
    if (url.pathname === '/api/bootstrap') return json(route, { version: 'e2e', cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' })
    if (url.pathname === '/api/projects') return json(route, { projects: [project] })
    if (url.pathname === `/api/projects/${project.id}/sessions`) return json(route, { sessions: url.searchParams.get('archived') === 'true' ? [] : [session, sessionB] })
    if (url.pathname === '/api/runs/active') return json(route, { runs: [] })
    if (url.pathname === `/api/sessions/${session.id}`) return json(route, session)
    if (url.pathname === `/api/sessions/${sessionB.id}`) return json(route, sessionB)
    if (url.pathname === `/api/sessions/${session.id}/snapshot`) return json(route, { session_id: session.id, revision: String(session.last_seq), session, history: { items: items(101, 150), oldest_seq: 101, newest_seq: 150, has_more_before: false, has_more_after: false } })
    if (url.pathname === `/api/sessions/${sessionB.id}/snapshot`) return json(route, { session_id: sessionB.id, revision: String(sessionB.last_seq), session: sessionB, history: { items: items(1, 3), oldest_seq: 1, newest_seq: 3, has_more_before: false, has_more_after: false } })
    if (url.pathname === `/api/sessions/${session.id}/items`) {
      return json(route, { items: items(101, 150), oldest_seq: 101, newest_seq: 150, has_more_before: false, has_more_after: false })
    }
    if (url.pathname === `/api/sessions/${sessionB.id}/items`) {
      return json(route, { items: items(1, 3), oldest_seq: 1, newest_seq: 3, has_more_before: false, has_more_after: false })
    }
    return json(route, { error: { code: 'not_mocked', message: `${route.request().method()} ${url.pathname}` } }, 404)
  })
}

// The first message whose bottom edge is below the container top — the same
// anchor definition the scroll memory uses.
function firstVisible(page: Page) {
  return page.locator('.messages').evaluate((element) => {
    const containerTop = element.getBoundingClientRect().top
    const anchor = [...element.querySelectorAll<HTMLElement>('.message[data-seq]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > containerTop)
    return anchor ? { seq: anchor.dataset.seq, offset: anchor.getBoundingClientRect().top - containerTop } : null
  })
}

test('restores the previous scroll position when switching back to a session', async ({ page }) => {
  await mockTwoSessionApp(page)
  await page.goto('/')
  await page.getByRole('button', { name: /^History fixture/ }).click()
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await settleViewport(page)

  // Scroll deep into the middle of the history.
  await interruptSettle(page)
  await page.locator('.messages').evaluate((element) => { element.scrollTop = element.scrollHeight / 2 })
  await settleViewport(page)
  const remembered = await firstVisible(page)
  expect(remembered).not.toBeNull()
  expect(await distanceFromBottom(page)).toBeGreaterThan(100)

  // Switch away and back: the cached conversation re-anchors on the
  // remembered message instead of snapping to the bottom.
  await page.getByRole('button', { name: /^Second session/ }).click()
  await expect(page.getByText('message-1', { exact: true })).toBeAttached()
  await page.getByRole('button', { name: /^History fixture/ }).click()
  await settleViewport(page)
  await expect.poll(async () => (await firstVisible(page))?.seq).toBe(remembered?.seq)
  const restored = await firstVisible(page)
  expect(restored?.seq).toBe(remembered?.seq)
  expect(Math.abs((restored?.offset ?? 0) - (remembered?.offset ?? 0))).toBeLessThanOrEqual(4)
  expect(await distanceFromBottom(page)).toBeGreaterThan(100)
})

test('loads finished sessions with failed tool groups collapsed', async ({ page }) => {
  const stamp = (seq: number) => new Date(seq * 1000).toISOString()
  const toolItems = [
    { seq: 101, id: 'i1', turn_id: 'turn-1', created_at: stamp(101), kind: 'message', visibility: 'normal', audience: 'user', message: { role: 'user', content: { inline: 'Run tools' } } },
    { seq: 102, id: 'i2', turn_id: 'turn-1', agent_iteration: 1, created_at: stamp(102), kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }] } },
    { seq: 103, id: 'i3', turn_id: 'turn-1', agent_iteration: 1, created_at: stamp(103), kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't1', content: { inline: 'a' } } },
    { seq: 104, id: 'i4', turn_id: 'turn-1', agent_iteration: 1, created_at: stamp(104), kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'tool', tool_call_id: 't2', is_error: true, content: { inline: 'boom' } } },
    { seq: 105, id: 'i5', turn_id: 'turn-1', agent_iteration: 2, created_at: stamp(105), kind: 'message', visibility: 'normal', audience: 'model', message: { role: 'assistant', content: { inline: 'One read failed.' } } },
  ]
  await mockStreamingApp(page, { initialEvents: [], gates: [], latestItems: () => toolItems, hasMoreBefore: false })
  await page.goto('/')
  // Failed members expand a live group, but a finished session loads its
  // groups collapsed — the failure stays discoverable via the badge.
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  expect(await page.locator('details.tool-group').evaluate((element) => (element as HTMLDetailsElement).open)).toBe(false)
  await expect(page.locator('.tool-group-summary')).toHaveText('Read 2 files')
  await expect(page.locator('.tool-group > summary small')).toHaveText('1 failed')
})
