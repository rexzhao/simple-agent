import type { ActiveRun, ItemsPage, MessageContent, ModelPricing, Session, SessionItem, SessionToolCall } from '../types'
import type {
  SessionContentHistoryWindow,
  SessionContentItem,
  SessionContentMessage,
  SessionContentMetadata,
  SessionContentText,
  SessionRunState,
  SessionView,
} from '../domain/sessionContent'

/** Presentation mapping kept outside the repository and transport layers. */
export function sessionMetadataForConversation(metadata: SessionContentMetadata, history: SessionContentHistoryWindow): Session {
  const newest = history.descriptor.newest_item_seq ? Number(history.descriptor.newest_item_seq) : 0
  const context = metadata.context as unknown as Session['context']
  return {
    id: metadata.id,
    created_at: metadata.created_at,
    updated_at: metadata.updated_at,
    display_name: metadata.display_name ?? '',
    created_by: metadata.created_by ?? 'user',
    parent_session_id: metadata.parent_session_id,
    root_session_id: metadata.root_session_id ?? metadata.id,
    spawn_depth: metadata.spawn_depth ?? 0,
    archived: metadata.archived,
    last_used_at: metadata.last_used_at,
    current_run_id: metadata.current_run_id,
    running_run_id: metadata.running_run_id,
    running_turn_id: metadata.running_turn_id,
    interrupted_run_id: metadata.interrupted_run_id,
    interrupted_turn_id: metadata.interrupted_turn_id,
    latest_run_id: metadata.latest_run_id,
    last_run_id: metadata.last_run_id,
    last_run_status: metadata.last_run_status,
    // Keep optional authority fields absent.  In particular, /new must be
    // able to distinguish "server default" from an explicit empty command
    // argument; presentation must not manufacture empty provider/model
    // values merely to satisfy the legacy Session shape.
    ...(metadata.provider !== undefined ? { provider: metadata.provider } : {}),
    ...(metadata.model_profile !== undefined ? { model_profile: metadata.model_profile } : {}),
    ...(metadata.model_id !== undefined ? { model_id: metadata.model_id } : {}),
    pricing: metadata.pricing as ModelPricing | undefined,
    reasoning_level: metadata.reasoning_level,
    project_id: metadata.project_id ?? '',
    ...(metadata.created_cwd !== undefined || metadata.cwd !== undefined
      ? { created_cwd: metadata.created_cwd ?? metadata.cwd }
      : {}),
    last_seq: Number.isSafeInteger(newest) ? newest : 0,
    status: metadata.status,
    show_reasoning: metadata.show_reasoning,
    full_access: metadata.full_access,
    debug: metadata.debug,
    cwd: metadata.cwd,
    config_path: metadata.config_path,
    context,
  }
}

export function itemsPageForConversation(history: SessionContentHistoryWindow): ItemsPage {
  return {
    items: history.items.map(itemForConversation),
    oldest_seq: safeSeq(history.descriptor.oldest_item_seq),
    newest_seq: safeSeq(history.descriptor.newest_item_seq),
    has_more_before: history.descriptor.has_more_before,
    has_more_after: history.descriptor.has_more_after,
  }
}

export function activeRunForConversation(view: SessionView, sessionID: string): ActiveRun | null {
  if (view.runState) {
    // A committed/failed terminal overlay is a settlement barrier, not an
    // active composer target. Durable history and Session Index expose the
    // terminal result; only the running overlay belongs in the live row.
    if (view.runState.status !== 'running') return null
    return runStateForConversation(view.runState, sessionID)
  }
  const active = view.activeRun
  if (!active) return null
  return {
    id: active.run_id,
    sessionID,
    turnID: active.turn_id,
    assistantText: '',
    steps: [],
    agentIteration: 0,
    status: active.recovery_required ? 'error_pending_refresh' : 'running',
  }
}

