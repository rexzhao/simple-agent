import { LocalReplica, resourceKeyString } from './localReplica'
import type { SyncReadError } from './errors'
import type { CodexLoginData } from './codexLoginAdapter'
import type { CodexLoginAvailability, CodexLoginDomain, CodexLoginReadError, CodexLoginReadModel, CodexLoginReadState, CodexLoginSource } from '../repositories/codexLogin'

interface CachedModel {
  value: unknown
  initialized: boolean
  readState: string
  error?: SyncReadError
  model: CodexLoginReadModel
}

function domainError(error: SyncReadError | undefined): CodexLoginReadError | undefined {
  return error ? { code: 'unavailable', message: 'Codex login status is temporarily unavailable' } : undefined
}

function domainLogin(value: CodexLoginData): CodexLoginDomain {
  const error = value.status === 'error'
    ? { code: 'sign_in_failed', message: 'Codex sign-in failed.' }
    : value.status === 'expired'
      ? { code: 'session_expired', message: 'Codex sign-in has expired.' }
      : { code: '', message: '' }
  return {
    provider: value.provider,
    status: value.status,
    loginID: value.loginID,
    userCode: value.userCode,
    verificationURL: value.verificationURL,
    refreshable: value.refreshable,
    errorCode: error.code,
    errorMessage: error.message,
  }
}

/** Pure local projection over LocalReplica. No command result is written here;
 * only snapshots/changes accepted by the sync runtime invalidate this store. */
export class CodexLoginStore implements CodexLoginSource {
  readonly replica: LocalReplica
  private readonly models = new Map<string, CachedModel>()
  private readonly listeners = new Set<() => void>()
  private readonly unsubscribeReplica: () => void

  constructor(replica = new LocalReplica(), private readonly retryResource?: (provider: string) => void) {
    this.replica = replica
    this.unsubscribeReplica = replica.subscribe((changed) => {
      if (changed.type !== 'codex_login') return
      this.models.delete(resourceKeyString(changed))
      for (const listener of [...this.listeners]) listener()
    })
  }

  dispose(): void {
    this.unsubscribeReplica()
    this.listeners.clear()
    this.models.clear()
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  getSnapshot(provider: string): CodexLoginReadModel {
    const resource = { type: 'codex_login' as const, id: provider }
    const record = this.replica.get<CodexLoginData>(resource)
    const key = resourceKeyString(resource)
    const cached = this.models.get(key)
    if (cached && cached.value === record.value && cached.initialized === record.initialized && cached.readState === record.metadata.readState && cached.error === record.metadata.error) return cached.model
    const status: CodexLoginReadState = !record.initialized
      ? (record.metadata.readState === 'error' ? 'error' : 'loading')
      : (record.metadata.readState === 'error' ? 'stale' : record.metadata.readState)
    const error = domainError(record.metadata.error)
    const availability: CodexLoginAvailability = !record.initialized
      ? (record.metadata.readState === 'error' ? { status: 'error', error } : { status: 'loading' })
      : (record.metadata.readState === 'error' || record.metadata.readState === 'stale' || error ? { status: 'stale', error } : { status: 'ready' })
    const model: CodexLoginReadModel = {
      status,
      provider,
      login: record.value ? domainLogin(record.value) : null,
      availability,
      error,
    }
    this.models.set(key, { value: record.value, initialized: record.initialized, readState: record.metadata.readState, error: record.metadata.error, model })
    return model
  }

  hasSnapshot(provider: string): boolean { return this.replica.get({ type: 'codex_login' as const, id: provider }).initialized }
  retry(provider: string): void { this.retryResource?.(provider) }
  resourceKey(provider: string): string { return resourceKeyString({ type: 'codex_login' as const, id: provider }) }
}
