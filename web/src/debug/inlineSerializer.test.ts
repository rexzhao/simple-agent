// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { DEBUG_INLINE_RESULT_BUDGET, DebugInlineSerializer, serializeDebugExecution } from './inlineSerializer'

describe('bounded web debug inline serializer', () => {
  it('summarizes special values, DOM/window values, cycles, and throwing properties', () => {
    const cycle: Record<string, unknown> = {}
    cycle.self = cycle
    const throwing: Record<string, unknown> = {}
    Object.defineProperty(throwing, 'secret', { get: () => { throw new Error('no read') }, enumerable: true })
    const value = {
      undefinedValue: undefined,
      nan: Number.NaN,
      infinity: Number.POSITIVE_INFINITY,
      bigint: 10n,
      functionValue: () => 'not called',
      symbolValue: Symbol('hidden'),
      error: new Error('boom'),
      cycle,
      throwing,
      node: document.body,
      window,
    }
    const result = serializeDebugExecution(value, [{ level: 'log', arguments: [value] }], window)
    expect(result.value).toMatchObject({
      undefinedValue: { type: 'undefined' },
      nan: { type: 'number', value: 'NaN' },
      infinity: { type: 'number', value: 'Infinity' },
      bigint: { type: 'bigint' },
      functionValue: { type: 'function' },
      symbolValue: { type: 'symbol', description: 'hidden' },
      error: { __sai_debug: 'error', message: 'boom' },
      cycle: { self: { reason: 'circular' } },
      throwing: { secret: { reason: 'unreadable' } },
      node: { type: 'dom_node' },
      window: { type: 'window' },
    })
    expect(result.console).toHaveLength(1)
    expect(JSON.stringify(result).length).toBeLessThan(DEBUG_INLINE_RESULT_BUDGET)
  })

  it('uses hard summaries for depth, strings, arrays, objects, and total budget', () => {
    const deep: Record<string, unknown> = {}
    let current = deep
    for (let index = 0; index < 20; index += 1) {
      current.child = {}
      current = current.child as Record<string, unknown>
    }
    const huge = {
      text: 'x'.repeat(100_000),
      values: Array.from({ length: 500 }, (_, index) => index),
      keys: Object.fromEntries(Array.from({ length: 200 }, (_, index) => [`key-${index}`, index])),
      deep,
    }
    const result = serializeDebugExecution(huge, [], window)
    const encoded = new TextEncoder().encode(JSON.stringify(result)).byteLength
    expect(encoded).toBeLessThanOrEqual(DEBUG_INLINE_RESULT_BUDGET)
    expect(JSON.stringify(result)).toContain('summary')

    const serializer = new DebugInlineSerializer()
    expect(() => serializer.serialize(new Proxy({}, { ownKeys: () => { throw new Error('proxy') } }))).not.toThrow()
    const revoked = Proxy.revocable({}, {})
    revoked.revoke()
    expect(() => serializer.serialize(revoked.proxy)).not.toThrow()
  })

  it('bounds strings and metadata without calling getters, toJSON, or user conversion', () => {
    let getterCalls = 0
    let toJSONCalls = 0
    const pseudoDOM = {
      get nodeType() { getterCalls += 1; return 1 },
      get nodeName() { getterCalls += 1; return 'DIV' },
    }
    const array = new Proxy([1, 2, 3], {
      get(_target, property) {
        if (property === 'length') throw new Error('length getter must not run')
        getterCalls += 1
        return undefined
      },
    })
    const toJSON = {
      toJSON() { toJSONCalls += 1; return { leaked: true } },
      value: 1,
    }
    const hugeError = new Error()
    Object.defineProperty(hugeError, 'name', { value: '名'.repeat(100_000), enumerable: true })
    Object.defineProperty(hugeError, 'message', { value: '错误'.repeat(100_000), enumerable: true })
    Object.defineProperty(hugeError, 'stack', { value: 'stack'.repeat(100_000), enumerable: true })
    const hugeSymbol = Symbol('符'.repeat(1_000_000))
    const hugeBigInt = BigInt('9'.repeat(1_000_000))
    const result = serializeDebugExecution({
      text: '🦄'.repeat(1_000_000),
      pseudoDOM,
      array,
      toJSON,
      hugeError,
      hugeSymbol,
      hugeBigInt,
    }, [], window)

    expect(getterCalls).toBe(0)
    expect(toJSONCalls).toBe(0)
    const encoded = new TextEncoder().encode(JSON.stringify(result)).byteLength
    expect(encoded).toBeLessThanOrEqual(DEBUG_INLINE_RESULT_BUDGET)
    expect(JSON.stringify(result)).not.toContain('original_bytes')
    expect(result.value).toMatchObject({
      text: { reason: 'max_string', truncated: true, at_least_bytes: 8193 },
      hugeBigInt: { type: 'bigint', truncated: true },
      hugeSymbol: { type: 'symbol', description_at_least_bytes: 257 },
      hugeError: { __sai_debug: 'error', name: { reason: 'max_string' }, message: { reason: 'max_string' } },
    })
  })
})
