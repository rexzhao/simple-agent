import type { ResourceRevision, RunCursor, Sequence } from './types'

const decimalPattern = /^[0-9]+$/

export function isDecimalString(value: unknown): value is string {
  return typeof value === 'string' && decimalPattern.test(value)
}

export function parseDecimal(value: string): bigint {
  if (!isDecimalString(value)) {
    throw new Error('expected a non-negative decimal string')
  }
  return BigInt(value)
}

export function compareDecimal(left: string, right: string): -1 | 0 | 1 {
  const leftValue = parseDecimal(left)
  const rightValue = parseDecimal(right)
  if (leftValue < rightValue) return -1
  if (leftValue > rightValue) return 1
  return 0
}

export function isSequence(value: unknown): value is Sequence {
  return isDecimalString(value)
}

export function isResourceRevision(value: unknown): value is ResourceRevision {
  return typeof value === 'string' && value.trim() !== ''
}

export function isRunCursor(value: unknown): value is RunCursor {
  return typeof value === 'string' && /^(0|[1-9][0-9]*)$/.test(value)
}

export function parseSequence(value: string): bigint {
  return parseDecimal(value)
}

export function parseRunCursor(value: string): bigint {
  if (!isRunCursor(value)) {
    throw new Error('expected a canonical non-negative run cursor')
  }
  return BigInt(value)
}

export function compareSequence(left: string, right: string): -1 | 0 | 1 {
  return compareDecimal(left, right)
}

export function compareRunCursor(left: string, right: string): -1 | 0 | 1 {
  const leftValue = parseRunCursor(left)
  const rightValue = parseRunCursor(right)
  if (leftValue < rightValue) return -1
  if (leftValue > rightValue) return 1
  return 0
}
