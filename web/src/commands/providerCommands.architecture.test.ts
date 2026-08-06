import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('ProviderCommands domain boundary', () => {
  it('contains only page/domain types and no transport or wire concepts', () => {
    const source = readFileSync(new URL('./providerCommands.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport|blob|lifecycle)/)
    expect(source).not.toMatch(/\b(?:BlobDescriptor|BlobClient|WebSocket|ResourceKey|CommandMessage|ProtocolMessage|fetch)\b/)
  })
})
