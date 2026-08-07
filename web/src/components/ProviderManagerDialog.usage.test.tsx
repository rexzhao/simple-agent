// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { creditsLabel, usageWindowRows, windowDurationLabel } from './ProviderManagerDialog'

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
      limit_reached: false,
      primary_window: { used_percent: 58, limit_window_seconds: 604800, reset_after_seconds: 300, reset_at: 1786255839 },
      secondary_window: null,
    })
    expect(rows).toHaveLength(1)
    expect(rows[0].label).toBe('Window · 7 d')
    expect(rows[0].window?.used_percent).toBe(58)

    const both = usageWindowRows({
      allowed: true,
      limit_reached: false,
      primary_window: { used_percent: 10, limit_window_seconds: 604800, reset_after_seconds: 300, reset_at: 1 },
      secondary_window: { used_percent: 5, limit_window_seconds: 18000, reset_after_seconds: 60, reset_at: 2 },
    })
    expect(both.map((row) => row.label)).toEqual(['Window · 7 d', 'Window · 5 h'])
  })

  it('returns no rows when the rate limit set is absent', () => {
    expect(usageWindowRows(undefined)).toEqual([])
  })

  it('labels credits from the response', () => {
    expect(creditsLabel({ has_credits: false, unlimited: false, overage_limit_reached: false, balance: '0' })).toBe('no separate credits')
    expect(creditsLabel({ has_credits: true, unlimited: false, overage_limit_reached: false, balance: '$5.00' })).toBe('balance $5.00')
    expect(creditsLabel({ has_credits: false, unlimited: true, overage_limit_reached: false, balance: '0' })).toBe('Unlimited')
  })
})