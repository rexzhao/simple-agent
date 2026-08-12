import { expect, test, type Page } from '@playwright/test'
import { installSyncMock, messageItem, type WireProject, type WireSession, type SyncMockServer } from './ws-fixture'

const project: WireProject = { id: 'project-1', root: '/fixture', display_name: 'Fixture', archived: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }
const session: WireSession = { id: 'session-1', project_id: project.id, display_name: 'History fixture', provider: 'fake', model_profile: 'fake', model_id: 'fake', status: 'idle', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', last_used_at: '2026-01-01T00:00:00Z', created_cwd: '/fixture', last_seq: 150, archived: false }

function items(from: number, to: number) {
  return Array.from({ length: to - from + 1 }, (_, index) => {
    const seq = from + index
    const long = seq % 7 === 0
    return messageItem(seq, seq % 2 ? 'user' : 'assistant', long ? `message-${seq}\n\n${'A deliberately tall history line. '.repeat(35)}` : `message-${seq}`)
  })
}

async function installHistory(page: Page, options: { initial?: unknown[]; before?: unknown[]; hasMoreBefore?: boolean } = {}): Promise<SyncMockServer> {
  return installSyncMock(page, {
    projects: [project], sessions: [session],
    contents: {
      [session.id]: {
        items: (options.initial as Record<string, unknown>[] | undefined) ?? items(101, 150),
        historyBefore: (options.before as Record<string, unknown>[] | undefined) ?? items(51, 100),
        hasMoreBefore: options.hasMoreBefore ?? true,
      },
    },
    bootstrap: { cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' },
  })
}

async function twoFrames(page: Page) {
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
}

async function settleViewport(page: Page) {
  await expect.poll(async () => {
    const before = await page.locator('.messages').evaluate((element) => element.scrollTop)
    await page.waitForTimeout(120)
    const after = await page.locator('.messages').evaluate((element) => element.scrollTop)
    return Math.abs(after - before)
  }).toBeLessThanOrEqual(2)
}

async function interruptSettle(page: Page) {
  const messages = page.locator('.messages')
  await expect.poll(() => messages.evaluate((element) => element.scrollHeight - element.clientHeight)).toBeGreaterThan(100)
  await expect.poll(() => messages.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThanOrEqual(12)
  await userScrollToTop(page)
}

async function userScrollToTop(page: Page) {
  const messages = page.locator('.messages')
  await messages.hover()
  for (let attempt = 0; attempt < 20; attempt += 1) {
    await page.mouse.wheel(0, -2000)
    await page.waitForTimeout(100)
    const scrollTop = await messages.evaluate((element) => element.scrollTop)
    if (scrollTop <= 1) return
  }
  await expect.poll(() => messages.evaluate((element) => element.scrollTop)).toBeLessThanOrEqual(1)
}

async function scrollToTopWithUser(page: Page) {
  await userScrollToTop(page)
}

const distanceFromBottom = (page: Page) => page.locator('.messages').evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight)

async function waitForMessageViewportAnchor(page: Page, seq: string) {
  await expect.poll(async () => (await messageViewportAnchor(page, seq)) !== null).toBe(true)
  const anchor = await messageViewportAnchor(page, seq)
  if (!anchor) throw new Error(`message ${seq} was not mounted in the conversation viewport`)
  return anchor
}

test('loads older history only on click and preserves the viewport anchor', async ({ page }) => {
  const server = await installHistory(page)
  await page.goto('/#token=e2e')
  await interruptSettle(page)
  const anchorBefore = await waitForMessageViewportAnchor(page, '101')
  expect(server.commands.filter((command) => command.name === 'session.history.read')).toHaveLength(0)

  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect.poll(() => server.commands.filter((command) => command.name === 'session.history.read').length).toBe(1)
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  await expect.poll(async () => {
    const anchorAfter = await messageViewportAnchor(page, '101')
    return anchorAfter !== null && Math.abs(anchorAfter.offset - (anchorBefore?.offset ?? 0)) <= 2
  }).toBe(true)
  const anchorAfter = await messageViewportAnchor(page, '101')
  expect(anchorAfter).not.toBeNull()
  expect(Math.abs((anchorAfter?.offset ?? 0) - anchorBefore.offset)).toBeLessThanOrEqual(2)
})

test('requests history pages through the typed D1 history window', async ({ page }) => {
  const server = await installHistory(page)
  await page.goto('/#token=e2e')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  const reads = server.commands.filter((command) => command.name === 'session.history.read' && command.arguments.direction === 'before')
  expect(reads.length).toBeGreaterThanOrEqual(1)
  // D1 deliberately rejects turn-aligned history descriptors. The command
  // still carries the explicit false value rather than using the old query
  // parameter/replay path.
  for (const command of reads) expect(command.arguments.align_turn).toBe(false)
})

test('virtualized rows do not cause a second jump when older messages become visible', async ({ page }) => {
  await installHistory(page)
  await page.goto('/#token=e2e')
  const mountedRow = page.locator('.message').filter({ hasText: 'message-150' }).first()
  await expect(mountedRow).toBeAttached()
  await expect(mountedRow).not.toHaveCSS('content-visibility', 'auto')
  await interruptSettle(page)
  const anchorBefore = await waitForMessageViewportAnchor(page, '101')
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  await expect.poll(async () => {
    const anchorAfter = await messageViewportAnchor(page, '101')
    return anchorAfter !== null && Math.abs(anchorAfter.offset - anchorBefore.offset) <= 2
  }).toBe(true)
  await scrollToTopWithUser(page)
  await expect.poll(async () => (await firstVisible(page))?.seq).toBe('51')
  await expect(page.locator('.message[data-seq="51"]')).toBeAttached()
})

type Gate = { release: (events: Array<Record<string, unknown>>) => void; promise: Promise<Array<Record<string, unknown>>> }
function newGate(): Gate {
  let release!: (events: Array<Record<string, unknown>>) => void
  const promise = new Promise<Array<Record<string, unknown>>>((resolve) => { release = resolve })
  return { release, promise }
}

// The run cases use a small command hook factory. It keeps all WebSocket
// protocol framing in ws-fixture.ts while leaving each test's timing gates
// explicit.
function runHook(initial: Array<Record<string, unknown>>, gates: Gate[], finalItems?: () => Record<string, unknown>[]) {
  return (server: SyncMockServer, command: { name: string; arguments: Record<string, unknown> }) => {
    if (command.name !== 'run.start') return
    void (async () => {
      await new Promise<void>((resolve) => setTimeout(resolve, 0))
      const sessionID = String(command.arguments.session_id)
      const runID = String(command.arguments.run_id)
      server.sendEvents(sessionID, runID, initial.map((event) => ({ ...event, type: String(event.type) })))
      for (const gate of gates) server.sendEvents(sessionID, runID, (await gate.promise).map((event) => ({ ...event, type: String(event.type) })))
      if (finalItems) server.settleRun(sessionID, runID, 'committed', finalItems())
    })()
  }
}

test('stops following output when the user scrolls up, resumes at the bottom', async ({ page }) => {
  const more = newGate()
  const finale = newGate()
  const server = await installSyncMock(page, {
    projects: [project], sessions: [session], contents: { [session.id]: { items: items(101, 150), hasMoreBefore: true } },
    onCommand: runHook([{ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'item-live', delta: 'First chunk. ' }], [more, finale]),
  })
  await page.goto('/#token=e2e')
  const messages = page.locator('.messages')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await settleViewport(page)
  await page.getByPlaceholder('Send a message to SAI').fill('hi')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByText(/First chunk/)).toBeAttached()
  more.release([{ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'item-live', delta: 'Second chunk, deliberately padded. '.repeat(20) }])
  await expect(page.getByText(/Second chunk/)).toBeAttached()
  await twoFrames(page)
  await messages.hover()
  await page.mouse.wheel(0, -1000)
  await expect.poll(() => distanceFromBottom(page)).toBeGreaterThan(100)
  await twoFrames(page)
  const awayBeforeFinal = await messages.evaluate((element) => element.scrollTop)
  finale.release([{ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'item-live', delta: 'Final chunk. ' }])
  // New typed output must not move a user who has explicitly left the bottom.
  await twoFrames(page)
  await expect.poll(async () => Math.abs(await messages.evaluate((element) => element.scrollTop) - awayBeforeFinal)).toBeLessThanOrEqual(4)
  // An explicit bottom request re-engages the current session viewport and
  // exposes the final chunk without relying on a replay/snapshot fallback.
  await messages.evaluate((element) => { element.scrollTop = element.scrollHeight })
  await expect.poll(() => distanceFromBottom(page)).toBeLessThanOrEqual(12)
  await expect(page.getByText(/Final chunk/)).toBeAttached()
  expect(server.snapshotCount({ type: 'session_content', id: session.id })).toBe(1)
})

test('sending a message while scrolled up snaps back to the bottom', async ({ page }) => {
  await installSyncMock(page, {
    projects: [project],
    sessions: [session],
    contents: { [session.id]: { items: items(101, 150), hasMoreBefore: true } },
  })
  await page.goto('/#token=e2e')
  const messages = page.locator('.messages')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await settleViewport(page)
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop = element.scrollHeight / 2 })
  await twoFrames(page)
  expect(await distanceFromBottom(page)).toBeGreaterThan(100)
  await page.getByPlaceholder('Send a message to SAI').fill('follow up')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect.poll(() => distanceFromBottom(page)).toBeLessThanOrEqual(12)
  await expect(page.getByText('follow up')).toHaveCount(0)
})

