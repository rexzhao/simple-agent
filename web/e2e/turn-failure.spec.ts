import { expect, test } from '@playwright/test'
import { installSyncMock, messageItem, type WireProject, type WireSession } from './ws-fixture'

const project: WireProject = { id: 'project-1', root: '/fixture', display_name: 'Fixture', archived: false, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }
const session: WireSession = {
  id: 'session-1',
  project_id: project.id,
  display_name: 'Failure fixture',
  provider: 'fake',
  model_profile: 'fast',
  model_id: 'fake-model',
  status: 'idle',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  last_used_at: '2026-01-01T00:00:00Z',
  created_cwd: '/fixture',
  last_seq: 0,
  archived: false,
}

// turn.failed is a bounded typed diagnostic projected from execution. The
// fixture exercises the same code/message path as the production adapter.
test('shows a failed durable turn and continues without resending the user message', async ({ page }) => {
  const server = await installSyncMock(page, {
    projects: [project],
    sessions: [session],
    onCommand: (mock, command) => {
      if (command.name !== 'run.start') return
      void (async () => {
        await new Promise<void>((resolve) => setTimeout(resolve, 80))
        const sessionID = String(command.arguments.session_id)
        const runID = String(command.arguments.run_id)
        mock.settleRun(
          sessionID,
          runID,
          'failed',
          [messageItem(1, 'user', 'fix the bug', { id: 'u1', turn_id: 'turn-1' })],
          { interrupted_run_id: runID, interrupted_turn_id: 'turn-1' },
          {
            code: 'model_http_error',
            message: '429: slow down and try again',
          },
        )
      })()
    },
  })

  await page.goto('/#token=e2e')
  await page.getByPlaceholder('Send a message to SAI').fill('fix the bug')
  await page.getByRole('button', { name: 'Send' }).click()

  // Admission itself does not create a local user bubble; the typed durable
  // item is the only source of the rendered message.
  await page.waitForTimeout(10)
  expect(await page.locator('.message.user.transient').count()).toBe(0)
  const continueButton = page.getByRole('button', { name: 'Continue' })
  const turnError = page.locator('.turn-error')
  await expect(continueButton).toBeVisible()
  await expect(page.locator('.message.user .message-text').last()).toHaveText('fix the bug')
  await expect(turnError).toContainText('model_http_error')
  await expect(turnError).toContainText('429: slow down and try again')
  await expect(page.locator('.error-banner')).toHaveCount(0)

  // Settlement must not erase the safe failure reason. Continue starts a new
  // typed run and therefore clears the old turn error without resending text.
  const durableUserCount = await page.locator('.message.user:not(.transient)').count()
  await continueButton.click()
  await expect.poll(() => server.commands.filter((command) => command.name === 'run.continue').length).toBe(1)
  const continueCommand = server.commands.find((command) => command.name === 'run.continue')
  expect(continueCommand?.arguments).not.toHaveProperty('content')
  await expect(turnError).toHaveCount(0)
  await expect(page.locator('.message.user:not(.transient)')).toHaveCount(durableUserCount)
})

test('does not offer Continue when the session ends with an assistant message', async ({ page }) => {
  await installSyncMock(page, {
    projects: [project],
    sessions: [session],
    contents: {
      [session.id]: {
        items: [messageItem(1, 'user', 'question'), messageItem(2, 'assistant', 'answer')],
        hasMoreBefore: false,
      },
    },
  })
  await page.goto('/#token=e2e')
  await expect(page.locator('.message.assistant .message-text').last()).toHaveText('answer')
  await expect(page.getByRole('button', { name: 'Continue' })).toHaveCount(0)
  await expect(page.locator('.turn-error')).toHaveCount(0)
})
