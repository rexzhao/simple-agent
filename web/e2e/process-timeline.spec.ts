import { expect, test, type Locator, type Page } from '@playwright/test'
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

test('iteration markers are painted next to flat live tool rows', async ({ page }) => {
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
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.tool-row')).toHaveCount(3)
  await expectMarkerVisible(page)
  expect(await page.locator('.iteration-marker').allTextContents()).toEqual(['1', '2'])
  hold.release([])
})

test('typed live tool and reasoning rows remain in event order around streamed text', async ({ page }) => {
  const hold = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
    { type: 'assistant.message.updated', turn_id: 'turn-main', agent_iteration: 2, item_id: 'live', message_revision: '1', content: 'I have both files, now editing. ', tool_calls: [] },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'edit_file', arguments: '{"path":"a.ts"}' },
    { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'edit_file', is_error: false, content: 'edited' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't4', name: 'edit_file', arguments: '{"path":"b.ts"}' },
  ], [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.tool-row')).toHaveCount(4)
  await expect(page.locator('.assistant-stream').last()).toContainText('I have both files, now editing.')
  expect(await page.locator('.iteration-marker').allTextContents()).toEqual(['1', '2'])
  hold.release([])
})

test('tool rows without intermediate text remain individually visible', async ({ page }) => {
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
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.tool-row')).toHaveCount(3)
  await expect(page.locator('.tool-row.requested')).toHaveCount(1)
  hold.release([])
})

test('running tool dots have a visible green pulse until the tool finishes', async ({ page }) => {
  const running = newGate()
  const finished = newGate()
  const settle = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'shell', arguments: '{"command":"echo hi"}' },
  ], [running, finished, settle])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run a tool')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.tool-row.requested')).toBeVisible()

  running.release([{ type: 'tool.running', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'shell' }])
  await expect(page.locator('.tool-row.running')).toBeVisible()
  const dot = page.locator('.tool-status-dot.running')
  await expect.poll(async () => dot.evaluate((element) => {
    const style = getComputedStyle(element)
    return { animationName: style.animationName, animationIterationCount: style.animationIterationCount, color: style.color, backgroundColor: style.backgroundColor }
  })).toEqual({ animationName: 'tool-status-pulse', animationIterationCount: 'infinite', color: 'rgb(21, 128, 61)', backgroundColor: 'rgb(21, 128, 61)' })

  const firstFrame = await dot.evaluate((element) => {
    const style = getComputedStyle(element)
    return { opacity: style.opacity, transform: style.transform, boxShadow: style.boxShadow }
  })
  await expect.poll(async () => dot.evaluate((element, baseline) => {
    const style = getComputedStyle(element)
    return style.opacity !== baseline.opacity || style.transform !== baseline.transform || style.boxShadow !== baseline.boxShadow
  }, firstFrame)).toBe(true)

  finished.release([{ type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'shell', is_error: false, content: 'hi' }])
  await expect(page.locator('.tool-row.finished')).toBeVisible()
  await expect.poll(async () => page.locator('.tool-status-dot.finished').evaluate((element) => getComputedStyle(element).animationName)).toBe('none')
  settle.release([])
})

test('round markers remain visible on flat tool rows', async ({ page }) => {
  const hold = newGate()
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: false, content: 'b' },
    { type: 'assistant.message.updated', turn_id: 'turn-main', agent_iteration: 2, item_id: 'live-2', message_revision: '1', content: 'Round two. ', tool_calls: [] },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', is_error: false, content: 'ok' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't4', name: 'shell', arguments: '{"command":"pwd"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't4', name: 'shell', is_error: false, content: 'ok' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 3, tool_call_id: 't5', name: 'shell', arguments: '{"command":"git status"}' },
  ], [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.tool-row')).toHaveCount(5)
  expect(await page.locator('.iteration-marker').allTextContents()).toEqual(['1', '2', '3'])
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

test('settled tool calls render as separate rows with visible markers', async ({ page }) => {
  const settle = newGate()
  let committed = false
  const server = await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', arguments: '{"command":"ls"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 2, tool_call_id: 't3', name: 'shell', is_error: false, content: 'ok' },
    { type: 'assistant.message.updated', turn_id: 'turn-main', agent_iteration: 3, item_id: 'live', message_revision: '1', content: 'All done.', tool_calls: [] },
  ], [settle], () => committed ? settledToolItems() : [])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.tool-row')).toHaveCount(2)
  committed = true
  settle.release([])
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.assistant:not(.transient)').last()).toContainText('All done.')
  await expect(page.locator('.process-message .tool-row')).toHaveCount(3)
  await expectMarkerVisible(page)
  expect(await page.locator('.iteration-marker').allTextContents()).toEqual(['1', '2'])
  void server
})

test('failed tool rows stay individually visible once settled', async ({ page }) => {
  const settle = newGate()
  let committed = false
  await installTimeline(page, [
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', arguments: '{"path":"a.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't1', name: 'read_file', is_error: false, content: 'a' },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', arguments: '{"path":"b.ts"}' }, { type: 'tool.finished', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 't2', name: 'read_file', is_error: true, content: 'boom' },
  ], [settle], () => committed ? settledToolItems(true) : [])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Run tools')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.tool-group')).toHaveCount(0)
  await expect(page.locator('.tool-row')).toHaveCount(2)
  committed = true
  settle.release([])
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.assistant:not(.transient)').last()).toContainText('One read failed.')
  await expect(page.locator('.tool-row.error')).toHaveCount(1)
})

