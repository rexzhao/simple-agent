import type { SessionContentHistoryReadOptions, SessionContentHistoryWindow, SessionContentItem, SessionContentMessage, SessionContentText } from '../domain/sessionContent'
import type { SessionHistoryReadResult } from '../commands/sessionCommands'
import type { ItemsPage, SessionItem } from '../types'

/**
 * Converts the command's bounded legacy page DTO into the content domain.
 * Blob resolution has already happened in CommandFacade, so this module never
 * sees a descriptor or makes a data-plane request.
 */
export function sessionHistoryResultToDomain(
  result: SessionHistoryReadResult,
  options: SessionContentHistoryReadOptions = {},
): SessionContentHistoryWindow {
  if (!result.history) throw new Error('session history returned no page')
  return itemsPageToDomain(result.history, result, options)
}

export function itemsPageToDomain(
  page: ItemsPage,
  result: Pick<SessionHistoryReadResult, 'cursor' | 'direction' | 'limit' | 'align_turn'> = { cursor: 0, direction: '', limit: 50, align_turn: false },
  options: SessionContentHistoryReadOptions = {},
): SessionContentHistoryWindow {
  const items = page.items.map(itemToDomain)
  const oldest = items.length > 0 ? Math.min(...items.map((item) => item.seq)) : page.oldest_seq
  const newest = items.length > 0 ? Math.max(...items.map((item) => item.seq)) : page.newest_seq
  const direction = result.direction || options.direction || ''
  const cursor = result.cursor || options.cursor || 0
  return {
    items,
    descriptor: {
      limit: result.limit || options.limit || 50,
      ...(direction === 'before' && cursor > 0 ? { before_item_seq: String(cursor) } : {}),
      ...(direction === 'after' && cursor > 0 ? { after_item_seq: String(cursor) } : {}),
      align_turn: result.align_turn ?? options.alignTurn ?? false,
      visible_only: true,
      oldest_item_seq: oldest > 0 ? String(oldest) : undefined,
      newest_item_seq: newest > 0 ? String(newest) : undefined,
      has_more_before: page.has_more_before,
      has_more_after: page.has_more_after,
    },
  }
}

function itemToDomain(item: SessionItem): SessionContentItem {
  const message = item.message ? messageToDomain(item.message) : undefined
  return {
    key: {
      // Older command pages may omit these optional fields. Empty turn id and
      // zero iteration are deterministic compatibility identities; current
      // session_content snapshots always provide the complete identity.
      turn_id: item.turn_id ?? '',
      agent_iteration: item.agent_iteration ?? 0,
      item_id: item.id,
    },
    seq: item.seq,
    created_at: item.created_at,
    kind: item.kind as SessionContentItem['kind'],
    visibility: item.visibility as SessionContentItem['visibility'],
    audience: item.audience as SessionContentItem['audience'],
    ...(item.status ? { status: item.status as SessionContentItem['status'] } : {}),
    ...(message ? { message } : {}),
  }
}

function text(value: { inline?: string; preview?: string } | string | undefined): SessionContentText | undefined {
  if (value === undefined) return undefined
  if (typeof value === 'string') return { inline: value }
  return {
    ...(value.inline !== undefined ? { inline: value.inline } : {}),
    ...(value.preview !== undefined ? { preview: value.preview } : {}),
  }
}

function messageToDomain(message: NonNullable<SessionItem['message']>): SessionContentMessage {
  return {
    role: message.role as SessionContentMessage['role'],
    ...(message.content ? { content: text(message.content) } : {}),
    ...(message.reasoning !== undefined ? { reasoning: text(message.reasoning) } : {}),
    ...(message.images ? { images: message.images.map((image) => ({ ...image })) } : {}),
    ...(message.tool_call_id ? { tool_call_id: message.tool_call_id } : {}),
    ...(message.tool_calls ? {
      tool_calls: message.tool_calls.map((call) => ({
        id: call.id,
        name: call.name,
        ...(call.arguments !== undefined ? { arguments: text(call.arguments) } : {}),
      })),
    } : {}),
    ...(message.is_error !== undefined ? { is_error: message.is_error } : {}),
  }
}

export function historyCursor(view: SessionContentHistoryWindow): number | undefined {
  const cursor = view.descriptor.oldest_item_seq
  if (!cursor) return undefined
  const value = Number(cursor)
  return Number.isSafeInteger(value) && value > 0 ? value : undefined
}
