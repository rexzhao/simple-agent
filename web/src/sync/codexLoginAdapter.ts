import { isWellFormedString } from '../protocol/identifiers'
import { isDecimalString } from '../protocol/sequence'
import { isProviderName } from '../domain/providerIdentity'
import type { ChangeOperation, JsonObject, JsonValue } from '../protocol/types'
import type { ResourceAdapter } from './localReplica'
import type { CodexLoginDomain, CodexLoginStatus } from '../repositories/codexLogin'

export interface CodexLoginData extends CodexLoginDomain {}

const snapshotFields = ['provider', 'status', 'login_id', 'user_code', 'verification_url', 'refreshable', 'error_code', 'error_message'] as const
const operationFields = ['op', 'key', 'value'] as const
const statuses = new Set<CodexLoginStatus>(['signed_out', 'pending', 'signed_in', 'expired', 'error'])
const errorCodes = new Set(['login_failed', 'login_start_failed'])
const maxLoginIDBytes = 128
const maxUserCodeBytes = 128
const maxURLBytes = 2048

function record(value: unknown, message: string): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error(message)
  return value as Record<string, unknown>
}

function exactKeys(value: Record<string, unknown>, fields: readonly string[], message: string): void {
  const expected = new Set(fields)
  if (Object.keys(value).some((key) => !expected.has(key)) || fields.some((key) => !Object.prototype.hasOwnProperty.call(value, key))) throw new Error(message)
}

function text(value: unknown, field: string, maxBytes: number, allowEmpty = true): string {
  if (typeof value !== 'string' || !isWellFormedString(value) || (!allowEmpty && value === '') || new TextEncoder().encode(value).byteLength > maxBytes || [...value].some((character) => /[\u0000-\u001f\u007f-\u009f]/u.test(character))) throw new Error(`${field} is invalid`)
  return value
}

function verificationURL(value: unknown): string {
  const result = text(value, 'verification_url', maxURLBytes)
  if (result === '') return result
  let parsed: URL
  try { parsed = new URL(result) } catch { throw new Error('verification_url is invalid') }
  if (parsed.username || parsed.password || parsed.search || parsed.hash || !parsed.hostname || parsed.pathname === '/') throw new Error('verification_url is invalid')
  const host = parsed.hostname.toLowerCase()
  if (parsed.protocol !== 'https:' && !(parsed.protocol === 'http:' && ['localhost', '127.0.0.1', '::1', '[::1]'].includes(host))) throw new Error('verification_url is invalid')
  return result
}

function provider(value: unknown): string {
  if (!isProviderName(value)) throw new Error('provider is invalid')
  return value
}

function snapshot(value: unknown, resourceID: string): CodexLoginData {
  const source = record(value, 'Codex login snapshot is invalid')
  exactKeys(source, snapshotFields, 'Codex login snapshot shape is invalid')
  const resultProvider = provider(source.provider)
  if (resultProvider !== resourceID || typeof source.status !== 'string' || !statuses.has(source.status as CodexLoginStatus) || typeof source.refreshable !== 'boolean') throw new Error('Codex login snapshot identity is invalid')
  const result: CodexLoginData = {
    provider: resultProvider,
    status: source.status as CodexLoginStatus,
    loginID: text(source.login_id, 'login_id', maxLoginIDBytes),
    userCode: text(source.user_code, 'user_code', maxUserCodeBytes),
    verificationURL: verificationURL(source.verification_url),
    refreshable: source.refreshable,
    errorCode: text(source.error_code, 'error_code', 128),
    errorMessage: text(source.error_message, 'error_message', 256),
  }
  validateState(result)
  return result
}

function validateState(value: CodexLoginData): void {
  if (value.status !== 'pending' && (value.userCode !== '' || value.verificationURL !== '')) throw new Error('device fields require pending status')
  if (value.status !== 'error' && (value.errorCode !== '' || value.errorMessage !== '')) throw new Error('error fields require error status')
  if (value.status === 'error' && (!errorCodes.has(value.errorCode) || !value.errorMessage)) throw new Error('error fields are invalid')
  if (value.status !== 'signed_in' && value.status !== 'expired' && value.refreshable) throw new Error('refreshable status is invalid')
}

function same(left: CodexLoginData, right: CodexLoginData): boolean {
  return left.provider === right.provider && left.status === right.status && left.loginID === right.loginID && left.userCode === right.userCode && left.verificationURL === right.verificationURL && left.refreshable === right.refreshable && left.errorCode === right.errorCode && left.errorMessage === right.errorMessage
}

export class CodexLoginAdapter implements ResourceAdapter<CodexLoginData> {
  readonly resourceType = 'codex_login' as const

  constructor(readonly resourceID: string) {
    if (!isProviderName(resourceID)) throw new Error('Codex login resource id is invalid')
  }

  validateResourceRevision(revision: string): void {
    if (!isDecimalString(revision)) throw new Error('resource_revision must be a decimal string')
  }

  decodeSnapshot(value: unknown, previous?: CodexLoginData): CodexLoginData {
    const result = snapshot(value, this.resourceID)
    return previous && same(previous, result) ? previous : result
  }

  applyChange(previous: CodexLoginData, operations: readonly ChangeOperation[]): CodexLoginData {
    if (!Array.isArray(operations) || operations.length === 0) throw new Error('Codex login change is empty')
    let result = previous
    for (const raw of operations) {
      const operation = record(raw, 'Codex login operation is invalid')
      exactKeys(operation, operationFields, 'Codex login operation shape is invalid')
      if (operation.op !== 'replace' || operation.key !== this.resourceID) throw new Error('Codex login operation is unsupported')
      const next = snapshot(operation.value, this.resourceID)
      result = same(result, next) ? result : next
    }
    return result
  }
}

export function codexLoginSnapshotValue(value: CodexLoginData): JsonObject {
  return {
    provider: value.provider,
    status: value.status,
    login_id: value.loginID,
    user_code: value.userCode,
    verification_url: value.verificationURL,
    refreshable: value.refreshable,
    error_code: value.errorCode,
    error_message: value.errorMessage,
  } as unknown as JsonObject
}

export function codexLoginWireValue(value: CodexLoginData): JsonValue {
  return codexLoginSnapshotValue(value) as unknown as JsonValue
}
