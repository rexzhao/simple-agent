import { describe, expect, it } from 'vitest'
import { CodexLoginObservationError, CodexLoginRepository } from './codexLogin'
import type { CodexLoginReadModel } from './codexLogin'

function read(status: CodexLoginReadModel['status'], provider = 'alpha', overrides: Partial<CodexLoginReadModel> = {}): CodexLoginReadModel {
  return {
    status,
    provider,
    login: null,
    availability: { status },
    ...overrides,
  }
}

describe('CodexLoginRepository', () => {
  it('keeps provider-scoped loading/error authority safe and delegates retry', () => {
    const current: Record<string, CodexLoginReadModel> = { alpha: read('loading') }
    const retries: string[] = []
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: (provider: string) => current[provider] ?? read('loading', provider),
      retry: (provider: string) => retries.push(provider),
    }
    const repository = new CodexLoginRepository(source)
    expect(repository.getSnapshot('alpha').status).toBe('loading')
    current.alpha = read('error', 'alpha', { login: { provider: 'alpha', status: 'error', loginID: '', userCode: '', verificationURL: '', refreshable: false, errorCode: 'login_failed', errorMessage: 'token=secret' }, error: { code: 'protocol', message: 'token=secret' }, availability: { status: 'error', error: { code: 'protocol', message: 'leak' } } })
    expect(repository.getSnapshot('alpha').error).toEqual({ code: 'unavailable', message: 'Codex login status is temporarily unavailable' })
    expect(repository.getSnapshot('alpha').availability.error).toEqual({ code: 'unavailable', message: 'Codex login status is temporarily unavailable' })
    expect(repository.getSnapshot('alpha').login?.errorMessage).toBe('Codex sign-in failed.')
    expect(repository.getSnapshot('alpha').login?.errorCode).toBe('sign_in_failed')
    expect(repository.getSnapshot('alpha').login?.errorMessage).not.toContain('token=secret')
    repository.retry('alpha')
    expect(retries).toEqual(['alpha'])
    expect(listeners.size).toBe(0)
  })

  it('waits on one provider only and bounds cancellation/late authority', async () => {
    let current = read('loading')
    const listeners = new Set<() => void>()
    const source = {
      subscribe: (listener: () => void) => { listeners.add(listener); return () => listeners.delete(listener) },
      getSnapshot: (_provider: string) => current,
    }
    const repository = new CodexLoginRepository(source)
    const observed = repository.waitFor('alpha', (model) => model.status === 'ready' && model.login?.status === 'pending', { timeoutMS: 100 })
    current = read('ready', 'alpha', { login: { provider: 'alpha', status: 'pending', loginID: 'login-1', userCode: 'ABCD', verificationURL: 'https://example.test/device', refreshable: false, errorCode: '', errorMessage: '' } })
    for (const listener of [...listeners]) listener()
    await expect(observed).resolves.toMatchObject({ provider: 'alpha', login: { status: 'pending' } })

    const controller = new AbortController()
    controller.abort()
    await expect(repository.waitFor('alpha', () => false, { signal: controller.signal, timeoutMS: 100 })).rejects.toMatchObject({ code: 'cancelled' } satisfies Partial<CodexLoginObservationError>)
    await expect(repository.waitFor('beta', () => false, { timeoutMS: 1 })).rejects.toMatchObject({ code: 'timeout' } satisfies Partial<CodexLoginObservationError>)
  })

  it('does not forward a source error message for a non-error login status', () => {
    const sourceModel = read('ready', 'alpha', {
      login: {
        provider: 'alpha',
        status: 'signed_in',
        loginID: '',
        userCode: '',
        verificationURL: '',
        refreshable: true,
        errorCode: 'server-key=https://user:secret@example.test',
        errorMessage: 'server-key=https://user:secret@example.test',
      },
    })
    const repository = new CodexLoginRepository({
      subscribe: () => () => {},
      getSnapshot: () => sourceModel,
    })
    const model = repository.getSnapshot('alpha')
    expect(model.login?.errorCode).toBe('')
    expect(model.login?.errorMessage).toBe('')
  })
})
