// JavaScript string length counts UTF-16 code units. Wire contracts that use
// Go's rune count must count Unicode code points instead, so astral characters
// have the same length on both sides of the protocol boundary.
export function unicodeCodePointLength(value: string): number {
  return Array.from(value).length
}
