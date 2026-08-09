import { expect, test } from '@playwright/test'
import { installSyncMock, type WireProject, type WireSession } from './ws-fixture'

const project: WireProject = {
  id: 'project-fixture',
  root: '/fixture',
  display_name: 'Fixture',
  archived: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

const session: WireSession = {
  id: 'session-fixture',
  project_id: project.id,
  display_name: 'Fixture session',
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

type ManualSocketState = {
  socket: WebSocket
  messages: Array<Record<string, any>>
}

test('the WS fixture enforces subscription ownership, unsubscribe, and socket cleanup', async ({ page }) => {
  const server = await installSyncMock(page, { projects: [project], sessions: [session] })
  await page.goto('/#token=e2e')
  await expect(page.getByRole('heading', { name: session.display_name })).toBeVisible()

  const resource = { type: 'session_content', id: session.id } as const
  const baseline = server.activeSubscriptionCount(resource)
  expect(baseline).toBe(1)

  await page.evaluate(() => {
    type SocketRecord = { socket: WebSocket; messages: Array<Record<string, any>> }
    const sockets: Record<string, SocketRecord> = {}
    const open = (name: string, subscriptionID: string) => {
      const url = new URL('/api/ws', window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      const socket = new WebSocket(url)
      const record = { socket, messages: [] as Array<Record<string, any>> }
      sockets[name] = record
      socket.onmessage = (event) => {
        const message = JSON.parse(String(event.data)) as Record<string, any>
        record.messages.push(message)
        if (message.type === 'welcome') {
          socket.send(JSON.stringify({
            version: 1,
            type: 'subscribe',
            id: `${name}-subscribe-request`,
            payload: {
              subscription_id: subscriptionID,
              resource: { type: 'session_content', id: 'session-fixture' },
            },
          }))
        }
      }
      socket.onopen = () => socket.send(JSON.stringify({
        version: 1,
        type: 'hello',
        id: `${name}-hello`,
        payload: { supported_versions: [1], client_id: `fixture-${name}` },
      }))
    }
    open('ownerA', 'manual-a')
    open('ownerB', 'manual-b')
    ;(window as Window & { __manualSockets?: Record<string, SocketRecord> }).__manualSockets = sockets
  })

  await expect.poll(() => server.activeSubscriptionCount(resource)).toBe(baseline + 2)

  // A socket cannot unsubscribe another socket's subscription.
  await page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    sockets.ownerA.socket.send(JSON.stringify({
      version: 1,
      type: 'unsubscribe',
      id: 'wrong-owner-unsubscribe',
      payload: { subscription_id: 'manual-b' },
    }))
  })
  await expect.poll(() => server.isSubscriptionActive('manual-b')).toBe(true)

  // Only the owning socket may release A. A subsequent publication must not
  // be delivered to either the released subscription or a stale owner.
  await page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    sockets.ownerA.socket.send(JSON.stringify({
      version: 1,
      type: 'unsubscribe',
      id: 'owner-a-unsubscribe',
      payload: { subscription_id: 'manual-a' },
    }))
  })
  await expect.poll(() => server.isSubscriptionActive('manual-a')).toBe(false)
  const ownerABeforePublication = await page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    return sockets.ownerA.messages.length
  })
  server.sendTypedEvent(session.id, 'fixture-run', {
    type: 'run.started',
    turn_id: 'fixture-turn',
    status: 'running',
  })
  await expect.poll(() => page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    return sockets.ownerB.messages.filter((message) => message.type === 'subscription_event').length
  })).toBeGreaterThan(0)
  const ownerAAfterPublication = await page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    return sockets.ownerA.messages.length
  })
  expect(ownerAAfterPublication).toBe(ownerABeforePublication)

  // Closing B removes its whole socket-owned subscription set. The app's
  // original subscription remains the only active owner for this resource.
  await page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    sockets.ownerB.socket.close()
  })
  await expect.poll(() => server.activeSubscriptionCount(resource)).toBe(baseline)
  await page.evaluate(() => {
    const sockets = (window as Window & { __manualSockets: Record<string, ManualSocketState> }).__manualSockets
    sockets.ownerA.socket.close()
  })
})

