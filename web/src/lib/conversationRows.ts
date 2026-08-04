import type { ActiveRun, RunStep, SessionItem } from '../types'
import { itemText, processKey } from './session'

/** A turn error is kept separate from ActiveRun so a failed turn can remain
 * actionable while the durable session is being refreshed. */
export interface ConversationRowTurnError {
  turnID: string
  message: string
}

interface ConversationRowBase {
  /** Stable for the lifetime of this row in this session. Never an array index. */
  key: string
}

export type ConversationRow =
  | (ConversationRowBase & { kind: 'message'; item: SessionItem })
  | (ConversationRowBase & { kind: 'compaction'; item: SessionItem })
  | (ConversationRowBase & {
    kind: 'process'
    createdAt: string
    lastSeq: number
    steps: RunStep[]
  })
  | (ConversationRowBase & {
    kind: 'active-process'
    run: ActiveRun
    steps: RunStep[]
    isLast: boolean
  })
  | (ConversationRowBase & {
    kind: 'active-compaction'
    compaction: NonNullable<ActiveRun['compaction']>
  })
  | (ConversationRowBase & {
    kind: 'provider-retry'
    retry: NonNullable<ActiveRun['providerRetry']>
  })
  | (ConversationRowBase & { kind: 'manual-compaction' })
  | (ConversationRowBase & { kind: 'turn-error'; message: string })
  | (ConversationRowBase & { kind: 'refresh-error' })
  | (ConversationRowBase & { kind: 'interrupted' })
  | (ConversationRowBase & { kind: 'bottom-spacer' })

export interface BuildConversationRowsInput {
  sessionID: string
  /** The raw page items from the shared durable session projection. */
  items: SessionItem[]
  activeRun?: ActiveRun | null
  recentStepsByTurn?: Record<string, RunStep[]>
  compacting?: boolean
  turnError?: ConversationRowTurnError | null
  sessionStatus?: string
}

/**
 * Builds the complete render stream for a conversation without making any
 * assumptions about its eventual list implementation. Durable rows are
 * identified by SessionItem ids/sequences; transient rows are identified by
 * run ids and RunStep ids. Consequently prepending a turn-aligned page does
 * not renumber any existing row and stream deltas update a row in place.
 */
export function buildConversationRows(input: BuildConversationRowsInput): ConversationRow[] {
  const activeRun = input.activeRun ?? null
  const rows = buildHistoricalRows(input.items, input.sessionID, input.recentStepsByTurn ?? {})

  if (activeRun) {
    if (activeRun.compaction) {
      rows.push({
        kind: 'active-compaction',
        key: rowKey(input.sessionID, 'active-compaction', activeRun.id),
        compaction: activeRun.compaction,
      })
    }
    activeRunSegments(activeRun).forEach((segment, index, segments) => {
      rows.push({
        kind: 'active-process',
        key: rowKey(input.sessionID, 'active-process', activeRun.id, segment.boundary),
        run: activeRun,
        steps: segment.steps,
        isLast: index === segments.length - 1,
      })
    })
    if (activeRun.providerRetry) {
      rows.push({
        kind: 'provider-retry',
        // The attempt is content, not identity. Keeping this key stable lets
        // the status row update while its countdown component resets itself.
        key: rowKey(input.sessionID, 'provider-retry', activeRun.id),
        retry: activeRun.providerRetry,
      })
    }
  }

  if (input.compacting) {
    rows.push({ kind: 'manual-compaction', key: rowKey(input.sessionID, 'manual-compaction') })
  }
  if (input.turnError) {
    rows.push({
      kind: 'turn-error',
      key: rowKey(input.sessionID, 'turn-error', input.turnError.turnID || 'unknown'),
      message: input.turnError.message,
    })
  }
  if (activeRun?.status === 'error_pending_refresh') {
    rows.push({
      kind: 'refresh-error',
      key: rowKey(input.sessionID, 'refresh-error', activeRun.id),
    })
  }
  if (!input.turnError && input.sessionStatus === 'interrupted' && !activeRun) {
    rows.push({ kind: 'interrupted', key: rowKey(input.sessionID, 'interrupted') })
  }
  return rows
}

