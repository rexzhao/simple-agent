import type { ContextMetadata, ModelPricing, ModelPricingTier } from '../types'

export interface TokenUsage {
  inputTokens: number
  outputTokens: number
  totalTokens: number
  cachedTokens: number
  cacheWriteTokens: number
  reasoningTokens: number
}

export interface UsageBreakdown {
  total: TokenUsage
  shortContext?: TokenUsage
  longContext?: TokenUsage
}

const zeroUsage = (): TokenUsage => ({
  inputTokens: 0,
  outputTokens: 0,
  totalTokens: 0,
  cachedTokens: 0,
  cacheWriteTokens: 0,
  reasoningTokens: 0,
})

export function usageCost(usage: TokenUsage | undefined, pricing: ModelPricing | undefined): number | undefined {
  if (!usage || !pricing) return undefined
  return usageCostForTier(usage, pricing, false)
}

export function usageCostBreakdown(breakdown: UsageBreakdown | undefined, pricing: ModelPricing | undefined): number | undefined {
  if (!breakdown || !pricing) return undefined
  if (!breakdown.shortContext && !breakdown.longContext) return usageCost(breakdown.total, pricing)
  return (breakdown.shortContext ? usageCostForTier(breakdown.shortContext, pricing, false) : 0)
    + (breakdown.longContext ? usageCostForTier(breakdown.longContext, pricing, true) : 0)
}

function usageCostForTier(usage: TokenUsage, pricing: ModelPricing, longContext: boolean): number {
  const tier = longContext && pricing.long_context ? pricing.long_context : shortContextPricing(pricing)
  const inputMissTokens = Math.max(0, usage.inputTokens)
  const inputHitTokens = Math.max(0, usage.cachedTokens)
  const cacheWriteTokens = Math.max(0, usage.cacheWriteTokens)
  const outputTokens = Math.max(0, usage.outputTokens)
  return (
    inputMissTokens * Math.max(0, tier.input_cache_miss)
    + inputHitTokens * Math.max(0, tier.input_cache_hit)
    + cacheWriteTokens * Math.max(0, tier.cache_write)
    + outputTokens * Math.max(0, tier.output)
  ) / 1_000_000
}

function shortContextPricing(pricing: ModelPricing): ModelPricingTier {
  return {
    input_cache_hit: pricing.input_cache_hit,
    input_cache_miss: pricing.input_cache_miss,
    // Legacy three-rate configurations did not have a separate cache-write
    // price, so preserve their previous behavior by using cache-miss pricing.
    cache_write: pricing.cache_write ?? pricing.input_cache_miss,
    output: pricing.output,
  }
}

export function contextUsage(context: ContextMetadata | undefined): TokenUsage | undefined {
  if (!context) return undefined
  const short = usageFromContext(context, 'short')
  const long = usageFromContext(context, 'long')
  const inputTokens = numberOrZero(context.total_input_tokens) || (short?.inputTokens ?? 0) + (long?.inputTokens ?? 0)
  const outputTokens = numberOrZero(context.total_output_tokens) || (short?.outputTokens ?? 0) + (long?.outputTokens ?? 0)
  const cachedTokens = numberOrZero(context.total_cached_tokens) || (short?.cachedTokens ?? 0) + (long?.cachedTokens ?? 0)
  const cacheWriteTokens = numberOrZero(context.total_cache_write_tokens) || (short?.cacheWriteTokens ?? 0) + (long?.cacheWriteTokens ?? 0)
  const reasoningTokens = numberOrZero(context.total_reasoning_tokens)
  const totalTokens = numberOrZero(context.total_tokens) || inputTokens + outputTokens + cachedTokens + cacheWriteTokens
  if (inputTokens <= 0 && outputTokens <= 0 && cachedTokens <= 0 && cacheWriteTokens <= 0) return undefined
  return { inputTokens, outputTokens, totalTokens, cachedTokens, cacheWriteTokens, reasoningTokens }
}

