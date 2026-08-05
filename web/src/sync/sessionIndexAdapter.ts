import { isRFC3339Timestamp } from '../protocol/datetime'
import { isDecimalString } from '../protocol/sequence'
import type { ChangeOperation, JsonObject, JsonValue } from '../protocol/types'
import type { ResourceAdapter } from './localReplica'

export type { ResourceAdapter } from './localReplica'

export type SessionIndexStatus = 'idle' | 'queued' | 'running' | 'completed' | 'failed' | 'interrupted'

export interface SessionSummary {
  session_id: string
  project_id: string
  parent_session_id: string | null
  display_name: string
  archived: boolean
  status: SessionIndexStatus
  run_id: string | null
  resource_revision: string
  updated_at: string
  has_unread_result: boolean
}

export interface SessionIndexData {
  readonly summariesByID: Readonly<Record<string, SessionSummary>>
  readonly orderedIDs: readonly string[]
}

const summaryFields = [
  'session_id', 'project_id', 'parent_session_id', 'display_name', 'archived',
  'status', 'run_id', 'resource_revision', 'updated_at', 'has_unread_result',
] as const
const snapshotFields = ['sessions'] as const
const statuses = new Set<SessionIndexStatus>(['idle', 'queued', 'running', 'completed', 'failed', 'interrupted'])

function own(value: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key)
}

function record(value: unknown, message: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error(message)
  return value as Record<string, unknown>
}

function exactKeys(value: Record<string, unknown>, fields: readonly string[], message: string): void {
  const expected = new Set(fields)
  for (const key of Object.keys(value)) if (!expected.has(key)) throw new Error(`${message}: unknown field`)
  for (const key of fields) if (!own(value, key)) throw new Error(`${message}: missing field`)
}

function isWellFormedUTF16(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const code = value.charCodeAt(index)
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false
      index += 1
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return false
    }
  }
  return true
}

function requiredString(value: unknown, field: string, allowEmpty = false): string {
  if (typeof value !== 'string' || (!allowEmpty && value.trim() === '') || !isWellFormedUTF16(value)) throw new Error(`${field} must be a valid string`)
  return value
}

function nullableString(value: unknown, field: string): string | null {
  if (value === null) return null
  return requiredString(value, field)
}

function decimal(value: unknown, field: string): string {
  if (!isDecimalString(value)) throw new Error(`${field} must be a decimal string`)
  return value
}

function summaryFromWire(value: unknown, expectedProjectID: string): SessionSummary {
  const source = record(value, 'session summary must be an object')
  exactKeys(source, summaryFields, 'session summary')
  const sessionID = requiredString(source.session_id, 'session_id')
  const projectID = requiredString(source.project_id, 'project_id')
  if (projectID !== expectedProjectID) throw new Error('session summary project does not match resource')
  const parent = nullableString(source.parent_session_id, 'parent_session_id')
  const runID = nullableString(source.run_id, 'run_id')
  const status = requiredString(source.status, 'status') as SessionIndexStatus
  if (!statuses.has(status)) throw new Error('status is not a supported session status')
  if (status === 'idle' && runID !== null) throw new Error('idle session must not have a run_id')
  if (status !== 'idle' && (runID === null || runID.trim() === '')) throw new Error('active session must have a run_id')
  const updatedAt = requiredString(source.updated_at, 'updated_at')
  if (!isRFC3339Timestamp(updatedAt)) throw new Error('updated_at must be an RFC3339 timestamp')
  return {
    session_id: sessionID,
    project_id: projectID,
    parent_session_id: parent,
    display_name: requiredString(source.display_name, 'display_name', true),
    archived: typeof source.archived === 'boolean' ? source.archived : (() => { throw new Error('archived must be a boolean') })(),
    status,
    run_id: runID,
    resource_revision: decimal(source.resource_revision, 'resource_revision'),
    updated_at: updatedAt,
    has_unread_result: typeof source.has_unread_result === 'boolean' ? source.has_unread_result : (() => { throw new Error('has_unread_result must be a boolean') })(),
  }
}

