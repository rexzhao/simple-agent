import { readdirSync, readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

function productSourceFiles(directory: URL): URL[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const url = new URL(`${entry.name}${entry.isDirectory() ? '/' : ''}`, directory)
    if (entry.isDirectory()) return productSourceFiles(url)
    return /\.(?:ts|tsx)$/u.test(entry.name) && !/\.test\.(?:ts|tsx)$/u.test(entry.name) ? [url] : []
  })
}

describe('run-control page cutover', () => {
  it('keeps run lifecycle and transport ownership below the application boundary', () => {
    const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8')
    const conversation = readFileSync(new URL('./components/Conversation.tsx', import.meta.url), 'utf8')
    const api = readFileSync(new URL('./api.ts', import.meta.url), 'utf8')

    expect(app).not.toMatch(/from ['"]\.\/api['"]/)
    expect(app).not.toMatch(/stream(?:Run|Lifecycle)|useRunRegistry|runEventReducer|api\.(?:startRun|continueRun|cancelRun|activeRuns)\s*\(/)
    expect(app).toContain('runCommands.startRun')
    expect(app).toContain('runCommands.continueRun')
    expect(app).toContain('activeRunForConversation(sessionContentView')
    expect(conversation).not.toMatch(/from ['"]\.\.\/api['"]|api\.sessionImage\s*\(/)

    expect(api).not.toMatch(/stream(?:Run|Lifecycle)/)
    expect(api).not.toMatch(/\/api\/runs|\/runs|\/continue/)
  })

  it('keeps deleted lifecycle and reducer systems out of all product source', () => {
    const forbidden = /\b(?:streamLifecycle|streamRun|useRunRegistry|runEventReducer|lifecycleReducer|LifecycleEvent|RunEvent|ActiveRunDescriptor|RunAdmission)\b/u
    for (const url of productSourceFiles(new URL('.', import.meta.url))) {
      const source = readFileSync(url, 'utf8')
      expect(source, url.pathname).not.toMatch(forbidden)
    }
  })
})