test('anchors clicked tool and reasoning popovers to the pointer while preserving vertical placement', async ({ page }) => {
  const hold = newGate()
  const initial = [
    { type: 'assistant.message.updated', turn_id: 'turn-main', agent_iteration: 1, item_id: 'reasoning-below', message_revision: '1', content: '', reasoning: 'reasoning details', tool_calls: [] },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 'tool-below', name: 'shell', arguments: '{"command":"tool details"}' },
    ...Array.from({ length: 14 }, (_, index) => ({ type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: `tool-filler-${index}`, name: 'shell', arguments: `{"command":"filler ${index}"}` })),
    { type: 'assistant.message.updated', turn_id: 'turn-main', agent_iteration: 1, item_id: 'reasoning-above', message_revision: '1', content: '', reasoning: 'reasoning details above', tool_calls: [] },
    { type: 'tool.requested', turn_id: 'turn-main', agent_iteration: 1, tool_call_id: 'tool-above', name: 'shell', arguments: '{"command":"tool details above"}' },
  ]
  await installTimeline(page, initial, [hold])
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Measure hover geometry')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.locator('.reasoning-trigger')).toHaveCount(2)
  await expect(page.locator('.tool-row-header')).toHaveCount(16)
  const reasoningBelow = page.locator('.reasoning-trigger').nth(0)
  const targetTool = page.locator('.tool-row-header').nth(0)
  const box = async (locator: Locator) => {
    const result = await locator.boundingBox()
    if (!result) throw new Error(`Missing visible box for ${await locator.evaluate((element) => element.className)}`)
    return result
  }
  const geometry = async (trigger: Locator, outer: Locator) => ({ trigger: await box(trigger), outer: await box(outer), popup: await box(page.locator('.process-hover-popover')) })

  await reasoningBelow.hover()
  await expect(page.locator('.process-hover-popover')).toHaveCount(0)
  await reasoningBelow.click({ position: { x: 40, y: 8 } })
  await expect(page.locator('.process-hover-popover')).toBeVisible()
  const reasoningGeometry = await geometry(reasoningBelow, page.locator('.reasoning-step').nth(0))
  const reasoningPointer = await reasoningBelow.evaluate((element) => {
    const rect = element.getBoundingClientRect()
    return { x: rect.left + 40, y: rect.top + 8 }
  })
  expect(reasoningGeometry.popup.x).toBe(reasoningPointer.x + 14)
  expect(reasoningGeometry.popup.y).toBeGreaterThan(reasoningGeometry.trigger.y + reasoningGeometry.trigger.height)

  await targetTool.hover()
  await expect(page.locator('.process-hover-popover')).toContainText('reasoning details')
  await targetTool.click({ position: { x: 40, y: 8 } })
  await expect(page.locator('.process-hover-popover')).toContainText('tool details')
  const toolGeometry = await geometry(targetTool, page.locator('.tool-row').nth(0))
  const toolPointer = await targetTool.evaluate((element) => {
    const rect = element.getBoundingClientRect()
    return { x: rect.left + 40, y: rect.top + 8 }
  })
  expect(toolGeometry.popup.x).toBe(toolPointer.x + 14)
  expect(toolGeometry.popup.y).toBeGreaterThan(toolGeometry.trigger.y + toolGeometry.trigger.height)

  await page.locator('.process-hover-popover').evaluate((element) => element.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true })))
  await page.waitForTimeout(220)
  await page.locator('.messages').evaluate((element) => { element.scrollTop = element.scrollHeight - element.clientHeight })
  const reasoningAbove = page.locator('.reasoning-trigger').nth(1)
  await reasoningAbove.hover()
  await expect(page.locator('.process-hover-popover')).toHaveCount(0)
  await reasoningAbove.click({ position: { x: 40, y: 8 } })
  await expect(page.locator('.process-hover-popover')).toHaveAttribute('data-placement', 'above')
  const reasoningAboveGeometry = await geometry(reasoningAbove, page.locator('.reasoning-step').nth(1))
  const toolAbove = page.locator('.tool-row-header').nth(15)
  await toolAbove.hover()
  await expect(page.locator('.process-hover-popover')).toContainText('reasoning details above')
  await toolAbove.click({ position: { x: 40, y: 8 } })
  await expect(page.locator('.process-hover-popover')).toHaveAttribute('data-placement', 'above')
  const toolAboveGeometry = await geometry(toolAbove, page.locator('.tool-row').nth(15))
  expect(reasoningAboveGeometry.popup.y + reasoningAboveGeometry.popup.height).toBeLessThanOrEqual(reasoningAboveGeometry.trigger.y)
  expect(toolAboveGeometry.popup.y + toolAboveGeometry.popup.height).toBeLessThanOrEqual(toolAboveGeometry.trigger.y)
  hold.release([])
})