function runStateForConversation(state: SessionRunState, sessionID: string): ActiveRun {
  const assistantItems: Record<string, { itemID: string; durableTextLength: number }> = {}
  const assistantItemBindings: Record<string, { turnID: string; agentIteration: number; itemID: string; durableTextLength: number }> = {}
  const assistantTails: Record<string, { turnID: string; agentIteration: number; itemID: string; text: string; durableTextLength: number }> = {}
  const textEntries = Object.values(state.text)
  for (const entry of textEntries) {
    const key = `${entry.key.turn_id}:${entry.key.agent_iteration}`
    assistantItems[key] = { itemID: entry.key.item_id, durableTextLength: entry.baseLength }
    assistantItemBindings[assistantItemKey(entry.key.turn_id, entry.key.agent_iteration, entry.key.item_id)] = {
      turnID: entry.key.turn_id,
      agentIteration: entry.key.agent_iteration,
      itemID: entry.key.item_id,
      durableTextLength: entry.baseLength,
    }
    assistantTails[assistantItemKey(entry.key.turn_id, entry.key.agent_iteration, entry.key.item_id)] = {
      turnID: entry.key.turn_id,
      agentIteration: entry.key.agent_iteration,
      itemID: entry.key.item_id,
      text: entry.text,
      durableTextLength: entry.baseLength,
    }
  }
  // Text tails remain owned by the durable/active message rows for rendering.
  // Process identities are ordered by their first event; lifecycle updates
  // only replace the existing keyed entry and never move or duplicate it.
  const steps = [] as ActiveRun['steps']
  for (const ref of state.stepOrder) {
    if (ref.kind === 'reasoning') {
      const entry = state.reasoning[ref.key]
      if (entry) {
        const reasoningTiming = state.reasoningTimings?.[JSON.stringify([entry.key.turn_id, entry.key.agent_iteration, entry.key.item_id])]
        steps.push({
          kind: 'reasoning',
          id: `${state.runID}:reasoning:${entry.key.item_id}:${entry.key.agent_iteration}`,
          text: entry.text,
          iteration: entry.key.agent_iteration,
          ...(reasoningTiming ? { reasoningTiming } : {}),
          turnID: entry.key.turn_id,
          itemID: entry.key.item_id,
        })
      }
      continue
    }
    const tool = state.tools[ref.key]
    if (tool) steps.push({
      kind: 'tool',
      id: tool.tool_call_id,
      name: tool.name,
      iteration: tool.agent_iteration,
      arguments: presentationToolArguments(tool.name, tool.arguments),
      result: tool.content,
      status: tool.is_error ? 'error' : tool.status,
    })
  }
  const status: ActiveRun['status'] = state.recoveryRequired || state.stale
    ? 'error_pending_refresh'
    : state.status === 'failed' ? 'failed'
      : state.status === 'cancelled' ? 'cancelled'
        : 'running'
  return {
    id: state.runID,
    sessionID,
    turnID: state.turnID,
    queuedPrompts: state.promptQueue.map((prompt) => ({ id: prompt.id, content: prompt.content, steer: prompt.steer })),
    // SessionContentRepository owns the merge for a durable item. Keeping an
    // unkeyed aggregate here would render that same tail a second time and
    // would attach iteration N's text to whichever item happens to be last.
    // Missing durable identities are rendered from assistantTails below.
    assistantText: '',
    assistantItems,
    assistantItemBindings,
    assistantTails,
    steps,
    agentIteration: Math.max(0, ...textEntries.map((entry) => entry.key.agent_iteration), ...Object.values(state.tools).map((tool) => tool.agent_iteration)),
    status,
    ...(state.settlement ? { settledRevision: state.settlement.resource_revision } : {}),
  }
}

function itemForConversation(item: SessionContentItem): SessionItem {
  return {
    seq: item.seq,
    id: item.key.item_id,
    turn_id: item.key.turn_id || undefined,
    agent_iteration: item.key.agent_iteration || undefined,
    created_at: item.created_at,
    kind: item.kind,
    visibility: item.visibility,
    audience: item.audience,
    status: item.status,
    message: item.message ? messageForConversation(item.message) : undefined,
  }
}

function messageForConversation(message: SessionContentMessage): NonNullable<SessionItem['message']> {
  return {
    role: message.role,
    content: message.content ? textForConversation(message.content) : undefined,
    reasoning: message.reasoning ? readableText(message.reasoning) : undefined,
    images: message.images ? message.images.map((image) => ({ ...image })) : undefined,
    tool_call_id: message.tool_call_id,
    tool_calls: message.tool_calls ? message.tool_calls.map(toolCallForConversation) : undefined,
    is_error: message.is_error,
  }
}

function toolCallForConversation(call: NonNullable<SessionContentMessage['tool_calls']>[number]): SessionToolCall {
  return {
    id: call.id,
    name: call.name,
    arguments: call.arguments ? presentationToolArguments(call.name, readableText(call.arguments)) : undefined,
  }
}

// web.eval source is intentionally retained in durable model history, but it
// must never be copied into presentation arguments. Keep this summary stable,
// useful for debugging, and limited to metadata about the request.
function presentationToolArguments(name: string, argumentsText: string): string {
  if (name !== 'web.eval') return argumentsText
  try {
    const value: unknown = JSON.parse(argumentsText)
    if (!value || typeof value !== 'object' || Array.isArray(value)) return '{"arguments":"redacted"}'
    const input = value as { code?: unknown; code_bytes?: unknown; timeout_ms?: unknown; arguments?: unknown }
    const summary: { timeout_ms?: number; code_bytes: number } = { code_bytes: 0 }
    if (typeof input.code === 'string' && input.code !== '') {
      summary.code_bytes = new TextEncoder().encode(input.code).byteLength
    } else if (typeof input.code_bytes === 'number' && Number.isSafeInteger(input.code_bytes) && input.code_bytes > 0 && input.code_bytes <= 65536) {
      summary.code_bytes = input.code_bytes
    } else if (input.arguments === 'redacted') {
      return '{"arguments":"redacted"}'
    } else {
      return '{"arguments":"redacted"}'
    }
    if (typeof input.code === 'string' && summary.code_bytes > 65536) return '{"arguments":"redacted"}'
    if (input.timeout_ms !== undefined && (typeof input.timeout_ms !== 'number' || !Number.isInteger(input.timeout_ms) || input.timeout_ms < 100 || input.timeout_ms > 30000)) return '{"arguments":"redacted"}'
    if (typeof input.timeout_ms === 'number') summary.timeout_ms = input.timeout_ms
    return JSON.stringify(summary)
  } catch {
    return '{"arguments":"redacted"}'
  }
}

function textForConversation(value: SessionContentText): MessageContent {
  return {
    ...(value.inline !== undefined ? { inline: value.inline } : {}),
    ...(value.preview !== undefined ? { preview: value.preview } : {}),
  }
}

function readableText(value: SessionContentText): string {
  return value.inline ?? value.preview ?? ''
}

function safeSeq(value: string | undefined): number {
  if (!value) return 0
  const number = Number(value)
  return Number.isSafeInteger(number) && number > 0 ? number : 0
}

function assistantItemKey(turnID: string, agentIteration: number, itemID: string): string {
  return JSON.stringify([turnID, agentIteration, itemID])
}
