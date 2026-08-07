const maxProviderNameBytes = 256
const maxProviderCreateFilenameBytes = 255

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

/** Mirrors execution.validateProviderSettingsCreateFilename for early UX rejection. */
export function isProviderCreateName(value: unknown): value is string {
  if (!isProviderName(value) || utf8ByteLength(value) + utf8ByteLength('.yaml') > maxProviderCreateFilenameBytes) return false
  const characters = [...value]
  if (value.endsWith('.') || isGoSpace(characters[characters.length - 1])) return false
  if (/[<>:"|?*]/u.test(value)) return false
  let deviceName = value
  const dot = deviceName.indexOf('.')
  if (dot >= 0) deviceName = deviceName.slice(0, dot)
  const upper = deviceName.toUpperCase()
  if (new Set(['CON', 'PRN', 'AUX', 'NUL', 'CLOCK$', 'COM1', 'COM2', 'COM3', 'COM4', 'COM5', 'COM6', 'COM7', 'COM8', 'COM9', 'LPT1', 'LPT2', 'LPT3', 'LPT4', 'LPT5', 'LPT6', 'LPT7', 'LPT8', 'LPT9']).has(upper)) return false
  return true
}