test('a settling run keeps loaded older history and the viewport anchor', async ({ page }) => {
  const settle = newGate()
  let settled = false
  const tail = () => [...items(101, 150), ...items(151, 152).map((item) => ({ ...item, turn_id: 'turn-1' }))]
  const server = await installSyncMock(page, {
    projects: [project], sessions: [session], contents: { [session.id]: { items: items(101, 150), historyBefore: items(51, 100), hasMoreBefore: true } },
    onCommand: (mock, command) => {
      if (command.name !== 'run.start') return
      void (async () => {
        await new Promise<void>((resolve) => setTimeout(resolve, 0))
        const sid = String(command.arguments.session_id)
        const rid = String(command.arguments.run_id)
        mock.sendEvents(sid, rid, [{ type: 'text.delta', turn_id: 'turn-1', agent_iteration: 1, item_id: 'live', delta: 'Working… ' }])
        await settle.promise
        settled = true
        mock.settleRun(sid, rid, 'committed', tail())
      })()
    },
  })
  await page.goto('/#token=e2e')
  await settleViewport(page)
  const messages = page.locator('.messages')
  const anchor = page.getByText('message-101', { exact: true })
  await interruptSettle(page)
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(anchor).toBeAttached()
  await page.getByRole('button', { name: 'Load earlier messages' }).click()
  await expect(page.getByRole('button', { name: 'Load earlier messages' })).toHaveCount(0)
  await settleViewport(page)
  // Sending a new turn intentionally follows its live output. Move away from
  // the bottom after its live row exists, then settlement must retain this
  // independently loaded older-page anchor.
  await page.getByPlaceholder('Send a message to SAI').fill('hi')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByText('Working…', { exact: false })).toBeAttached()
  await twoFrames(page)
  await messages.evaluate((element) => {
    element.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerType: 'mouse', button: 0 }))
    element.scrollTop = Math.max(0, element.scrollHeight - element.clientHeight - 180)
    element.dispatchEvent(new Event('scroll', { bubbles: true }))
    element.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, pointerType: 'mouse', button: 0 }))
  })
  await expect.poll(() => distanceFromBottom(page)).toBeGreaterThan(100)
  await twoFrames(page)
  const anchorBeforeSettlement = await firstVisible(page)
  settle.release([])
  await expect.poll(() => settled).toBe(true)
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await settleViewport(page)
  const anchorAfterSettlement = await firstVisible(page)
  expect(anchorBeforeSettlement).not.toBeNull()
  expect(anchorAfterSettlement).not.toBeNull()
  expect(anchorAfterSettlement?.seq).toBe(anchorBeforeSettlement?.seq)
  expect(Math.abs((anchorAfterSettlement?.offset ?? 0) - (anchorBeforeSettlement?.offset ?? 0))).toBeLessThanOrEqual(2)
  await messages.hover()
  await page.mouse.wheel(0, -10000)
  await expect(page.getByText('message-51', { exact: true })).toBeAttached()
  await messages.evaluate((element) => { element.scrollTop = element.scrollHeight })
  await page.waitForTimeout(150)
  await expect(page.getByText('message-152', { exact: true })).toBeAttached()
})

