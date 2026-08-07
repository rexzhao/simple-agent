// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { creditsLabel, usageWindowRows, windowDurationLabel } from './ProviderManagerDialog'
import { codexUsageDomain } from '../domain/codexUsage'

describe('Codex usage display helpers', () => {
  it('formats window durations from seconds', () => {
    expect(windowDurationLabel(604800)).toBe('7 d')
    expect(windowDurationLabel(18000)).toBe('5 h')
    expect(windowDurationLabel(3600)).toBe('1 h')
    expect(windowDurationLabel(1500)).toBe('25 min')
    expect(windowDurationLabel(86400)).toBe('1 d')
    expect(windowDurationLabel(0)).toBe('usage')
  })

  it('builds one row per present window, skipping null secondary', () => {
    const rows = usageWindowRows({
      allowed: true,
      limitReached: false,
      primaryWindow: { usedPercent: 58, limitWindowSeconds: 604800, resetAfterSeconds: 300, resetAt: 1786255839 },
      secondaryWindow: null,
    })
    expect(rows).toHaveLength(1)
    expect(rows[0].label).toBe('Window · 7 d')
    expect(rows[0].window?.usedPercent).toBe(58)

    const both = usageWindowRows({
      allowed: true,
      limitReached: false,
      primaryWindow: { usedPercent: 10, limitWindowSeconds: 604800, resetAfterSeconds: 300, resetAt: 1 },
      secondaryWindow: { usedPercent: 5, limitWindowSeconds: 18000, resetAfterSeconds: 60, resetAt: 2 },
    })
    expect(both.map((row) => row.label)).toEqual(['Window · 7 d', 'Window · 5 h'])
  })

  it('returns no rows when the rate limit set is absent', () => {
    expect(usageWindowRows(undefined)).toEqual([])
  })

  it('labels credits from the response', () => {
    expect(creditsLabel({ hasCredits: false, unlimited: false, overageLimitReached: false, balance: '0' })).toBe('no separate credits')
    expect(creditsLabel({ hasCredits: true, unlimited: false, overageLimitReached: false, balance: '$5.00' })).toBe('balance $5.00')
    expect(creditsLabel({ hasCredits: false, unlimited: true, overageLimitReached: false, balance: '0' })).toBe('Unlimited')
  })

  it('keeps the REST compatibility response bounded and drops identity fields', () => {
    const usage = codexUsageDomain({
      user_id: 'private-user',
      email: 'person@example.test',
      plan_type: 'pro',
      rate_limit: {
        allowed: true,
        limit_reached: false,
        primary_window: { used_percent: 900, limit_window_seconds: 604800, reset_after_seconds: 60, reset_at: 1786255839 },
        secondary_window: null,
      },
      additional_rate_limits: Array.from({ length: 100 }, (_, index) => ({ limit_name: `limit-${index}`, metered_feature: 'feature', rate_limit: null })),
      credits: { has_credits: true, unlimited: false, overage_limit_reached: false, balance: '$5.00' },
    })
    expect(usage).not.toHaveProperty('user_id')
    expect(usage).not.toHaveProperty('email')
    expect(usage.rateLimit?.primaryWindow?.usedPercent).toBe(100)
    expect(usage.additionalRateLimits).toHaveLength(64)
  })

  it('rejects malformed usage rather than rendering an unbounded transport value', () => {
    expect(() => codexUsageDomain({ plan_type: 'pro', rate_limit: { allowed: 'yes' } })).toThrow()
  })
})