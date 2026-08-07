import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('Provider manager application boundary', () => {
  it('does not own REST, sync, protocol, Blob, or secret-bearing document state', () => {
    const source = readFileSync(new URL('./ProviderManagerDialog.tsx', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"]\.\.\/api['"]|\bfetch\s*\(/)
    expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport)/)
    expect(source).not.toMatch(/\b(?:WebSocket|LocalReplica|BlobDescriptor|subscription_id|stream_epoch|resource_revision|api\.provider|api\.codexLogin|codexLoginStatus)\b/)
    expect(source).not.toMatch(/\b(?:setInterval|setTimeout|clearInterval|clearTimeout)\s*\(/)
    expect(source).not.toMatch(/ProviderSettingsDocument|CodexAuthStatus/)
    const updateModel = source.slice(source.indexOf('const updateModel'), source.indexOf('const duplicateModel'))
    expect(updateModel).not.toContain('onProviderChange')
    expect(source).toContain('savedCodexProvider')
  })
})