function equalSummary(left: SessionSummary, right: SessionSummary): boolean {
  return summaryFields.every((field) => left[field] === right[field])
}

function orderIDs(summariesByID: Readonly<Record<string, SessionSummary>>): string[] {
  return Object.values(summariesByID)
    .sort((left, right) => left.display_name < right.display_name ? -1 : left.display_name > right.display_name ? 1 : left.session_id < right.session_id ? -1 : left.session_id > right.session_id ? 1 : 0)
    .map((summary) => summary.session_id)
}

function sameIDs(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((id, index) => id === right[index])
}

function makeData(summariesByID: Record<string, SessionSummary>, previous?: SessionIndexData): SessionIndexData {
  const orderedIDs = orderIDs(summariesByID)
  if (previous && sameIDs(previous.orderedIDs, orderedIDs)) {
    const sameEntities = orderedIDs.every((id) => previous.summariesByID[id] === summariesByID[id])
    if (sameEntities) return previous
  }
  return { summariesByID, orderedIDs }
}

export class SessionIndexAdapter implements ResourceAdapter<SessionIndexData> {
  readonly resourceType = 'session_index' as const

  constructor(readonly projectID: string) {
    if (requiredString(projectID, 'project id').trim() !== projectID) throw new Error('project id must be canonical')
  }

  validateResourceRevision(revision: string): void {
    decimal(revision, 'resource_revision')
  }

  decodeSnapshot(value: unknown, previous: SessionIndexData | undefined): SessionIndexData {
    const source = record(value, 'session index snapshot must be an object')
    exactKeys(source, snapshotFields, 'session index snapshot')
    if (!Array.isArray(source.sessions)) throw new Error('sessions must be an array')
    const summariesByID: Record<string, SessionSummary> = Object.create(null) as Record<string, SessionSummary>
    for (const value of source.sessions) {
      const summary = summaryFromWire(value, this.projectID)
      if (own(summariesByID, summary.session_id)) throw new Error('session snapshot contains duplicate session_id')
      const old = previous && own(previous.summariesByID, summary.session_id) ? previous.summariesByID[summary.session_id] : undefined
      summariesByID[summary.session_id] = old && equalSummary(old, summary) ? old : summary
    }
    return makeData(summariesByID, previous)
  }

  applyChange(previous: SessionIndexData, operations: readonly ChangeOperation[]): SessionIndexData {
    if (!Array.isArray(operations) || operations.length === 0) throw new Error('session index change must contain operations')
    const summariesByID: Record<string, SessionSummary> = Object.assign(
      Object.create(null) as Record<string, SessionSummary>,
      previous.summariesByID,
    )
    for (const rawOperation of operations) {
      const operation = record(rawOperation, 'session index operation must be an object')
      const op = requiredString(operation.op, 'operation op')
      if (op === 'upsert') {
        exactKeys(operation, ['op', 'key', 'value'], 'upsert operation')
        const key = requiredString(operation.key, 'upsert key')
        const summary = summaryFromWire(operation.value, this.projectID)
        if (summary.session_id !== key) throw new Error('upsert key does not match session_id')
        const old = own(summariesByID, key) ? summariesByID[key] : undefined
        summariesByID[key] = old && equalSummary(old, summary) ? old : summary
      } else if (op === 'remove') {
        exactKeys(operation, ['op', 'key'], 'remove operation')
        delete summariesByID[requiredString(operation.key, 'remove key')]
      } else {
        throw new Error('session index operation is not supported')
      }
    }
    return makeData(summariesByID, previous)
  }
}

export function sessionIndexSnapshotValue(data: SessionIndexData): JsonObject {
  return { sessions: data.orderedIDs.map((id) => data.summariesByID[id]) as unknown as JsonValue[] }
}
