import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { SessionIndexRepository as SyncSessionIndexRepository } from '../sync/sessionIndexRepository'
import { SessionIndexAdapter, type SessionSummary } from '../sync/sessionIndexAdapter'
import { LocalReplica } from '../sync/localReplica'
import type { JsonObject } from '../protocol/types'
import {
  SessionIndexRepository,
  selectSessionReadModel,
} from './sessionIndex'

const summary = (id: string, overrides: Partial<SessionSummary> = {}): SessionSummary => ({
  session_id: id,
  project_id: 'project_a',
  parent_session_id: null,
  display_name: id,
  archived: false,
  status: 'idle',
  run_id: null,
  resource_revision: '1',
  updated_at: '2025-01-01T00:00:00Z',
  has_unread_result: false,
  ...overrides,
})

function wire(value: SessionSummary): JsonObject {
  return { ...value } as unknown as JsonObject
}

describe('domain-facing Session Index repository', () => {
  it('keeps A session getSnapshot identity stable while B changes in the same project', () => {
    const replica = new LocalReplica()
    const syncRepository = new SyncSessionIndexRepository(replica)
    const domainRepository = new SessionIndexRepository(syncRepository)
    const resource = { type: 'session_index' as const, id: 'project_a' }
    const adapter = new SessionIndexAdapter('project_a')
    replica.applySnapshot(resource, adapter, { sessions: [wire(summary('a')), wire(summary('b'))] }, {
      streamEpoch: 'epoch_1', sequence: '1' as never, resourceRevision: '1', generation: 1,
    })

    const aBefore = selectSessionReadModel(domainRepository, 'project_a', 'a')
    const bBefore = selectSessionReadModel(domainRepository, 'project_a', 'b')
    const listBefore = domainRepository.getProjectReadModel('project_a')
    let aNotifications = 0
    const unsubscribe = domainRepository.subscribeSession('project_a', 'a', () => { aNotifications += 1 })

    replica.applyChange(resource, adapter, [{
      op: 'upsert', key: 'b', value: wire(summary('b', {
        status: 'completed', run_id: 'run_b', has_unread_result: true, resource_revision: '2',
      })),
    }], { streamEpoch: 'epoch_1', sequence: '2' as never, resourceRevision: '2', generation: 1 })

    const aAfter = selectSessionReadModel(domainRepository, 'project_a', 'a')
    const bAfter = selectSessionReadModel(domainRepository, 'project_a', 'b')
    const listAfter = domainRepository.getProjectReadModel('project_a')
    expect(aAfter).toBe(aBefore)
    expect(aAfter.summary).toBe(aBefore.summary)
    expect(bAfter).not.toBe(bBefore)
    expect(bAfter.summary).toMatchObject({ status: 'completed', has_unread_result: true })
    expect(listAfter).not.toBe(listBefore)
    expect(aNotifications).toBe(1)
    unsubscribe()
  })

  it('keeps the public entrypoint free of transport/protocol/sync details', () => {
    const entryFiles = [
      new URL('./sessionIndex.ts', import.meta.url),
      new URL('./sessionContent.ts', import.meta.url),
      new URL('../hooks/useSessionIndex.ts', import.meta.url),
      new URL('../hooks/useSessionContent.ts', import.meta.url),
      new URL('../commands/sessionCommands.ts', import.meta.url),
    ]
    for (const url of entryFiles) {
      const source = readFileSync(url, 'utf8')
      expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport|blob)/)
      expect(source).not.toMatch(/\b(?:ResourceKey|Sequence|BlobDescriptor|WebSocketTransport|SyncRuntime)\b/)
    }
    for (const url of [new URL('./sessionContent.ts', import.meta.url), new URL('../hooks/useSessionContent.ts', import.meta.url)]) {
      const source = readFileSync(url, 'utf8')
      expect(source).not.toMatch(/\b(?:subscription_id|stream_epoch|sequence|generation|resource_revision)\b/)
    }
  })

  it('does not allow page files to import internal sync/protocol/transport modules', () => {
    const pageFiles = [
      '../App.tsx',
      '../components/Composer.tsx', '../components/Conversation.tsx', '../components/DebugSettingsDialog.tsx',
      '../components/ProcessTimeline.tsx', '../components/ProviderManagerDialog.tsx', '../components/SessionModelDialog.tsx',
      '../components/SessionSubPanel.tsx', '../components/VirtualConversationList.tsx', '../components/WorkspaceTree.tsx',
      '../components/icons.tsx', '../components/misc.tsx',
      '../hooks/useComposerDrafts.ts', '../hooks/useRunRegistry.ts', '../hooks/useSessionHistory.ts',
      '../hooks/useSessionSelection.ts', '../hooks/useSessionStore.ts', '../hooks/useSessionIndex.ts',
    ]
    for (const file of pageFiles) {
      const source = readFileSync(new URL(file, import.meta.url), 'utf8')
      expect(source, file).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport)/)
    }
  })
})
