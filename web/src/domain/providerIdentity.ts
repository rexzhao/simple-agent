const maxProviderNameBytes = 256

function wellFormed(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false
      index += 1
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return false
    }
  }
  return true
}

function isGoSpace(value: string | undefined): boolean {
  if (!value) return false
  const code = value.codePointAt(0) as number
  return code === 0x0009 || code === 0x000a || code === 0x000b || code === 0x000c || code === 0x000d ||
    code === 0x0020 || code === 0x0085 || code === 0x00a0 || code === 0x1680 ||
    (code >= 0x2000 && code <= 0x200a) || code === 0x2028 || code === 0x2029 ||
    code === 0x202f || code === 0x205f || code === 0x3000
}

export function isWellFormedString(value: string): boolean {
  return wellFormed(value)
}

export function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).byteLength
}

/** Shared with sync command validation; mirrors config.ValidateProviderName. */
export function isProviderName(value: unknown): value is string {
  if (typeof value !== 'string' || !wellFormed(value) || value.length === 0 || utf8ByteLength(value) > maxProviderNameBytes || value === '.' || value === '..') return false
  const characters = [...value]
  if (isGoSpace(characters[0]) || isGoSpace(characters[characters.length - 1])) return false
  if (value.includes('/') || value.includes('\\') || /[\p{Cc}]/u.test(value)) return false
  return true
}
