import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('CodexLoginCommands domain boundary', () => {
  it('does not depend on wire, sync, transport, blobs, or the legacy API', () => {
    const source = readFileSync(new URL('./codexLoginCommands.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport|blob|api)/)
    expect(source).not.toMatch(/\b(?:Blob|WebSocket|ResourceKey|CommandMessage|fetch)\b/)
  })
})
