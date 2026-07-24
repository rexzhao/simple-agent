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
    let body: unknown
    if (url.pathname === '/api/bootstrap') body = { version: 'e2e', cwd: '/fixture', server_root: '/fixture', config_path: '/fixture/config' }
    else if (url.pathname === '/api/projects') body = { projects: [project] }
    else if (url.pathname === `/api/projects/${project.id}/sessions`) body = { sessions: [session] }
    else if (url.pathname === '/api/runs/active') body = { runs: [] }
    else if (url.pathname === `/api/sessions/${session.id}`) body = session
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

test('automatically loads older history and preserves the viewport anchor', async ({ page }) => {
  let release!: () => void
  const gate = new Promise<void>((resolve) => { release = resolve })
  const olderCalls = await mockApp(page, gate)
  await page.goto('/')
  const messages = page.locator('.messages')
  const anchor = page.getByText('message-101', { exact: true })
  await expect(anchor).toBeAttached()

  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect.poll(olderCalls).toBe(1)
  const before = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  release()
  await expect(page.getByText('message-51', { exact: true })).toBeAttached()
  await twoFrames(page)
  const after = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  expect(Math.abs(after - before)).toBeLessThanOrEqual(2)
  expect(olderCalls()).toBe(1)
})

test('content-visibility does not cause a second jump when older messages become visible', async ({ page }) => {
  await mockApp(page, Promise.resolve())
  await page.goto('/')
  const messages = page.locator('.messages')
  const anchor = page.getByText('message-101', { exact: true })
  await expect(anchor).toBeAttached()
  await expect(anchor.locator('xpath=ancestor::article[1]')).toHaveCSS('content-visibility', 'auto')
  await messages.evaluate((element) => { element.scrollTop = 0 })
  await expect(page.getByText('message-51', { exact: true })).toBeAttached()
  await twoFrames(page)
  await anchor.scrollIntoViewIfNeeded()
  const before = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  await twoFrames(page)
  const after = await anchor.evaluate((element) => element.getBoundingClientRect().top)
  expect(Math.abs(after - before)).toBeLessThanOrEqual(2)
})
