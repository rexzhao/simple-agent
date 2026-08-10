import { describe, expect, it } from 'vitest'
import { sha256Hex } from './sha256'

async function reference(bytes: Uint8Array): Promise<string> {
  const digest = await globalThis.crypto.subtle.digest('SHA-256', bytes as unknown as ArrayBuffer)
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, '0')).join('')
}

describe('sha256Hex', () => {
  it('matches published SHA-256 test vectors', () => {
    expect(sha256Hex(new Uint8Array(0))).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
    expect(sha256Hex(new TextEncoder().encode('abc'))).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
    expect(sha256Hex(new TextEncoder().encode('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq')))
      .toBe('248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1')
    expect(sha256Hex(new TextEncoder().encode('{"sessions":[]}'))).toBe('a1179f88a4a67a27ff6c7922fd583ba4fc86888f4882350e58bb15d0158adb76')
  })

  it('matches the platform digest across padding boundaries and larger inputs', async () => {
    // Lengths around the 0x80/length-block boundary exercise every padding path.
    const lengths = [1, 55, 56, 57, 63, 64, 65, 119, 120, 127, 128, 129, 1024, 100_000]
    for (const length of lengths) {
      const bytes = new Uint8Array(length)
      for (let index = 0; index < length; index += 1) bytes[index] = (index * 31 + length) % 256
      expect(sha256Hex(bytes)).toBe(await reference(bytes))
    }
  })
})