type ActiveRunSegment = { steps: RunStep[]; boundary: string }

/**
 * Splits transient process output at server-reported prompt boundaries
 * without constructing a local user item. The boundary's step index is the
 * position observed when the prompt was drained, so later assistant/tool
 * steps remain in the following process segment.
 */
function activeRunSegments(run: ActiveRun): ActiveRunSegment[] {
  const boundaries = [...(run.processBoundaries ?? [])]
    .filter((boundary) => Number.isInteger(boundary.stepIndex) && boundary.stepIndex >= 0)
    .sort((left, right) => left.stepIndex - right.stepIndex)
  const segments: ActiveRunSegment[] = []
  let start = 0
  let boundary = 'initial'
  for (const marker of boundaries) {
    const end = Math.min(run.steps.length, Math.max(start, marker.stepIndex))
    segments.push({ steps: run.steps.slice(start, end), boundary })
    start = end
    boundary = `after-boundary:${marker.id}`
  }
  segments.push({ steps: run.steps.slice(start), boundary })
  // A boundary is an ordering marker, not a blank conversation row. Keep a
  // single empty segment only while the run has no process steps at all, so
  // the live cursor/Generating label still has a container.
  const nonEmpty = segments.filter((segment) => segment.steps.length > 0)
  return nonEmpty.length > 0 ? nonEmpty : [segments[segments.length - 1] ?? { steps: [], boundary: 'initial' }]
}

/** Builds only the durable part of the stream. Exported for consumers that
 * need to inspect the historical model without transient run state. */
