import { expect, test, type Page } from '@playwright/test'
import { installSyncMock, messageItem, type SyncMockServer, type WireProject, type WireSession } from './ws-fixture'

const project: WireProject = { id: 'project-main', root: '/workspace/project', display_name: 'Main project', archived: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }
const session: WireSession = { id: 'session-main', project_id: project.id, display_name: 'Primary session', provider: 'fake', model_profile: 'fast', model_id: 'fake-model', reasoning_level: 'high', status: 'idle', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', last_used_at: '2026-01-01T00:00:00Z', created_cwd: project.root, last_seq: 0, archived: false, show_reasoning: true }

type Gate = {
  release: (events: Array<Record<string, unknown>>) => void
  promise: Promise<Array<Record<string, unknown>>>
}

function newGate(): Gate {
  let release!: (events: Array<Record<string, unknown>>) => void
  const promise = new Promise<Array<Record<string, unknown>>>((resolve) => {
    release = resolve
  })
  return { release, promise }
}

function runFixture(initial: Array<Record<string, unknown>>, gates: Gate[], settledItems?: () => Record<string, unknown>[]) {
  return (server: SyncMockServer, command: { name: string; arguments: Record<string, unknown> }) => {
    if (command.name !== 'run.start') return
    void (async () => {
      await new Promise<void>((resolve) => setTimeout(resolve, 0))
      const sessionID = String(command.arguments.session_id)
      const runID = String(command.arguments.run_id)
      server.sendEvents(sessionID, runID, initial.map((event) => ({ ...event, type: String(event.type) })))
      for (const gate of gates) server.sendEvents(sessionID, runID, (await gate.promise).map((event) => ({ ...event, type: String(event.type) })))
      server.settleRun(sessionID, runID, 'committed', settledItems?.() ?? [])
    })()
  }
}

async function installTimeline(page: Page, initial: Array<Record<string, unknown>>, gates: Gate[], settledItems?: () => Record<string, unknown>[]) {
  return installSyncMock(page, { projects: [project], sessions: [session], onCommand: runFixture(initial, gates, settledItems) })
}

function groupStates(page: Page) { return page.locator('details.tool-group').evaluateAll((els) => els.map((el) => ({ open: (el as HTMLDetailsElement).open, summary: el.querySelector('.tool-group-summary')?.textContent }))) }

async function expectMarkerVisible(page: Page) {
  const marker = page.locator('.iteration-marker').first()
  await expect(marker).toBeAttached()
  const probe = await marker.evaluate((element) => {
    const el = element as HTMLElement
    el.style.pointerEvents = 'auto'
    el.scrollIntoView({ block: 'center' })
    const rect = el.getBoundingClientRect()
    const hit = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2)
    return hit ? `${hit.tagName}.${hit.className}` : 'null'
  })
  expect(probe).toBe('I.iteration-marker')
}

test('iteration markers are painted next to live tool groups', async ({ page }) => {
  const hold = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' },
  ], [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  await expectMarkerVisible(page)
  expect(await page.locator('.tool-group-body .iteration-marker').allTextContents()).toEqual(['1', '2'])
  expect(await page.locator('.tool-group-body .iteration-marker').first().evaluate((el) => getComputedStyle(el).marginRight)).toBe('21px')
  hold.release([])
})

test('typed live tool batches remain coherent around streamed text', async ({ page }) => {
  const hold = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
    { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 2, item_id: 'live', delta: 'I have both files, now editing. ' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'edit_file', arguments: '{"path":"a.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'edit_file', is_error: false, content: 'edited' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't4', name: 'edit_file', arguments: '{"path":"b.ts"}' },
  ], [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('details.tool-group')).toHaveCount(2)
  await expect(page.locator('.assistant-stream').last()).toContainText('I have both files, now editing.')
  expect(await groupStates(page)).toEqual([
    { open: false, summary: 'Read 2 files' },
    { open: true, summary: 'Edited 2 files' },
  ])
  expect(await page.locator('.iteration-marker').allTextContents()).toEqual(['1', '2'])
  hold.release([])
})

test('tool batches without intermediate text keep the live group open', async ({ page }) => {
  const hold = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' },
  ], [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  await page.waitForTimeout(200)
  expect(await groupStates(page)).toEqual([{ open: true, summary: 'Read 2 files · Ran 1 command' }])
  hold.release([])
})

