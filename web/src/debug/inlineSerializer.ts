import type { JsonObject, JsonValue } from '../domain/json'

export const DEBUG_INLINE_RESULT_BUDGET = 64 * 1024
export const DEBUG_SERIALIZER_MAX_DEPTH = 8
export const DEBUG_SERIALIZER_MAX_OBJECT_KEYS = 64
export const DEBUG_SERIALIZER_MAX_ARRAY_ELEMENTS = 128
export const DEBUG_SERIALIZER_MAX_STRING_BYTES = 8 * 1024
export const DEBUG_SERIALIZER_MAX_CONSOLE_ENTRIES = 128
export const DEBUG_SERIALIZER_MAX_CONSOLE_ARGUMENTS = 32
const DEBUG_SERIALIZER_STRUCTURE_RESERVE = 2 * 1024

export interface DebugBoundedText {
  readonly value: string
  readonly truncated: boolean
  readonly atLeastBytes?: number
}

type Reason = 'budget' | 'max_depth' | 'max_keys' | 'max_elements' | 'max_string' | 'circular' | 'unreadable'

interface CodePoint {
  readonly text: string
  readonly next: number
  readonly bytes: number
}

function codePointAt(value: string, index: number): CodePoint {
  const first = value.charCodeAt(index)
  if (first >= 0xd800 && first <= 0xdbff) {
    const second = value.charCodeAt(index + 1)
    if (second >= 0xdc00 && second <= 0xdfff) {
      return { text: value.slice(index, index + 2), next: index + 2, bytes: 4 }
    }
    return { text: '\ufffd', next: index + 1, bytes: 3 }
  }
  if (first >= 0xdc00 && first <= 0xdfff) return { text: '\ufffd', next: index + 1, bytes: 3 }
  if (first <= 0x7f) return { text: value.slice(index, index + 1), next: index + 1, bytes: 1 }
  if (first <= 0x7ff) return { text: value.slice(index, index + 1), next: index + 1, bytes: 2 }
  return { text: value.slice(index, index + 1), next: index + 1, bytes: 3 }
}

/**
 * Scans at most maxBytes plus one UTF-8 code point. It never asks TextEncoder
 * to encode the caller's complete string, and it replaces lone surrogates so
 * all returned text is well-formed for the wire.
 */
export function boundDebugString(value: string, maxBytes: number): DebugBoundedText {
  if (maxBytes <= 0) return { value: '', truncated: value.length > 0, ...(value.length > 0 ? { atLeastBytes: 1 } : {}) }
  let result = ''
  let bytes = 0
  let index = 0
  while (index < value.length) {
    const point = codePointAt(value, index)
    if (bytes + point.bytes > maxBytes) {
      return { value: result, truncated: true, atLeastBytes: maxBytes + 1 }
    }
    result += point.text
    bytes += point.bytes
    index = point.next
  }
  return { value: result, truncated: false }
}

function marker(reason: Reason, extra: JsonObject = {}): JsonObject {
  const safeExtra: JsonObject = {}
  for (const key of Object.keys(extra)) {
    const value = extra[key]
    safeExtra[key] = typeof value === 'string' ? boundDebugString(value, 256).value : value
  }
  return { __sai_debug: 'summary', reason, ...safeExtra }
}

function byteLength(value: string): number {
  // All callers pass serializer-produced, already bounded JSON. This is not
  // used to inspect an arbitrary page string.
  return new TextEncoder().encode(value).byteLength
}

/**
 * Converts arbitrary page values to a finite JSON representation. It only
 * reads enumerable data properties (accessors are summarized without calling
 * them) and never calls toJSON, getters, or user conversion methods.
 */
export class DebugInlineSerializer {
  private usedBytes = 0
  private readonly seen = new WeakSet<object>()
  private readonly contentBudget: number

  constructor(budgetBytes = DEBUG_INLINE_RESULT_BUDGET, private readonly windowRef?: Window) {
    this.contentBudget = Math.max(0, budgetBytes - DEBUG_SERIALIZER_STRUCTURE_RESERVE)
  }

  serialize(value: unknown): JsonValue {
    return this.value(value, 0)
  }

  get used(): number { return this.usedBytes }

