import { isRFC3339Timestamp } from '../protocol/datetime'
import { isCanonicalWireIdentifier, isWellFormedString } from '../protocol/identifiers'
import { isDecimalString } from '../protocol/sequence'
import type { ChangeOperation, JsonObject, JsonValue } from '../protocol/types'
import type { ResourceAdapter } from './localReplica'

export interface ProjectSummary {
  readonly id: string
  readonly root: string
  readonly display_name: string
  readonly archived: boolean
  readonly created_at: string
  readonly updated_at: string
}

export interface ProjectIndexData {
  readonly summariesByID: Readonly<Record<string, ProjectSummary>>
  readonly orderedIDs: readonly string[]
}

const summaryFields = ['id', 'root', 'display_name', 'archived', 'created_at', 'updated_at'] as const
const snapshotFields = ['projects'] as const

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

function requiredString(value: unknown, field: string, allowEmpty = false): string {
  if (typeof value !== 'string' || (!allowEmpty && value.trim() === '') || !isWellFormedString(value)) throw new Error(`${field} must be a valid string`)
  return value
}

function projectID(value: unknown, field: string): string {
  const id = requiredString(value, field)
  if (!isCanonicalWireIdentifier(id) || !/^[A-Za-z0-9._-]+$/.test(id) || id === '.' || id === '..') throw new Error(`${field} must be a canonical project id`)
  return id
}

function timestamp(value: unknown, field: string): string {
  const result = requiredString(value, field)
  if (!isRFC3339Timestamp(result) || !Number.isFinite(Date.parse(result))) throw new Error(`${field} must be an RFC3339 timestamp`)
  return result
}

function summaryFromWire(value: unknown): ProjectSummary {
  const source = record(value, 'project summary must be an object')
  exactKeys(source, summaryFields, 'project summary')
  return {
    id: projectID(source.id, 'id'),
    root: requiredString(source.root, 'root'),
    display_name: requiredString(source.display_name, 'display_name', true),
    archived: typeof source.archived === 'boolean' ? source.archived : (() => { throw new Error('archived must be a boolean') })(),
    created_at: timestamp(source.created_at, 'created_at'),
    updated_at: timestamp(source.updated_at, 'updated_at'),
  }
}

function equalSummary(left: ProjectSummary, right: ProjectSummary): boolean {
  return summaryFields.every((field) => left[field] === right[field])
}

function orderIDs(summariesByID: Readonly<Record<string, ProjectSummary>>): string[] {
  return Object.values(summariesByID)
    .sort((left, right) => {
      const leftTime = Date.parse(left.created_at)
      const rightTime = Date.parse(right.created_at)
      if (leftTime !== rightTime) return leftTime - rightTime
      return left.id < right.id ? -1 : left.id > right.id ? 1 : 0
    })
    .map((summary) => summary.id)
}

function sameIDs(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((id, index) => id === right[index])
}

function makeData(summariesByID: Record<string, ProjectSummary>, previous?: ProjectIndexData): ProjectIndexData {
  const orderedIDs = orderIDs(summariesByID)
  if (previous && sameIDs(previous.orderedIDs, orderedIDs)) {
    const sameEntities = orderedIDs.every((id) => previous.summariesByID[id] === summariesByID[id])
    if (sameEntities) return previous
  }
  return { summariesByID, orderedIDs }
}

export class ProjectIndexAdapter implements ResourceAdapter<ProjectIndexData> {
  readonly resourceType = 'project_index' as const

  constructor(readonly resourceID = 'server') {
    if (resourceID !== 'server') throw new Error('project index resource id must be server')
  }

  validateResourceRevision(revision: string): void {
    if (!isDecimalString(revision)) throw new Error('resource_revision must be a decimal string')
  }

  decodeSnapshot(value: unknown, previous: ProjectIndexData | undefined): ProjectIndexData {
    const source = record(value, 'project index snapshot must be an object')
    exactKeys(source, snapshotFields, 'project index snapshot')
    if (!Array.isArray(source.projects)) throw new Error('projects must be an array')
    const summariesByID: Record<string, ProjectSummary> = Object.create(null) as Record<string, ProjectSummary>
    for (const raw of source.projects) {
      const summary = summaryFromWire(raw)
      if (own(summariesByID, summary.id)) throw new Error('project snapshot contains duplicate id')
      const old = previous && own(previous.summariesByID, summary.id) ? previous.summariesByID[summary.id] : undefined
      summariesByID[summary.id] = old && equalSummary(old, summary) ? old : summary
    }
    return makeData(summariesByID, previous)
  }

  applyChange(previous: ProjectIndexData, operations: readonly ChangeOperation[]): ProjectIndexData {
    if (!Array.isArray(operations) || operations.length === 0) throw new Error('project index change must contain operations')
    const summariesByID: Record<string, ProjectSummary> = Object.assign(
      Object.create(null) as Record<string, ProjectSummary>, previous.summariesByID,
    )
    for (const rawOperation of operations) {
      const operation = record(rawOperation, 'project index operation must be an object')
      const op = requiredString(operation.op, 'operation op')
      if (op === 'upsert') {
        exactKeys(operation, ['op', 'key', 'value'], 'upsert operation')
        const key = projectID(operation.key, 'upsert key')
        const summary = summaryFromWire(operation.value)
        if (summary.id !== key) throw new Error('upsert key does not match project id')
        const old = own(summariesByID, key) ? summariesByID[key] : undefined
        summariesByID[key] = old && equalSummary(old, summary) ? old : summary
      } else if (op === 'remove') {
        exactKeys(operation, ['op', 'key'], 'remove operation')
        delete summariesByID[projectID(operation.key, 'remove key')]
      } else {
        throw new Error('project index operation is not supported')
      }
    }
    return makeData(summariesByID, previous)
  }
}

export function projectIndexSnapshotValue(data: ProjectIndexData): JsonObject {
  return { projects: data.orderedIDs.map((id) => data.summariesByID[id]) as unknown as JsonValue[] }
}
