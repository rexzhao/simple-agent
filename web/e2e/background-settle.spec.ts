import { expect, test } from '@playwright/test'
import { installSyncMock, messageItem, type WireProject, type WireSession } from './ws-fixture'

const project: WireProject = { id: 'project-main', root: '/workspace/project', display_name: 'Main project', archived: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }
const sessionMain: WireSession = { id: 'session-main', project_id: project.id, display_name: 'Main session', status: 'idle', archived: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-02T00:00:00Z', last_used_at: '2026-01-02T00:00:00Z', last_seq: 0 }
const sessionBg: WireSession = { id: 'session-bg', project_id: project.id, display_name: 'Background session', status: 'running', archived: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', last_used_at: '2026-01-01T00:00:00Z', last_seq: 2 }
const bgItems = [messageItem(1, 'user', 'Background task', { turn_id: 'turn-bg' }), messageItem(2, 'assistant', 'Background answer', { turn_id: 'turn-bg' })]

test('a run settling on a background session reconciles without a manual refresh', async ({ page }) => {
  const server = await installSyncMock(page, {
    projects: [project], sessions: [sessionMain],
    contents: {
      [sessionBg.id]: { items: bgItems, activeRun: { run_id: 'run-bg', session_id: sessionBg.id, turn_id: 'turn-bg', started_at: '2026-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch', run_cursor: '0', replay_available: false, recovery_required: false } },
    },
  })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: sessionMain.display_name })).toBeVisible()
  // Add the background authority only after the main conversation is open so
  // initial navigation cannot select it. It remains unopened for settlement.
  server.addSession(sessionBg, {
    items: bgItems,
    activeRun: { run_id: 'run-bg', session_id: sessionBg.id, turn_id: 'turn-bg', started_at: '2026-01-01T00:00:00Z', status: 'running', recoverable: true, run_epoch: 'epoch', run_cursor: '0', replay_available: false, recovery_required: false },
  })
  const selectedIdleIcon = page.locator('.session-tree-row.selected .session-icon.idle')
  await expect(selectedIdleIcon).toHaveCount(1)
  await expect(selectedIdleIcon).toHaveCSS('background-color', 'rgb(254, 243, 199)')
  const backgroundIcon = page.locator('.session-tree-button').filter({ hasText: sessionBg.display_name }).locator('.session-icon.running')
  await expect(backgroundIcon).toHaveCount(1)
  await expect(backgroundIcon).toHaveCSS('animation-name', 'session-icon-pulse')
  server.settleRun(sessionBg.id, 'run-bg', 'committed', bgItems)

  // The background session stays unopened. Its Session Index publication must
  // still clear the global running state while the selected content remains
  // on the main session.
  await expect(page.getByRole('heading', { name: sessionMain.display_name })).toBeVisible()
  await expect(page.getByText('Refresh needed')).toHaveCount(0)
  await expect(page.locator('.session-tree-button').filter({ hasText: sessionBg.display_name }).locator('.session-icon.running')).toHaveCount(0)
  await expect(page.locator('.session-tree-button').filter({ hasText: sessionBg.display_name }).locator('.status-dot')).toHaveCount(0)

  // Opening after settlement must reconcile the snapshot/history, rather
  // than depending on a live run event that was never observed while closed.
  await page.getByText(sessionBg.display_name, { exact: true }).click()
  await expect(page.getByRole('heading', { name: sessionBg.display_name })).toBeVisible()
  await expect(page.locator('.message.assistant.transient')).toHaveCount(0)
  await expect(page.locator('.message.assistant:not(.transient)')).toContainText('Background answer')
  await expect(page.locator('.message.user:not(.transient)')).toContainText('Background task')
  expect(server.snapshotCount({ type: 'session_content', id: sessionBg.id })).toBe(1)
})