  private value(value: unknown, depth: number): JsonValue {
    if (value === undefined) return this.summary('unreadable', { type: 'undefined' })
    if (value === null || typeof value === 'boolean' || typeof value === 'string') {
      if (typeof value === 'string') return this.string(value)
      return this.withBudget(value)
    }
    if (typeof value === 'number') {
      if (!Number.isFinite(value)) return this.summary('unreadable', { type: 'number', value: String(value) })
      return this.withBudget(value)
    }
    if (typeof value === 'bigint') return this.summary('unreadable', { type: 'bigint', truncated: true })
    if (typeof value === 'function') return this.summary('unreadable', { type: 'function' })
    if (typeof value === 'symbol') {
      let description: string | undefined
      try { description = value.description } catch { /* summary without a description */ }
      if (description === undefined) return this.summary('unreadable', { type: 'symbol' })
      const bounded = boundDebugString(description, 256)
      return this.summary('unreadable', {
        type: 'symbol',
        description: bounded.value,
        ...(bounded.atLeastBytes === undefined ? {} : { description_at_least_bytes: bounded.atLeastBytes }),
      })
    }

    const objectValue = value as object
    if (this.windowValue(objectValue)) return this.summary('unreadable', { type: 'window' })
    if (this.domNode(objectValue)) return this.summary('unreadable', { type: 'dom_node' })
    const error = this.errorValue(objectValue)
    if (error) return this.serializeError(error)
    if (depth >= DEBUG_SERIALIZER_MAX_DEPTH) return this.summary('max_depth', { type: 'object' })
    if (this.seen.has(objectValue)) return this.summary('circular', { type: 'object' })

    this.seen.add(objectValue)
    try {
      let isArray = false
      try { isArray = Array.isArray(objectValue) } catch { return this.summary('unreadable', { type: 'array' }) }
      if (isArray) {
        const output: JsonValue[] = []
        if (!this.reserve(2)) return this.summary('budget', { type: 'array', truncated: true })
        let length: number
        try {
          const descriptor = Object.getOwnPropertyDescriptor(objectValue, 'length')
          if (!descriptor || !('value' in descriptor) || typeof descriptor.value !== 'number') {
            return this.summary('unreadable', { type: 'array' })
          }
          length = descriptor.value
        } catch {
          return this.summary('unreadable', { type: 'array' })
        }
        const count = Math.min(length, DEBUG_SERIALIZER_MAX_ARRAY_ELEMENTS)
        for (let index = 0; index < count; index += 1) {
          if (!this.reserve(1)) {
            output.push(this.summary('budget', { type: 'array', truncated: true }))
            break
          }
          output.push(this.property(objectValue, String(index), depth + 1))
        }
        if (length > count) output.push(this.summary('max_elements', { type: 'array', remaining: length - count }))
        return output
      }

      const output: JsonObject = {}
      if (!this.reserve(2)) return this.summary('budget', { type: 'object', truncated: true })
      let count = 0
      let truncatedKeys = false
      try {
        // A bounded for-in loop avoids retaining an unbounded Object.keys
        // result. Descriptors are used for the selected properties, so no
        // getter is invoked.
        for (const key in objectValue) {
          if (!Object.prototype.hasOwnProperty.call(objectValue, key)) continue
          if (count >= DEBUG_SERIALIZER_MAX_OBJECT_KEYS) {
            truncatedKeys = true
            break
          }
          const boundedKey = boundDebugString(key, 256).value
          const encodedKey = JSON.stringify(boundedKey)
          const keyBytes = typeof encodedKey === 'string' ? byteLength(encodedKey) : 0
          if (!this.reserve(keyBytes + 1)) {
            truncatedKeys = true
            break
          }
          output[boundedKey] = this.property(objectValue, key, depth + 1)
          count += 1
        }
      } catch {
        return this.summary('unreadable', { type: 'object' })
      }
      if (truncatedKeys) output.__sai_debug_truncated_keys = true
      return output
    } finally {
      this.seen.delete(objectValue)
    }
  }