export function buildHistoricalRows(items: SessionItem[], sessionID: string, recentStepsByTurn: Record<string, RunStep[]> = {}): ConversationRow[] {
  const rows: ConversationRow[] = []
  let steps: RunStep[] = []
  let processCreatedAt = ''
  let processTurnID = ''
  let processLastSeq = 0
  let processBoundary = ''
  let flowBoundary = ''
  let agentIteration = 0
  const emittedRecentTurns = new Set<string>()

  const flushProcess = (turnID = processTurnID) => {
    const recentKey = turnID ? processKey(sessionID, turnID) : ''
    const recentSteps = recentKey && !emittedRecentTurns.has(recentKey) ? recentStepsByTurn[recentKey] : undefined
    const displayedSteps = recentSteps?.length ? recentSteps : steps
    if (displayedSteps.length > 0) {
      const boundary = processBoundary || flowBoundary || `turn:${turnID || 'unknown'}`
      rows.push({
        kind: 'process',
        key: rowKey(sessionID, 'process', turnID || 'unknown', boundary),
        createdAt: processCreatedAt,
        lastSeq: processLastSeq,
        steps: displayedSteps,
      })
    }
    if (recentKey && recentSteps?.length) emittedRecentTurns.add(recentKey)
    steps = []
    processCreatedAt = ''
    processTurnID = ''
    processLastSeq = 0
    processBoundary = ''
  }

  const itemBoundary = (item: SessionItem): string => `item:${item.id}:${item.seq}`
  const ensureProcess = (item: SessionItem) => {
    if (!processCreatedAt) processCreatedAt = item.created_at
    if (!processTurnID) processTurnID = item.turn_id || ''
    if (!processBoundary) processBoundary = flowBoundary || itemBoundary(item)
  }

  for (const item of items) {
    const role = item.message?.role
    const text = itemText(item)

    if (item.kind === 'compaction') {
      flushProcess(item.turn_id || processTurnID)
      flowBoundary = itemBoundary(item)
      rows.push({ kind: 'compaction', key: rowKey(sessionID, 'compaction', item.id, item.seq), item })
      continue
    }

    if (role === 'user') {
      const itemTurnID = item.turn_id || ''
      // A user item sharing the process turn id is a mid-turn appended
      // message. It closes the preceding process segment without changing
      // the identity of that segment.
      if (processTurnID && itemTurnID && itemTurnID === processTurnID) {
        flushProcess(processTurnID)
        processTurnID = itemTurnID
        flowBoundary = itemBoundary(item)
        rows.push({ kind: 'message', key: rowKey(sessionID, 'message', item.id, item.seq), item })
        continue
      }
      flushProcess(processTurnID)
      processTurnID = itemTurnID
      flowBoundary = itemBoundary(item)
      agentIteration = 0
      rows.push({ kind: 'message', key: rowKey(sessionID, 'message', item.id, item.seq), item })
      continue
    }

    if (role === 'assistant' && (item.message?.tool_calls?.length ?? 0) > 0) {
      // If a malformed page crosses turns inside a process, finish the old
      // row rather than allowing two turns to share one key.
      if (processTurnID && item.turn_id && processTurnID !== item.turn_id) flushProcess(processTurnID)
      agentIteration = item.agent_iteration || agentIteration + 1
      ensureProcess(item)
      processLastSeq = item.seq
      if (text) steps.push({ kind: 'output', id: `${item.id}-output`, text, iteration: agentIteration })
      for (const toolCall of item.message?.tool_calls ?? []) {
        steps.push({
          kind: 'tool',
          id: toolCall.id,
          name: toolCall.name,
          iteration: agentIteration,
          arguments: toolCall.arguments,
          status: 'requested',
        })
      }
      continue
    }

    if (role === 'tool') {
      if (processTurnID && item.turn_id && processTurnID !== item.turn_id) flushProcess(processTurnID)
      agentIteration = item.agent_iteration || agentIteration || 1
      ensureProcess(item)
      processLastSeq = item.seq
      const toolCallID = item.message?.tool_call_id || item.id
      const index = steps.findIndex((step) => step.kind === 'tool' && step.id === toolCallID)
      const status: Extract<RunStep, { kind: 'tool' }>['status'] = item.message?.is_error || item.status === 'error'
        ? 'error'
        : item.status === 'pending' ? 'requested' : 'finished'
      if (index >= 0) {
        const tool = steps[index] as Extract<RunStep, { kind: 'tool' }>
        steps[index] = { ...tool, result: text, status }
      } else {
        steps.push({ kind: 'tool', id: toolCallID, name: 'tool', iteration: agentIteration, result: text, status })
      }
      continue
    }

    if (!processCreatedAt) processCreatedAt = item.created_at
    flushProcess(item.turn_id || processTurnID)
    flowBoundary = itemBoundary(item)
    if (text) rows.push({ kind: 'message', key: rowKey(sessionID, 'message', item.id, item.seq), item })
  }
  flushProcess(processTurnID)
  return rows
}

export function conversationRowItemKey(_index: number, row: ConversationRow): string {
  return row.key
}

/** Returns the number of rows that were inserted before the previous first row. */
export function prependedConversationRowCount(previousRows: ConversationRow[], nextRows: ConversationRow[]): number {
  const previousFirstKey = previousRows[0]?.key
  if (!previousFirstKey) return 0
  const previousFirstIndex = nextRows.findIndex((row) => row.key === previousFirstKey)
  return previousFirstIndex > 0 ? previousFirstIndex : 0
}

/**
 * Computes the Virtuoso logical origin for a session. The large initial value
 * leaves room for many explicit history prepends while staying positive, as
 * required by Virtuoso's firstItemIndex contract.
 */
export function getConversationFirstItemIndex(previousRows: ConversationRow[], nextRows: ConversationRow[], previousFirstItemIndex = 1_000_000): number {
  return previousFirstItemIndex - prependedConversationRowCount(previousRows, nextRows)
}

/** Public mostly for tests and future list implementations. */
export function conversationRowKey(sessionID: string, kind: string, ...parts: (string | number)[]): string {
  return JSON.stringify([sessionID, kind, ...parts])
}

function rowKey(sessionID: string, kind: string, ...parts: (string | number)[]): string {
  return conversationRowKey(sessionID, kind, ...parts)
}
