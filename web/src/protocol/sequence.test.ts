import { describe, expect, it } from 'vitest'
import {
  compareDecimal,
  compareRunCursor,
  compareSequence,
  isResourceRevision,
  parseDecimal,
  parseRunCursor,
  parseSequence,
} from './sequence'

describe('protocol decimal helpers', () => {
  it('uses BigInt without losing large wire values', () => {
    const value = '18446744073709551616'
    expect(parseDecimal(value)).toBe(18446744073709551616n)
    expect(parseSequence(value)).toBe(18446744073709551616n)
    expect(parseRunCursor(value)).toBe(18446744073709551616n)
  })

  it('compares each protocol counter as its own decimal concept', () => {
    expect(compareDecimal('0007', '7')).toBe(0)
    expect(compareSequence('6', '7')).toBe(-1)
    expect(compareRunCursor('7', '7')).toBe(0)
  })

  it('treats resource revisions as opaque strings', () => {
    expect(isResourceRevision('revision-718')).toBe(true)
    expect(isResourceRevision('')).toBe(false)
  })

  it('rejects non-decimal values', () => {
    expect(() => parseDecimal('-1')).toThrow()
    expect(() => parseDecimal('1.0')).toThrow()
    expect(() => compareDecimal('1', 'nope')).toThrow()
  })
})