  private property(objectValue: object, key: string, depth: number): JsonValue {
    try {
      const descriptor = Object.getOwnPropertyDescriptor(objectValue, key)
      if (!descriptor || !('value' in descriptor)) return this.summary('unreadable', { type: 'accessor', key })
      return this.value(descriptor.value, depth)
    } catch {
      return this.summary('unreadable', { type: 'property', key })
    }
  }

  private string(value: string): JsonValue {
    const bounded = boundDebugString(value, DEBUG_SERIALIZER_MAX_STRING_BYTES)
    if (!bounded.truncated) return this.withBudget(bounded.value)
    return this.summary('max_string', {
      type: 'string',
      value: bounded.value,
      ...(bounded.atLeastBytes === undefined ? {} : { at_least_bytes: bounded.atLeastBytes }),
      truncated: true,
    })
  }

  private serializeError(error: Error): JsonObject {
    const name = this.ownString(error, 'name') ?? 'Error'
    const message = this.ownString(error, 'message') ?? ''
    const stack = this.ownString(error, 'stack')
    return {
      __sai_debug: 'error',
      name: this.string(name),
      message: this.string(message),
      ...(stack === undefined ? {} : { stack: this.string(stack) }),
    }
  }

  private ownString(objectValue: object, key: string): string | undefined {
    try {
      const descriptor = Object.getOwnPropertyDescriptor(objectValue, key)
      if (descriptor && 'value' in descriptor && typeof descriptor.value === 'string') return descriptor.value
    } catch { /* summarize an inaccessible property without invoking it */ }
    return undefined
  }

  private summary(reason: Reason, extra: JsonObject = {}): JsonValue {
    return this.withBudget(marker(reason, extra))
  }

  private withBudget(value: JsonValue): JsonValue {
    const encoded = JSON.stringify(value)
    if (typeof encoded !== 'string') return marker('unreadable', { type: 'value' })
    const bytes = byteLength(encoded)
    if (this.usedBytes + bytes > this.contentBudget) return marker('budget', { type: 'value', truncated: true })
    this.usedBytes += bytes
    return value
  }

  private reserve(bytes: number): boolean {
    if (this.usedBytes + bytes > this.contentBudget) return false
    this.usedBytes += bytes
    return true
  }

  private windowValue(value: object): boolean {
    try { return value === this.windowRef || value === globalThis || value === (globalThis as unknown as { window?: unknown }).window } catch { return false }
  }

  private domNode(value: object): boolean {
    try { return typeof Node !== 'undefined' && value instanceof Node } catch { return false }
  }

  private errorValue(value: object): Error | undefined {
    try { return value instanceof Error ? value : undefined } catch { return undefined }
  }
}

export interface DebugInlineExecutionResult {
  readonly value: JsonValue
  readonly console: Array<{ level: 'log' | 'info' | 'warn' | 'error' | 'debug'; arguments: JsonValue[] }>
}

export function serializeDebugExecution(value: unknown, consoleEntries: Array<{ level: DebugInlineExecutionResult['console'][number]['level']; arguments: unknown[] }>, windowRef?: Window): DebugInlineExecutionResult {
  const serializer = new DebugInlineSerializer(DEBUG_INLINE_RESULT_BUDGET, windowRef)
  const serializedConsole: DebugInlineExecutionResult['console'] = []
  for (const entry of consoleEntries.slice(0, DEBUG_SERIALIZER_MAX_CONSOLE_ENTRIES)) {
    serializedConsole.push({
      level: entry.level,
      arguments: entry.arguments.slice(0, DEBUG_SERIALIZER_MAX_CONSOLE_ARGUMENTS).map((argument) => serializer.serialize(argument)),
    })
  }
  if (consoleEntries.length > serializedConsole.length) {
    serializedConsole.push({ level: 'debug', arguments: [marker('max_elements', { type: 'console', remaining: consoleEntries.length - serializedConsole.length })] })
  }
  let result: DebugInlineExecutionResult = { value: serializer.serialize(value), console: serializedConsole }
  try {
    if (byteLength(JSON.stringify(result)) <= DEBUG_INLINE_RESULT_BUDGET) return result
  } catch { /* use the stable minimal result below */ }
  result = { value: marker('budget', { type: 'result', truncated: true }), console: [] }
  return result
}
