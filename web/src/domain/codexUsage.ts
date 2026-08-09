/**
 * Normalized, page-safe Codex usage data. The typed command result is converted
 * to this shape at the application boundary while transport details remain
 * uninvolved.
 */
export interface CodexUsageWindowDomain {
  readonly usedPercent: number
  readonly limitWindowSeconds: number
  readonly resetAfterSeconds: number
  readonly resetAt: number
}

export interface CodexUsageWindowSetDomain {
  readonly allowed: boolean
  readonly limitReached: boolean
  readonly primaryWindow?: CodexUsageWindowDomain
  readonly secondaryWindow?: CodexUsageWindowDomain | null
}

export interface CodexUsageAdditionalDomain {
  readonly limitName: string
  readonly meteredFeature: string
  readonly rateLimit?: CodexUsageWindowSetDomain
}

export interface CodexUsageCreditsDomain {
  readonly hasCredits: boolean
  readonly unlimited: boolean
  readonly overageLimitReached: boolean
  readonly balance: string
}

export interface CodexUsageDomain {
  readonly planType: string
  readonly rateLimit?: CodexUsageWindowSetDomain
  readonly additionalRateLimits?: readonly CodexUsageAdditionalDomain[]
  readonly credits?: CodexUsageCreditsDomain
}

const MAX_ADDITIONAL_LIMITS = 64
const MAX_TEXT_BYTES = 512
const MAX_WINDOW_SECONDS = 1_000_000_000
const MAX_RESET_AT = 10_000_000_000_000

function record(value: unknown, message: string): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error(message)
  return value as Record<string, unknown>
}

function text(value: unknown, field: string): string {
  if (typeof value !== 'string' || new TextEncoder().encode(value).byteLength > MAX_TEXT_BYTES) throw new Error(`${field} is invalid`)
  return value
}

function booleanValue(value: unknown, field: string): boolean {
  if (typeof value !== 'boolean') throw new Error(`${field} is invalid`)
  return value
}

function boundedNumber(value: unknown, field: string, maximum: number): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new Error(`${field} is invalid`)
  return Math.max(0, Math.min(maximum, Math.trunc(value)))
}

function usageWindow(value: unknown): CodexUsageWindowDomain | undefined {
  if (value === undefined || value === null) return undefined
  const source = record(value, 'Codex usage window is invalid')
  return {
    usedPercent: boundedNumber(source.used_percent, 'used_percent', 100),
    limitWindowSeconds: boundedNumber(source.limit_window_seconds, 'limit_window_seconds', MAX_WINDOW_SECONDS),
    resetAfterSeconds: boundedNumber(source.reset_after_seconds, 'reset_after_seconds', MAX_WINDOW_SECONDS),
    resetAt: boundedNumber(source.reset_at, 'reset_at', MAX_RESET_AT),
  }
}

function usageWindowSet(value: unknown): CodexUsageWindowSetDomain | undefined {
  if (value === undefined || value === null) return undefined
  const source = record(value, 'Codex usage rate limit is invalid')
  return {
    allowed: booleanValue(source.allowed, 'allowed'),
    limitReached: booleanValue(source.limit_reached, 'limit_reached'),
    primaryWindow: usageWindow(source.primary_window),
    secondaryWindow: source.secondary_window === null ? null : usageWindow(source.secondary_window),
  }
}

function usageCredits(value: unknown): CodexUsageCreditsDomain | undefined {
  if (value === undefined || value === null) return undefined
  const source = record(value, 'Codex usage credits are invalid')
  return {
    hasCredits: booleanValue(source.has_credits, 'has_credits'),
    unlimited: booleanValue(source.unlimited, 'unlimited'),
    overageLimitReached: booleanValue(source.overage_limit_reached, 'overage_limit_reached'),
    balance: text(source.balance, 'balance'),
  }
}

/**
 * Accept only the bounded fields that the page renders. Identity fields,
 * account information, and unknown future payload fields never cross this
 * application domain boundary.
 */
export function codexUsageDomain(value: unknown): CodexUsageDomain {
  const source = record(value, 'Codex usage response is invalid')
  const rawAdditional = source.additional_rate_limits
  if (rawAdditional !== undefined && rawAdditional !== null && !Array.isArray(rawAdditional)) throw new Error('additional_rate_limits is invalid')
  const additional = rawAdditional === undefined || rawAdditional === null
    ? undefined
    : rawAdditional.slice(0, MAX_ADDITIONAL_LIMITS).map((value, index) => {
      const item = record(value, `additional_rate_limits[${index}] is invalid`)
      return {
        limitName: text(item.limit_name, 'limit_name'),
        meteredFeature: text(item.metered_feature, 'metered_feature'),
        rateLimit: usageWindowSet(item.rate_limit),
      }
    })
  return {
    planType: text(source.plan_type, 'plan_type'),
    rateLimit: usageWindowSet(source.rate_limit),
    additionalRateLimits: additional,
    credits: usageCredits(source.credits),
  }
}
