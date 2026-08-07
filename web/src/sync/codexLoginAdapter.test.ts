import type { Sequence } from '../protocol/types'
import { describe, expect, it } from 'vitest'
import { LocalReplica } from './localReplica'
import { CodexLoginAdapter } from './codexLoginAdapter'
import { CodexLoginStore } from './codexLoginStore'

const signedOut = {
  provider: 'codex', status: 'signed_out', login_id: '', user_code: '', verification_url: '', refreshable: false, error_code: '', error_message: '',
}

const pending = {
  provider: 'codex', status: 'pending', login_id: 'login-1', user_code: 'ABCD-1234', verification_url: 'https://example.test/device', refreshable: false, error_code: '', error_message: '',
}

describe('CodexLoginAdapter', () => {
  it('strictly decodes safe snapshots and replaces one provider state', () => {
    const adapter = new CodexLoginAdapter('codex')
    const first = adapter.decodeSnapshot(signedOut)
    expect(first.status).toBe('signed_out')
    const changed = adapter.applyChange(first, [{ op: 'replace', key: 'codex', value: pending }])
    expect(changed.status).toBe('pending')
    expect(changed.loginID).toBe('login-1')
    expect(() => adapter.decodeSnapshot({ ...signedOut, unexpected: true })).toThrow()
    expect(() => adapter.applyChange(first, [{ op: 'replace', key: 'other', value: pending }])).toThrow()
    expect(() => adapter.decodeSnapshot({ ...pending, provider: 'other' })).toThrow()
  })

  it('rejects unsafe device URL, error shape, and invalid Unicode', () => {
    const adapter = new CodexLoginAdapter('codex')
    expect(() => adapter.decodeSnapshot({ ...pending, verification_url: 'https://example.test/device?token=secret' })).toThrow()
    expect(() => adapter.decodeSnapshot({ ...pending, verification_url: 'https://example.test/' })).toThrow()
    expect(() => adapter.decodeSnapshot({ ...pending, user_code: '\ud800' })).toThrow()
    expect(() => adapter.decodeSnapshot({ ...signedOut, status: 'error', error_code: '', error_message: '' })).toThrow()
    expect(() => adapter.decodeSnapshot({ ...signedOut, status: 'error', error_code: 'provider_diagnostic', error_message: 'safe message' })).toThrow()
  })

  it('projects snapshot/change lifecycle without accepting command results', () => {
    const replica = new LocalReplica()
    const store = new CodexLoginStore(replica)
    const resource = { type: 'codex_login' as const, id: 'codex' }
    expect(store.getSnapshot('codex').status).toBe('loading')
    replica.applySnapshot(resource, new CodexLoginAdapter('codex'), signedOut, { streamEpoch: 'epoch', sequence: '0' as Sequence, resourceRevision: '0', generation: 1 })
    expect(store.getSnapshot('codex').login?.status).toBe('signed_out')
    replica.applyChange(resource, new CodexLoginAdapter('codex'), [{ op: 'replace', key: 'codex', value: pending }], { streamEpoch: 'epoch', sequence: '1' as Sequence, resourceRevision: '1', generation: 1 })
    expect(store.getSnapshot('codex').login?.userCode).toBe('ABCD-1234')
    store.dispose()
  })
})
