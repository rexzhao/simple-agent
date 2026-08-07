import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('application sync page boundary', () => {
  it('keeps Context, hooks, and the pure contract below infrastructure', () => {
    const pageBoundaryFiles = [
      new URL('./applicationContext.tsx', import.meta.url),
      new URL('./hooks/useSyncApplication.ts', import.meta.url),
      new URL('./applicationServices.ts', import.meta.url),
    ]
    for (const url of pageBoundaryFiles) {
      const source = readFileSync(url, 'utf8')
      expect(source).not.toMatch(/from ['"][^'"]*(?:sync|protocol|transport|blob|api|bootstrap)/i)
      expect(source).not.toMatch(/\b(?:ProtocolMessage|WebSocketTransport|SyncRuntime|BlobClient|bootstrap)\b/i)
    }
    const contextSource = readFileSync(new URL('./applicationContext.tsx', import.meta.url), 'utf8')
    expect(contextSource).not.toMatch(/\b(?:runtime|stores|replica|transport|BlobClient)\b/i)
  })
})
