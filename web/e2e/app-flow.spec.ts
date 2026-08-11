import { expect, test } from '@playwright/test'
import { contentItem, installSyncMock, messageItem, type WireProject, type WireSession } from './ws-fixture'

const project: WireProject = {
  id: 'project-main', root: '/workspace/project', display_name: 'Main project', archived: false,
  created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
}

const session: WireSession = {
  id: 'session-main', project_id: project.id, display_name: 'Primary session', provider: 'fake', model_profile: 'fast', model_id: 'fake-model', reasoning_level: 'high', status: 'idle',
  created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', last_used_at: '2026-01-01T00:00:00Z', created_cwd: project.root, last_seq: 0, archived: false,
}
const secondarySession: WireSession = { ...session, id: 'session-secondary', display_name: 'Secondary session' }

const items = (user: string, assistant: string) => [messageItem(1, 'user', user), messageItem(2, 'assistant', assistant)]

function installExisting(page: Parameters<typeof installSyncMock>[0], options: Parameters<typeof installSyncMock>[1] = {}) {
  return installSyncMock(page, { projects: [project], sessions: [session], ...options })
}

test('connects a first project, creates a session, and commits a streamed run', async ({ page }) => {
  let finish!: () => void
  const finished = new Promise<void>((resolve) => { finish = resolve })
  const server = await installSyncMock(page, {
    projects: [],
    onCommand: (mock, command) => {
      if (command.name !== 'run.start') return
      void (async () => {
        await new Promise<void>((resolve) => setTimeout(resolve, 250))
        const sessionID = String(command.arguments.session_id)
        const runID = String(command.arguments.run_id)
        mock.sendEvents(sessionID, runID, [
          { type: 'text.delta', turn_id: 'turn-fixture', agent_iteration: 1, item_id: 'assistant-live', delta: 'Streamed answer' },
        ])
        await finished
        mock.settleRun(sessionID, runID, 'committed', items('Build the feature', 'Streamed answer'))
      })()
    },
  })

  await page.goto('/#token=e2e')
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
  await expect(page.locator('.message.assistant.transient').filter({ hasText: 'Streamed answer' })).toBeVisible()
  await expect(page.locator('.cursor')).toHaveCount(1)

  finish()
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('Streamed answer')
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  const createdSession = server.commands.find((command) => command.name === 'session.create')
  const createdSessionID = String(createdSession?.arguments.session_id ?? '')
  expect(createdSessionID).not.toBe('')
  expect(server.snapshotCount({ type: 'session_content', id: createdSessionID })).toBe(1)
})

