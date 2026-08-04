import { parseDecimalRevision } from './sessionStore'

/**
 * The committed session watermark carried by a settlement event.  New
 * servers send committed_revision; last_seq is retained for old servers.
 * Returning null is intentional: callers must use the conservative snapshot
 * path when neither value is usable.
 */
export function settlementRevision(event: unknown): string | undefined {
  if (!event || typeof event !== 'object') return undefined
  const payload = event as { committed_revision?: unknown; last_seq?: unknown }
  if (payload.committed_revision !== undefined) {
    // Do not silently downgrade a present-but-malformed preferred watermark
    // to a potentially lossy legacy field.
    return parseDecimalRevision(payload.committed_revision) ?? undefined
  }
  return parseDecimalRevision(payload.last_seq) ?? undefined
}
