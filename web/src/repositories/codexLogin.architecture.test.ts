import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('CodexLoginRepository domain boundary', () => {
  it('has no sync/protocol/transport/blob dependency', () => {
    const source = readFileSync(new URL('./codexLogin.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport|blob)/)
    expect(source).not.toMatch(/\b(?:Blob|WebSocket|ResourceKey|Sequence|Subscription)\b/)
  })
})
