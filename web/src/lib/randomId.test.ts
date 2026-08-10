import { afterEach, describe, expect, it, vi } from 'vitest'
import { randomID } from './randomId'

const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('randomID', () => {
  it('returns RFC 4122 version 4 UUIDs', () => {
    const ids = new Set(Array.from({ length: 32 }, () => randomID()))
    for (const id of ids) expect(id).toMatch(UUID_PATTERN)
    expect(ids.size).toBe(32)
  })

  it('falls back to getRandomValues when randomUUID is unavailable (insecure context)', () => {
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) })
    const id = randomID()
    expect(id).toMatch(UUID_PATTERN)
  })

  it('falls back to a non-crypto source when Web Crypto is entirely unavailable', () => {
    vi.stubGlobal('crypto', undefined)
    const ids = new Set(Array.from({ length: 32 }, () => randomID()))
    for (const id of ids) expect(id).toMatch(UUID_PATTERN)
    expect(ids.size).toBe(32)
  })
})
