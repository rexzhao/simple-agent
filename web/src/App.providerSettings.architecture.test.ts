import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('Provider settings page cutover', () => {
  it('opens through the application repository signal rather than an HTTP read', () => {
    const source = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8')
    const apiSource = readFileSync(new URL('./api.ts', import.meta.url), 'utf8')
    expect(source).not.toMatch(/api\.providerSettings\s*\(/)
    expect(source).not.toMatch(/api\.(?:createProvider|updateProvider|updateProviderDefault|discoverProviderModels|startCodexLogin|codexLoginStatus|clearCodexLogin)\s*\(/)
    expect(source).not.toMatch(/function providerAuthorityMatches|function providerModelAuthorityMatches|JSON\.stringify\(domainLevels\)/)
    expect(source).toContain('waitForProviderPublication')
    expect(apiSource).not.toMatch(/\b(?:providerSettings|createProvider|updateProvider|updateProviderDefault|discoverProviderModels|startCodexLogin|codexLoginStatus|clearCodexLogin|sessionModels|codexUsage|projects|sessions|session|createSession)\s*:/)
  })
})