test('round markers remain visible when typed tool batches share one live group', async ({ page }) => {
  const hold = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
    { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 2, item_id: 'live-2', delta: 'Round two. ' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', is_error: false, content: 'ok' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't4', name: 'shell', arguments: '{"command":"pwd"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't4', name: 'shell', is_error: false, content: 'ok' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't5', name: 'shell', arguments: '{"command":"git status"}' },
  ], [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('details.tool-group')).toHaveCount(2)
  expect(await groupStates(page)).toEqual([
    { open: false, summary: 'Read 2 files' },
    { open: true, summary: 'Ran 3 commands' },
  ])
  expect(await page.locator('.step-message .iteration-marker').allTextContents()).toEqual(['2'])
  expect(await page.locator('details.tool-group').nth(1).locator('.tool-group-body .iteration-marker').allTextContents()).toEqual(['3'])
  hold.release([])
})

function settledToolItems(failed = false) {
  const base = [
    messageItem(1, 'user', 'Run tools', { turn_id: 'turn-main' }),
    { seq: 2, id: 'i2', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:02Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't1', name: 'read_file', arguments: { inline: '{"path":"a.ts"}' } }, { id: 't2', name: 'read_file', arguments: { inline: '{"path":"b.ts"}' } }] } },
    { seq: 3, id: 'i3', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:03Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'tool', tool_call_id: 't1', content: { inline: 'a' } } },
    { seq: 4, id: 'i4', turn_id: 'turn-main', agent_iteration: 1, created_at: '2026-01-01T00:00:04Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'tool', tool_call_id: 't2', is_error: failed, content: { inline: failed ? 'boom' : 'b' } } },
  ]
  if (failed) {
    return [...base, { seq: 5, id: 'i5', turn_id: 'turn-main', agent_iteration: 2, created_at: '2026-01-01T00:00:05Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'One read failed.' } } }]
  }
  return [...base,
    { seq: 5, id: 'i5', turn_id: 'turn-main', agent_iteration: 2, created_at: '2026-01-01T00:00:05Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', tool_calls: [{ id: 't3', name: 'shell', arguments: { inline: '{"command":"ls"}' } }] } },
    { seq: 6, id: 'i6', turn_id: 'turn-main', agent_iteration: 2, created_at: '2026-01-01T00:00:06Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'tool', tool_call_id: 't3', content: { inline: 'ok' } } },
    { seq: 7, id: 'i7', turn_id: 'turn-main', agent_iteration: 3, created_at: '2026-01-01T00:00:07Z', kind: 'message', visibility: 'visible', audience: 'model', message: { role: 'assistant', content: { inline: 'All done.' } } },
  ]
}

test('groups render collapsed with visible markers after the run settles', async ({ page }) => {
  const settle = newGate()
  let committed = false
  const server = await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', is_error: false, content: 'ok' },
    { type: 'text.delta', turn_id: 'turn-main', agent_iteration: 3, item_id: 'live', delta: 'All done.' },
  ], [settle], () => committed ? settledToolItems() : [])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  committed = true
  settle.release([])
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.assistant:not(.transient)').last()).toContainText('All done.')
  expect(await groupStates(page)).toEqual([{ open: false, summary: 'Read 2 files · Ran 1 command' }])
  await expectMarkerVisible(page)
  expect(await page.locator('.tool-group > summary > .iteration-marker').first().evaluate((el) => getComputedStyle(el).marginRight)).toBe('11px')
  void server
})

test('failed tools expand the live group but stay collapsed once settled', async ({ page }) => {
  const settle = newGate()
  let committed = false
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: true, content: 'boom' },
  ], [settle], () => committed ? settledToolItems(true) : [])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('details.tool-group')).toHaveCount(1)
  expect(await groupStates(page)).toEqual([{ open: true, summary: 'Read 2 files' }])
  committed = true
  settle.release([])
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.assistant:not(.transient)').last()).toContainText('One read failed.')
  expect(await groupStates(page)).toEqual([{ open: false, summary: 'Read 2 files' }])
  await expect(page.locator('.tool-group > summary small')).toHaveText('1 failed')
})
