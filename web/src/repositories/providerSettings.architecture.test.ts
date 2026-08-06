import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('domain-facing Provider Settings repository', () => {
  it('does not depend on sync/protocol/transport and exposes only domain metadata', () => {
    const source = readFileSync(new URL('./providerSettings.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport|blob|lifecycle)/)
    expect(source).not.toMatch(/\b(?:ResourceKey|Sequence|BlobDescriptor|WebSocketTransport|SyncRuntime|SyncReadError)\b/)
    expect(source).not.toMatch(/\b(?:subscription_id|stream_epoch|resource_revision|raw wire)\b/)
  })
})
