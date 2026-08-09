import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

describe('read command composition', () => {
  it('routes session model and Codex usage reads through the typed command facade', () => {
    const source = readFileSync(new URL('./applicationComposition.ts', import.meta.url), 'utf8')
    const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
    expect(source).toContain('commandFacade.readModels(projectID)')
    expect(source).toContain('commandFacade.readCodexUsage(provider)')
    expect(source).not.toMatch(/api\.(?:sessionModels|codexUsage)\s*\(/)
    expect(apiSource).not.toMatch(/\b(?:sessionModels|codexUsage|projects|sessions|session|createSession)\s*:/)
  })
})