test('keeps Queue and Steer prompts above the composer until the run consumes them', async ({ page }) => {
  const runID = 'run-queued-prompts'
  const server = await installExisting(page, {
    contents: {
      [session.id]: {
        activeRun: { run_id: runID, session_id: session.id, turn_id: 'turn-queued-prompts', started_at: '2026-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'queue-epoch', run_cursor: '0', replay_available: false, recovery_required: false },
      },
    },
  })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()

  const composer = page.getByPlaceholder('Append a message to the current run…')
  await composer.fill('queued from e2e')
  await page.getByRole('button', { name: 'Append to current run' }).click()
  await expect(composer).toHaveValue('')
  await expect(page.getByLabel('Queued messages')).toContainText('queued from e2e')
  await expect(page.getByText('Queued', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Promote to steer message' }).click()
  await expect(page.getByText('Steer', { exact: true })).toBeVisible()
  await expect(composer).toHaveValue('')

  server.sendTypedEvent(session.id, runID, { type: 'run.prompt_appended', prompts: ['queued from e2e'] })
  server.sendTypedEvent(session.id, runID, { type: 'run.prompt_queue', prompts: [] })
  await expect(page.getByLabel('Queued messages')).toHaveCount(0)
})

test('hands a durable assistant bubble through checkpointed and transient output', async ({ page }) => {
  let releaseTail!: () => void
  let releaseSettled!: () => void
  const tail = new Promise<void>((resolve) => { releaseTail = resolve })
  const settled = new Promise<void>((resolve) => { releaseSettled = resolve })
  const assistantA = messageItem(2, 'assistant', 'a', { id: 'assistant-stream', turn_id: 'turn-stream' })
  const assistantAB = messageItem(2, 'assistant', 'ab', { id: 'assistant-stream', turn_id: 'turn-stream' })
  const server = await installExisting(page, {
    onCommand: (mock, command) => {
      if (command.name !== 'run.start') return
      void (async () => {
        await new Promise<void>((resolve) => setTimeout(resolve, 0))
        const sessionID = String(command.arguments.session_id)
        const runID = String(command.arguments.run_id)
        mock.publishSessionContentOperations(sessionID, [
          { op: 'item.upsert', item: contentItem(assistantA) },
          { op: 'history.window.descriptor.replace', descriptor: { limit: 200, oldest_item_seq: '2', newest_item_seq: '2', align_turn: false, visible_only: true, has_more_before: false, has_more_after: false } },
        ])
        // The projection change and the transient event are distinct ordered
        // authorities. Let the bounded fixture deliver the durable change
        // before exercising checkpoint de-duplication.
        await new Promise<void>((resolve) => setTimeout(resolve, 0))
        mock.sendEvents(sessionID, runID, [{ type: 'text.delta', turn_id: 'turn-stream', agent_iteration: 1, item_id: 'assistant-stream', delta: 'a', durable_text_length: 1, durable_checkpointed: true }])
        await tail
        mock.sendEvents(sessionID, runID, [{ type: 'text.delta', turn_id: 'turn-stream', agent_iteration: 1, item_id: 'assistant-stream', delta: 'b', durable_text_length: 1, durable_checkpointed: false }])
        await settled
        mock.settleRun(sessionID, runID, 'committed', [messageItem(1, 'user', 'stream durable output'), assistantAB])
      })()
    },
  })
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('stream durable output')
  await page.getByRole('button', { name: 'Send' }).click()

  await expect(page.locator('.message.assistant:not(.transient)')).toHaveCount(1)
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('a')
  releaseTail()
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('ab')
  await expect(page.locator('.message.assistant.transient')).toHaveCount(0)
  releaseSettled()
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('ab')
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  expect(server.snapshotCount({ type: 'session_content', id: session.id })).toBe(1)
})

test('settles a run through the durable projection when the settlement watermark is ahead', async ({ page }) => {
  const repaired = messageItem(2, 'assistant', 'repaired answer', { id: 'repaired-assistant', turn_id: 'turn-lagging' })
  const server = await installExisting(page, {
    onCommand: (mock, command) => {
      if (command.name !== 'run.start') return
      void (async () => {
        const sessionID = String(command.arguments.session_id)
        const runID = String(command.arguments.run_id)
        mock.settleRun(sessionID, runID, 'committed', [repaired])
        // The settlement advertises the durable revision ahead of the
        // unreleased content changes. Releasing the changes backwards is a
        // real sequence gap, not a normal settle-plus-snapshot path.
        mock.releaseChanges({ type: 'session_content', id: sessionID }, 'reverse')
      })()
    },
  })
  server.holdChanges({ type: 'session_content', id: session.id })
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('lagging settlement')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByText('repaired answer')).toBeVisible()
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  // The out-of-order content authority must force a fresh WS snapshot. This
  // proves settlement recovery is driven by the typed sequence boundary.
  await expect.poll(() => server.snapshotCount({ type: 'session_content', id: session.id })).toBe(2)
})

test('keeps a late old-run failure out of a fast-switched session', async ({ page }) => {
  const server = await installSyncMock(page, {
    projects: [project], sessions: [session, secondarySession],
    contents: { [session.id]: { activeRun: { run_id: 'old-run', session_id: session.id, turn_id: 'turn-old', started_at: '2026-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch', run_cursor: '0', replay_available: false, recovery_required: false } } },
  })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()
  await expect.poll(() => server.activeSubscriptionID({ type: 'session_content', id: session.id })).toBeTruthy()
  const oldSubscriptionID = server.activeSubscriptionID({ type: 'session_content', id: session.id })
  await page.getByText(secondarySession.display_name, { exact: true }).click()
  await expect(page.getByRole('heading', { name: secondarySession.display_name })).toBeVisible()
  await expect.poll(() => oldSubscriptionID ? server.isSubscriptionActive(oldSubscriptionID) : false).toBe(false)
  expect(oldSubscriptionID).toBeTruthy()
  expect(server.sendLateTypedEvent(oldSubscriptionID!, session.id, 'old-run', {
    type: 'turn.failed',
    turn_id: 'turn-old',
    code: 'model_http_error',
    message: '429: old run failed late',
  })).toBe(true)
  await expect(page.getByRole('alert')).toHaveCount(0)
  expect(server.snapshotCount({ type: 'session_content', id: secondarySession.id })).toBe(1)
})

test('does not let an ordered Session Index update undo a newer selection', async ({ page }) => {
  const server = await installExisting(page, { sessions: [session, secondarySession] })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()
  await page.getByText(secondarySession.display_name, { exact: true }).click()
  await expect(page.getByRole('heading', { name: secondarySession.display_name })).toBeVisible()
  const index = { type: 'session_index', id: project.id }
  server.holdChanges(index)
  server.updateSessionIndexOnly(session.id, { display_name: 'Delayed main authority' })
  server.updateSessionIndexOnly(secondarySession.id, { display_name: 'New secondary authority' })
  server.releaseChanges(index, 'ordered')
  // Session Index owns navigation labels; the open Session Content authority
  // still owns the selected conversation heading.
  await expect(page.getByRole('heading', { name: secondarySession.display_name })).toBeVisible()
  await expect(page.getByText('New secondary authority', { exact: true })).toBeVisible()
  await expect(page.getByText('Delayed main authority', { exact: true })).toBeVisible()
})

test('does not let a newer Session Index authority refresh get overwritten', async ({ page }) => {
  const server = await installExisting(page, { sessions: [session, secondarySession] })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()
  await page.getByText(secondarySession.display_name, { exact: true }).click()
  await expect(page.getByRole('heading', { name: secondarySession.display_name })).toBeVisible()
  const index = { type: 'session_index', id: project.id }
  server.holdChanges(index)
  server.updateSessionIndexOnly(secondarySession.id, { display_name: 'Newest secondary authority' })
  server.updateSessionIndexOnly(session.id, { display_name: 'Late stale main authority' })
  server.releaseChanges(index, 'reverse')
  await expect(page.getByRole('heading', { name: secondarySession.display_name })).toBeVisible()
  await expect(page.getByText('Newest secondary authority', { exact: true })).toBeVisible()
  await expect(page.getByText('Late stale main authority', { exact: true })).toBeVisible()
  await expect.poll(() => server.snapshotCount(index)).toBe(2)
})

test('does not resurrect a project removed while index updates are in flight', async ({ page }) => {
  page.on('dialog', (dialog) => void dialog.accept())
  const server = await installExisting(page, { sessions: [session, secondarySession] })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()
  const projectSubscriptionID = server.activeSubscriptionID({ type: 'project_index', id: 'server' })
  expect(projectSubscriptionID).toBeTruthy()
  await page.getByRole('button', { name: `Delete ${project.display_name}`, exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Connect your first project' })).toBeVisible()
  await expect(page.getByText(secondarySession.display_name, { exact: true })).toHaveCount(0)
  expect(server.sendLateChange(projectSubscriptionID!, '1', '0', [{ op: 'upsert', key: project.id, value: project }])).toBe(true)
  await expect(page.getByRole('heading', { name: 'Connect your first project' })).toBeVisible()
  await expect(page.getByText(project.display_name, { exact: true })).toHaveCount(0)
})

test('cancels an active run without persisting its transient turn', async ({ page }) => {
  const server = await installExisting(page)
  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('Wait for cancellation')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(page.getByRole('button', { name: 'Stop' })).toBeVisible()
  await expect(page.locator('.message.user')).toHaveCount(0)
  await page.getByRole('button', { name: 'Stop' }).click()
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
  await expect(page.locator('.message.user')).toHaveCount(0)
  expect(server.snapshotCount({ type: 'session_content', id: session.id })).toBe(1)
})

test('resyncs a recovered run from durable session history', async ({ page }) => {
  const recoveredItems = items('Before refresh', 'Recovered answer')
  const partialItems = [messageItem(1, 'user', 'Before refresh')]
  const server = await installSyncMock(page, {
    projects: [project], sessions: [session], contents: {
      [session.id]: { items: partialItems, activeRun: { run_id: 'run-recovered', session_id: session.id, turn_id: 'turn-main', started_at: '2026-01-01T00:00:01Z', status: 'running', recoverable: true, run_epoch: 'epoch', run_cursor: '0', replay_available: false, recovery_required: true } },
    },
  })
  await page.goto('/#token=e2e')
  await expect(page.getByText('Before refresh', { exact: true })).toBeVisible()
  expect(server.snapshotCount({ type: 'session_content', id: session.id })).toBe(1)
  // A cursor-2 frame against the recovery-required cursor-0 snapshot is an
  // actual transient sequence gap. The following snapshot is the only place
  // where the complete durable history becomes authoritative.
  server.sendTypedEvent(session.id, 'run-recovered', { type: 'text.delta', run_cursor: '2', turn_id: 'turn-main', agent_iteration: 1, item_id: 'assistant-recovered', delta: 'ignored gap' })
  server.recoverRunFromSnapshot(session.id, 'run-recovered', recoveredItems)
  await expect.poll(() => server.snapshotCount({ type: 'session_content', id: session.id })).toBe(2)
  await expect(page.getByText('Recovered answer', { exact: true })).toBeVisible()
  server.settleRun(session.id, 'run-recovered', 'committed', recoveredItems)
  await expect(page.getByLabel('Session status: idle')).toBeVisible()
})

test('renames sessions and projects and confirms project-wide deletion', async ({ page }) => {
  page.on('dialog', (dialog) => void dialog.accept('Renamed project'))
  await installExisting(page, { sessions: [session] })
  await page.goto('/#token=e2e')
  await page.getByRole('button', { name: `Rename ${project.display_name}` }).click()
  await expect(page.getByText('Renamed project', { exact: true })).toBeVisible()
  page.removeAllListeners('dialog')
  page.on('dialog', (dialog) => void dialog.accept('Renamed session'))
  await page.getByRole('button', { name: `Rename ${session.display_name}` }).click()
  await expect(page.getByRole('heading', { name: 'Renamed session' })).toBeVisible()
  page.removeAllListeners('dialog')
  page.on('dialog', (dialog) => void dialog.accept())
  await page.getByRole('button', { name: 'Delete Renamed project', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Connect your first project' })).toBeVisible()
})

test('shows archived sessions and restores them to the active list', async ({ page }) => {
  page.on('dialog', (dialog) => void dialog.accept())
  const server = await installExisting(page)
  await page.goto('/#token=e2e')
  await page.getByRole('button', { name: `Archive ${session.display_name}` }).click()
  await expect(page.getByRole('heading', { name: 'No sessions yet' })).toBeVisible()
  await page.getByRole('button', { name: 'Archived (1)' }).click()
  await expect(page.getByText(session.display_name)).toBeVisible()
  await page.getByRole('button', { name: `Restore ${session.display_name}` }).click()
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Archived (1)' })).toHaveCount(0)
  expect(server.snapshotCount({ type: 'session_index', id: project.id })).toBeGreaterThanOrEqual(1)
})
