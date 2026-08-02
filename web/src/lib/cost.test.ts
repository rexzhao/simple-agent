import { describe, expect, it } from 'vitest'
import { contextUsage, usageCost, usageCostBreakdown, usageBreakdownFromEvents } from './cost'

describe('usageCost', () => {
  it('charges cache hits, cache misses, cache writes, and output per million tokens', () => {
    expect(usageCost({ inputTokens: 100, outputTokens: 50, totalTokens: 200, cachedTokens: 25, cacheWriteTokens: 25, reasoningTokens: 10 }, {
      input_cache_hit: 0.1,
      input_cache_miss: 1,
      output: 2,
    })).toBeCloseTo(0.0002275)
  })

  it('reads cumulative usage from session context', () => {
    expect(contextUsage({ context_window: 1000, context_window_source: 'configured', warning_threshold_percent: 80, total_input_tokens: 10, total_output_tokens: 5, total_tokens: 20, total_cached_tokens: 3, total_cache_write_tokens: 2, total_reasoning_tokens: 1 })).toMatchObject({
      inputTokens: 10,
      outputTokens: 5,
      totalTokens: 20,
      cachedTokens: 3,
      cacheWriteTokens: 2,
    })
  })

  it('uses long-context prices for requests above the configured threshold', () => {
    const pricing = {
      input_cache_hit: 0.5,
      input_cache_miss: 5,
      cache_write: 6.25,
      output: 30,
      long_context_threshold: 100,
      long_context: { input_cache_hit: 1, input_cache_miss: 10, cache_write: 12.5, output: 45 },
    }
    const breakdown = usageBreakdownFromEvents([
      { inputTokens: 90, outputTokens: 1, totalTokens: 91, cachedTokens: 5, cacheWriteTokens: 0, reasoningTokens: 0 },
      { inputTokens: 101, outputTokens: 2, totalTokens: 103, cachedTokens: 0, cacheWriteTokens: 0, reasoningTokens: 0 },
    ], pricing)
    expect(usageCostBreakdown(breakdown, pricing)).toBeCloseTo((90 * 5 + 5 * 0.5 + 1 * 30 + 101 * 10 + 2 * 45) / 1_000_000)
  })
})