test('opens a tall session at the bottom without paging older history', async ({ page }) => {
  const server = await installHistory(page)
  await page.goto('/#token=e2e')
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await settleViewport(page)
  expect(await distanceFromBottom(page)).toBeLessThanOrEqual(12)
  expect(server.commands.filter((command) => command.name === 'session.history.read')).toHaveLength(0)
})

const sessionB: WireSession = { ...session, id: 'session-2', display_name: 'Second session', last_seq: 3 }

test('restores the previous scroll position when switching back to a session', async ({ page }) => {
  await installSyncMock(page, { projects: [project], sessions: [session, sessionB], contents: { [session.id]: { items: items(101, 150), hasMoreBefore: false }, [sessionB.id]: { items: items(1, 3), hasMoreBefore: false } } })
  await page.goto('/#token=e2e')
  await page.getByRole('button', { name: /^Session idle History fixture/ }).click()
  await expect(page.getByText('message-150', { exact: true })).toBeAttached()
  await settleViewport(page)
  await interruptSettle(page)
  await page.locator('.messages').evaluate((element) => { element.scrollTop = element.scrollHeight / 2 })
  await settleViewport(page)
  const remembered = await firstVisible(page)
  expect(remembered).not.toBeNull()
  expect(await distanceFromBottom(page)).toBeGreaterThan(100)
  await page.getByRole('button', { name: /^Session idle Second session/ }).click()
  await expect(page.getByText('message-1', { exact: true })).toBeAttached()
  await page.getByRole('button', { name: /^Session idle History fixture/ }).click()
  await settleViewport(page)
  await expect.poll(async () => (await firstVisible(page))?.seq).toBe(remembered?.seq)
  await expect.poll(async () => {
    const restored = await firstVisible(page)
    return restored ? Math.abs(restored.offset - (remembered?.offset ?? 0)) : Number.POSITIVE_INFINITY
  }).toBeLessThanOrEqual(4)
  const restored = await firstVisible(page)
  expect(restored?.seq).toBe(remembered?.seq)
  expect(Math.abs((restored?.offset ?? 0) - (remembered?.offset ?? 0))).toBeLessThanOrEqual(4)
  expect(await distanceFromBottom(page)).toBeGreaterThan(100)
})