test('the WS fixture can delay snapshots and resume a subscription after reconnect', async ({ page }) => {
  const server = await installSyncMock(page, { projects: [project], sessions: [session] })
  await page.goto('/#token=e2e')

  const resource = { type: 'fixture_resource', id: 'resume-test' } as const
  const subscriptionID = 'resume-subscription'
  const openSocket = async () => {
    await page.evaluate(({ resource, subscriptionID }) => {
      type SocketRecord = { socket: WebSocket; messages: Array<Record<string, any>> }
      const url = new URL('/api/ws', window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      const record = { socket: new WebSocket(url), messages: [] as Array<Record<string, any>> }
      record.socket.onopen = () => record.socket.send(JSON.stringify({
        version: 1,
        type: 'hello',
        id: 'resume-hello',
        payload: { supported_versions: [1], client_id: 'resume-fixture' },
      }))
      record.socket.onmessage = (event) => {
        const message = JSON.parse(String(event.data)) as Record<string, any>
        record.messages.push(message)
        if (message.type === 'welcome') {
          record.socket.send(JSON.stringify({
            version: 1,
            type: 'subscribe',
            id: 'resume-subscribe-request',
            payload: { subscription_id: subscriptionID, resource },
          }))
        }
      }
      ;(window as Window & { __resumeSocket?: SocketRecord }).__resumeSocket = record
    }, { resource, subscriptionID })
  }

  server.delaySnapshots(resource)
  await openSocket()
  await expect.poll(() => server.activeSubscriptionCount(resource)).toBe(1)
  expect(server.snapshotCount(resource)).toBe(0)
  server.releaseSnapshots(resource)
  await expect.poll(() => page.evaluate(() => Boolean((window as Window & { __resumeSocket?: ManualSocketState }).__resumeSocket?.messages.some((message) => message.type === 'snapshot')))).toBe(true)
  expect(server.snapshotCount(resource)).toBe(1)
  const resumeSequence = await page.evaluate(() => {
    const messages = (window as Window & { __resumeSocket?: ManualSocketState }).__resumeSocket?.messages ?? []
    const snapshot = messages.find((message) => message.type === 'snapshot')
    return String(snapshot?.sequence ?? '0')
  })

  await page.evaluate(() => (window as Window & { __resumeSocket?: ManualSocketState }).__resumeSocket?.socket.close())
  await expect.poll(() => server.activeSubscriptionCount(resource)).toBe(0)

  server.holdChanges(resource)
  // Publish directly through the resource-neutral fixture controls so the
  // reconnect has real journal entries to resume, not a fresh initial state.
  server.publishResourceOperations(resource, [{ op: 'upsert', key: 'first', value: { value: 1 } }])
  server.publishResourceOperations(resource, [{ op: 'upsert', key: 'second', value: { value: 2 } }])
  server.releaseChanges(resource, 'ordered')

  await page.evaluate(({ resource, subscriptionID, resumeSequence }) => {
    const url = new URL('/api/ws', window.location.href)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    const record = { socket: new WebSocket(url), messages: [] as Array<Record<string, any>> }
    record.socket.onopen = () => record.socket.send(JSON.stringify({
      version: 1,
      type: 'hello',
      id: 'resume-again-hello',
      payload: { supported_versions: [1], client_id: 'resume-fixture' },
    }))
    record.socket.onmessage = (event) => {
      const message = JSON.parse(String(event.data)) as Record<string, any>
      record.messages.push(message)
      if (message.type === 'welcome') {
        record.socket.send(JSON.stringify({
          version: 1,
          type: 'subscribe',
          id: 'resume-again-request',
          payload: { subscription_id: subscriptionID, resource, resume: { stream_epoch: 'e2e-epoch', sequence: resumeSequence } },
        }))
      }
    }
    ;(window as Window & { __resumeSocket?: ManualSocketState }).__resumeSocket = record
  }, { resource, subscriptionID, resumeSequence })
  await expect.poll(() => server.activeSubscriptionCount(resource)).toBe(1)
  await expect.poll(() => page.evaluate(() => (window as Window & { __resumeSocket?: ManualSocketState }).__resumeSocket?.messages.filter((message) => message.type === 'change').length ?? 0)).toBe(2)
  expect(server.snapshotCount(resource)).toBe(1)
})