export function contextUsageBreakdown(context: ContextMetadata | undefined): UsageBreakdown | undefined {
  const total = contextUsage(context)
  if (!total || !context) return undefined
  const shortContext = usageFromContext(context, 'short')
  const longContext = usageFromContext(context, 'long')
  if (!shortContext && !longContext) return { total }
  return { total, shortContext, longContext }
}

function usageFromContext(context: ContextMetadata, kind: 'short' | 'long'): TokenUsage | undefined {
  const prefix = kind === 'short' ? 'total_short_' : 'total_long_'
  const inputTokens = numberOrZero(context[`${prefix}input_tokens` as keyof ContextMetadata] as number | undefined)
  const outputTokens = numberOrZero(context[`${prefix}output_tokens` as keyof ContextMetadata] as number | undefined)
  const cachedTokens = numberOrZero(context[`${prefix}cached_tokens` as keyof ContextMetadata] as number | undefined)
  const cacheWriteTokens = numberOrZero(context[`${prefix}cache_write_tokens` as keyof ContextMetadata] as number | undefined)
  if (inputTokens <= 0 && outputTokens <= 0 && cachedTokens <= 0 && cacheWriteTokens <= 0) return undefined
  return {
    inputTokens,
    outputTokens,
    cachedTokens,
    cacheWriteTokens,
    totalTokens: inputTokens + outputTokens + cachedTokens + cacheWriteTokens,
    reasoningTokens: 0,
  }
}

function numberOrZero(value: number | undefined): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

export function usageBreakdownFromEvents(events: TokenUsage[] | undefined, pricing: ModelPricing | undefined): UsageBreakdown | undefined {
  if (!events || events.length === 0) return undefined
  let total = zeroUsage()
  let shortContext: TokenUsage | undefined
  let longContext: TokenUsage | undefined
  for (const event of events) {
    total = addUsage(total, event)
    const inputTokens = Math.max(0, event.inputTokens) + Math.max(0, event.cachedTokens) + Math.max(0, event.cacheWriteTokens)
    const isLong = Boolean(pricing?.long_context && pricing.long_context_threshold && inputTokens > pricing.long_context_threshold)
    if (isLong) longContext = addUsage(longContext, event)
    else shortContext = addUsage(shortContext, event)
  }
  return { total, shortContext, longContext }
}

export function contextRequestCount(context: ContextMetadata | undefined): number {
  return Math.max(0, Math.floor(numberOrZero(context?.total_requests)))
}

export function usageEventCount(events: TokenUsage[] | undefined): number {
  return events?.length ?? 0
}

export function addUsage(base: TokenUsage | undefined, extra: TokenUsage | undefined): TokenUsage {
  const left = base ?? zeroUsage()
  const right = extra ?? zeroUsage()
  return {
    inputTokens: left.inputTokens + right.inputTokens,
    outputTokens: left.outputTokens + right.outputTokens,
    totalTokens: left.totalTokens + right.totalTokens,
    cachedTokens: left.cachedTokens + right.cachedTokens,
    cacheWriteTokens: left.cacheWriteTokens + right.cacheWriteTokens,
    reasoningTokens: left.reasoningTokens + right.reasoningTokens,
  }
}

export function addUsageBreakdown(base: UsageBreakdown | undefined, extra: UsageBreakdown | undefined): UsageBreakdown | undefined {
  if (!base && !extra) return undefined
  const total = addUsage(base?.total, extra?.total)
  const baseShort = base?.shortContext ?? (!base?.longContext ? base?.total : undefined)
  const extraShort = extra?.shortContext ?? (!extra?.longContext ? extra?.total : undefined)
  const shortContext = addUsage(baseShort, extraShort)
  const longContext = addUsage(base?.longContext, extra?.longContext)
  const hasShort = Boolean(base?.shortContext || extra?.shortContext || baseShort || extraShort)
  const hasLong = Boolean(base?.longContext || extra?.longContext)
  return {
    total,
    shortContext: hasShort ? shortContext : undefined,
    longContext: hasLong ? longContext : undefined,
  }
}
