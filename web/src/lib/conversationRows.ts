import type { ActiveRun, RunStep, SessionItem } from '../types'
import { itemText, sessionItemIdentityKey } from './session'

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
  | (ConversationRowBase & {
    kind: 'message'
    item: SessionItem
    /** Uncheckpointed output attached to this item's authoritative id. */
    assistantTail?: string
    assistantStreaming?: boolean
  })
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
    /** The assistant tail is rendered by the durable message row instead. */
    assistantTailAttached?: boolean
  })
  | (ConversationRowBase & {
    /** Presentation-only live row for a run that has no process steps yet. */
    kind: 'active-cursor'
    run: ActiveRun
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
  compacting?: boolean
  turnError?: ConversationRowTurnError | null
  sessionStatus?: string
}

/**
 * Builds the complete render stream for a conversation without making any
 * assumptions about its eventual list implementation. Durable rows are
 * identified by their complete `(turn_id, agent_iteration, item_id)` identity;
 * transient rows are identified by run ids and RunStep ids. Consequently
 * prepending a turn-aligned page does not renumber any existing row and stream
 * deltas update a row in place.
 */
export function buildConversationRows(input: BuildConversationRowsInput): ConversationRow[] {
  const activeRun = input.activeRun ?? null
  const durableToolIDs = durableToolCallIDs(input.items)
  const durableToolResultIDs = durableToolResultIDsForItems(input.items)
  const liveTools = new Map(
    (activeRun?.steps ?? [])
      .filter((step): step is Extract<RunStep, { kind: 'tool' }> => step.kind === 'tool')
      .map((step) => [step.id, step]),
  )
  // A tool call can be durable before its result is. Fold its current live
  // status into the durable process group and then suppress the duplicate
  // active step. This retains requested/running/error progress without
  // creating two rows for one durable tool id.
  const historicalRows = buildHistoricalRows(input.items, input.sessionID).map((row) => {
    if (row.kind !== 'process') return row
    return {
      ...row,
      steps: row.steps.map((step) => {
        if (step.kind !== 'tool') return step
        const live = liveTools.get(step.id)
        return live && !durableToolResultIDs.has(step.id) ? { ...step, ...live } : step
      }),
    }
  })
  const assistantBinding = activeRun ? activeAssistantBinding(activeRun) : undefined
  // The binding is an explicit backend-provided item id. The page may not yet
  // contain that item during the append/snapshot race; only then do we retain
  // the process-row fallback. Never infer this relationship from text or turn
  // content, since identical output can be two distinct assistant messages.
  const attachedAssistantIdentity = assistantBinding && historicalRows.some((row) =>
    row.kind === 'message' && sessionItemIdentityKey(row.item) === assistantIdentityKey(assistantBinding) && row.item.message?.role === 'assistant',
  ) ? assistantIdentityKey(assistantBinding) : undefined
  // A final assistant projection can arrive while the stream still contains
  // the reasoning deltas that produced it. Build the suppression set from
  // complete durable identities only; incomplete legacy events remain
  // transient until the durable projection is rendered after settlement.
  const durableReasoning = activeRun ? durableReasoningIdentities(input.items, activeRun) : emptyDurableReasoningIdentities()
  const rows = historicalRows.map((row): ConversationRow => {
    if (row.kind !== 'message' || sessionItemIdentityKey(row.item) !== attachedAssistantIdentity) return row
    return {
      ...row,
      assistantTail: activeRun?.assistantText || undefined,
      assistantStreaming: activeRun?.status === 'running',
    }
  })

  if (activeRun) {
    if (activeRun.compaction) {
      rows.push({
        kind: 'active-compaction',
        key: rowKey(input.sessionID, 'active-compaction', activeRun.id),
        compaction: activeRun.compaction,
      })
    }
    // Tool calls/results are durable projection items. Once one is present in
    // the page, hide only that matching transient tool step; reasoning and
    // still-transient tools remain visible while the run is live.
    const rawSegments = activeRunSegments(activeRun)
    const filteredSegments = rawSegments.map((segment) => {
      return {
        ...segment,
        steps: segment.steps.filter((step) => {
          if (step.kind === 'tool' && durableToolIDs.has(step.id)) return false
          if (step.kind === 'reasoning' && shouldSuppressDurableReasoning(step, durableReasoning)) return false
          return true
        }),
      }
    }).filter((segment) => segment.steps.length > 0)
    const hasLiveSteps = filteredSegments.length > 0
    // Drop segments emptied by durable reconciliation. An attached durable
    // assistant message owns the cursor when it has the current item. A
    // running run with no attached item gets a presentation-only cursor row
    // when there are no process steps. A non-running run keeps that row only
    // when it has partial assistant text; otherwise it must not manufacture
    // an empty transient article.
    // Durable tools in an older turn must not suppress a fresh run's cursor.
    const keepEmptyPresentationRow = activeRun.status === 'running' || Boolean(activeRun.assistantText)
    const segments = hasLiveSteps
      ? filteredSegments
      : attachedAssistantIdentity
        ? []
        : keepEmptyPresentationRow
          ? [{ steps: [], boundary: rawSegments[rawSegments.length - 1]?.boundary ?? 'initial' }]
          : []
    const shouldRenderProcess = segments.length > 0
    if (shouldRenderProcess) {
      if (!hasLiveSteps) {
        // Keep the same key as the initial active-process segment so the
        // placeholder converges in place when the first real step arrives.
        rows.push({
          kind: 'active-cursor',
          key: rowKey(input.sessionID, 'active-process', activeRun.id, segments[0].boundary),
          run: activeRun,
        })
      } else {
        segments.forEach((segment, index) => {
          rows.push({
            kind: 'active-process',
            key: rowKey(input.sessionID, 'active-process', activeRun.id, segment.boundary),
            run: activeRun,
            steps: segment.steps,
            isLast: index === segments.length - 1,
            assistantTailAttached: Boolean(attachedAssistantIdentity),
          })
        })
      }
    }
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

function activeAssistantBinding(run: ActiveRun): { itemID: string; turnID: string; agentIteration: number; durableTextLength: number } | undefined {
  const turnID = run.turnID?.trim()
  if (!turnID || run.agentIteration <= 0) return undefined
  const invocationKey = `${turnID}:${run.agentIteration}`
  const indexed = run.assistantItems?.[invocationKey]
  if (!indexed) return undefined
  // assistantItems is only a compatibility/current-invocation index. Resolve
  // the actual entity through the complete identity binding when available.
  const binding = run.assistantItemBindings?.[assistantItemIdentityKey(turnID, run.agentIteration, indexed.itemID)]
  return binding ?? { ...indexed, turnID, agentIteration: run.agentIteration }
}

interface DurableReasoningIdentities {
  identities: Set<string>
}

function emptyDurableReasoningIdentities(): DurableReasoningIdentities {
  return { identities: new Set() }
}

function durableReasoningIdentities(items: SessionItem[], run: ActiveRun): DurableReasoningIdentities {
  const identities = emptyDurableReasoningIdentities()
  for (const item of items) {
    if (item.message?.role !== 'assistant' || !item.message.reasoning) continue
    const turnID = item.turn_id?.trim() ?? ''
    const iteration = item.agent_iteration ?? 0
    if (!turnID || !item.id.trim() || !Number.isInteger(iteration) || iteration <= 0) continue
    const binding = run.assistantItemBindings?.[assistantItemIdentityKey(turnID, iteration, item.id)]
      ?? run.assistantItems?.[`${turnID}:${iteration}`]
    // A durable reasoning item is authoritative for a transient step only
    // when the run's explicit turn/iteration binding points at this exact
    // item. A page item alone is not enough to identify a live step.
    if (binding?.itemID !== item.id) continue
    identities.identities.add(reasoningIdentityKey(turnID, iteration, item.id))
  }
  return identities
}

function shouldSuppressDurableReasoning(
  step: Extract<RunStep, { kind: 'reasoning' }>,
  durable: DurableReasoningIdentities,
): boolean {
  const itemID = step.itemID?.trim() ?? ''
  const turnID = step.turnID?.trim() ?? ''
  const iteration = step.iteration
  if (!turnID || !itemID || !Number.isInteger(iteration) || iteration <= 0) return false
  return durable.identities.has(reasoningIdentityKey(turnID, iteration, itemID))
}

function reasoningTurnIterationKey(turnID: string, iteration: number): string {
  return `${turnID}\u0000${iteration}`
}

function reasoningIdentityKey(turnID: string, iteration: number, itemID: string): string {
  return `${reasoningTurnIterationKey(turnID, iteration)}\u0000${itemID}`
}

function assistantIdentityKey(binding: { turnID: string; agentIteration: number; itemID: string }): string {
  return assistantItemIdentityKey(binding.turnID, binding.agentIteration, binding.itemID)
}

function assistantItemIdentityKey(turnID: string, agentIteration: number, itemID: string): string {
  return JSON.stringify([turnID, agentIteration, itemID])
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
  // single empty segment as an internal anchor for the presentation-only
  // cursor row while the run has no process steps at all.
  const nonEmpty = segments.filter((segment) => segment.steps.length > 0)
  return nonEmpty.length > 0 ? nonEmpty : [segments[segments.length - 1] ?? { steps: [], boundary: 'initial' }]
}

/** Builds only the durable part of the stream. Exported for consumers that
 * need to inspect the historical model without transient run state. */
export function buildHistoricalRows(items: SessionItem[], sessionID: string): ConversationRow[] {
  const rows: ConversationRow[] = []
  let steps: RunStep[] = []
  let processCreatedAt = ''
  let processTurnID = ''
  let processLastSeq = 0
  let processBoundary = ''
  let flowBoundary = ''
  let agentIteration = 0

  const flushProcess = (turnID = processTurnID) => {
    if (steps.length > 0) {
      const boundary = processBoundary || flowBoundary || `turn:${turnID || 'unknown'}`
      rows.push({
        kind: 'process',
        key: rowKey(sessionID, 'process', turnID || 'unknown', boundary),
        createdAt: processCreatedAt,
        lastSeq: processLastSeq,
        steps,
      })
    }
    steps = []
    processCreatedAt = ''
    processTurnID = ''
    processLastSeq = 0
    processBoundary = ''
  }

  const itemBoundary = (item: SessionItem): string => `item:${sessionItemIdentityKey(item)}`
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
      rows.push({ kind: 'compaction', key: rowKey(sessionID, 'compaction', sessionItemIdentityKey(item)), item })
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
        rows.push({ kind: 'message', key: rowKey(sessionID, 'message', sessionItemIdentityKey(item)), item })
        continue
      }
      flushProcess(processTurnID)
      processTurnID = itemTurnID
      flowBoundary = itemBoundary(item)
      agentIteration = 0
      rows.push({ kind: 'message', key: rowKey(sessionID, 'message', sessionItemIdentityKey(item)), item })
      continue
    }

    if (role === 'assistant' && (item.message?.tool_calls?.length ?? 0) > 0) {
      // If a malformed page crosses turns inside a process, finish the old
      // row rather than allowing two turns to share one key.
      if (processTurnID && item.turn_id && processTurnID !== item.turn_id) flushProcess(processTurnID)
      agentIteration = item.agent_iteration || agentIteration + 1
      ensureProcess(item)
      processLastSeq = item.seq
      if (item.message?.reasoning) {
        steps.push({ kind: 'reasoning', id: `${item.id}-reasoning`, text: item.message.reasoning, iteration: agentIteration })
      }
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

    if (role === 'assistant' && item.message?.reasoning) {
      if (processTurnID && item.turn_id && processTurnID !== item.turn_id) flushProcess(processTurnID)
      agentIteration = item.agent_iteration || agentIteration || 1
      ensureProcess(item)
      processLastSeq = item.seq
      steps.push({ kind: 'reasoning', id: `${item.id}-reasoning`, text: item.message.reasoning, iteration: agentIteration })
    }
    if (!processCreatedAt) processCreatedAt = item.created_at
    flushProcess(item.turn_id || processTurnID)
    flowBoundary = itemBoundary(item)
    if (text) rows.push({ kind: 'message', key: rowKey(sessionID, 'message', sessionItemIdentityKey(item)), item })
  }
  flushProcess(processTurnID)
  return rows
}

function durableToolCallIDs(items: SessionItem[]): Set<string> {
  const ids = new Set<string>()
  for (const item of items) {
    if (item.message?.role === 'assistant') {
      for (const call of item.message.tool_calls ?? []) if (call.id) ids.add(call.id)
    }
    if (item.message?.role === 'tool') {
      ids.add(item.message.tool_call_id || item.id)
      ids.add(item.id)
    }
  }
  return ids
}

function durableToolResultIDsForItems(items: SessionItem[]): Set<string> {
  const ids = new Set<string>()
  for (const item of items) {
    if (item.message?.role !== 'tool') continue
    ids.add(item.message.tool_call_id || item.id)
    ids.add(item.id)
  }
  return ids
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
