const maxWireIdentifierBytes = 4096

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

// Go's unicode.IsSpace is the Unicode White_Space property plus the
// additional Latin-1 whitespace characters below. Keep this explicit rather
// than using JavaScript's broader \s/trim set (which also treats U+FEFF as
// whitespace while Go's validator does not).
function isGoSpace(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0) as number
    if (
      code === 0x0009 || code === 0x000a || code === 0x000b || code === 0x000c || code === 0x000d ||
      code === 0x0020 || code === 0x0085 || code === 0x00a0 || code === 0x1680 ||
      (code >= 0x2000 && code <= 0x200a) || code === 0x2028 || code === 0x2029 ||
      code === 0x202f || code === 0x205f || code === 0x3000
    ) return true
  }
  return false
}

/**
 * The shared SessionContent wire identifier boundary. This mirrors
 * sessioncontent.validateID: canonical UTF-8, no whitespace/control, and the
 * protocol MaxWireIdentifierBytes limit. It intentionally does not impose a
 * path-safe alphabet; durable item IDs are opaque wire identifiers.
 */
export function isCanonicalWireIdentifier(value: unknown, allowEmpty = false): value is string {
  if (typeof value !== 'string' || !wellFormed(value)) return false
  if (allowEmpty && value === '') return true
  if (value === '' || isGoSpace(value) || /\p{Cc}/u.test(value)) return false
  return new TextEncoder().encode(value).byteLength <= maxWireIdentifierBytes
}

export function isWellFormedString(value: string): boolean {
  return wellFormed(value)
}

export { maxWireIdentifierBytes }