test('loads finished sessions with failed tool rows individually visible', async ({ page }) => {
  const stamp = (seq: number) => new Date(seq * 1000).toISOString()
  const toolItems = [
    { seq: 101, id: 'i1', turn_id: 'turn-1', created_at: stamp(101), kind: 'message', visibility: 'visible', audience: 'user', message: { role: 'user', content: { inline: 'Run tools' } } },
    { seq: 102, id: 'i2', turn_id: 'turn-1', agent_iteration: 1, created_at: stamp(102), kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't1', name: 'read_file', arguments: { inline: '{"path":"a.ts"}' } }, { id: 't2', name: 'read_file', arguments: { inline: '{"path":"b.ts"}' } }] } },
    { seq: 103, id: 'i3', turn_id: 'turn-1', agent_iteration: 1, created_at: stamp(103), kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'tool', tool_call_id: 't1', content: { inline: 'a' } } },
    { seq: 104, id: 'i4', turn_id: 'turn-1', agent_iteration: 1, created_at: stamp(104), kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'tool', tool_call_id: 't2', is_error: true, content: { inline: 'boom' } } },
    { seq: 105, id: 'i5', turn_id: 'turn-1', agent_iteration: 2, created_at: stamp(105), kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'One read failed.' } } },
  ]
  await installSyncMock(page, { projects: [project], sessions: [session], contents: { [session.id]: { items: toolItems, hasMoreBefore: false } } })
  await page.goto('/#token=e2e')
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.process-message .tool-row')).toHaveCount(2)
  await expect(page.locator('.process-message .tool-row.error')).toHaveCount(1)
})

function firstVisible(page: Page) {
  return page.locator('.messages').evaluate((element) => {
    const top = element.getBoundingClientRect().top
    const row = [...element.querySelectorAll<HTMLElement>('.message[data-seq]')]
      .find((candidate) => candidate.getBoundingClientRect().bottom > top)
    return row ? { seq: row.dataset.seq, offset: row.getBoundingClientRect().top - top } : null
  })
}

function messageViewportAnchor(page: Page, seq: string) {
  return page.locator('.messages').evaluate((element, targetSeq) => {
    const message = element.querySelector<HTMLElement>(`.message[data-seq="${targetSeq}"]`)
    if (!message) return null
    return { seq: targetSeq, offset: message.getBoundingClientRect().top - element.getBoundingClientRect().top }
  }, seq)
}
