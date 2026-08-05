export type SyncReadErrorCode =
  | 'aborted'
  | 'transport'
  | 'protocol'
  | 'server'
  | 'invalid_snapshot'
  | 'invalid_change'
  | 'sequence_gap'
  | 'stream_epoch_mismatch'
  | 'resync_required'
  | 'blob_auth'
  | 'blob_size'
  | 'blob_type'
  | 'blob_etag'
  | 'blob_hash'
  | 'blob_expired'
  | 'blob_invalid'
  | 'runtime_stopped'

/**
 * Errors crossing the sync boundary are deliberately typed and metadata-only.
 * In particular, callers must not stringify an arbitrary fetch/WebSocket
 * error because its message may contain a credential-bearing URL.
 */
export class SyncReadError extends Error {
  readonly code: SyncReadErrorCode
  readonly resourceKey?: string

  constructor(code: SyncReadErrorCode, message: string, resourceKey?: string) {
    super(message)
    this.name = 'SyncReadError'
    this.code = code
    this.resourceKey = resourceKey
  }
}

export function asSyncReadError(
  reason: unknown,
  fallbackCode: SyncReadErrorCode,
  resourceKey?: string,
): SyncReadError {
  if (reason instanceof SyncReadError) {
    return resourceKey && !reason.resourceKey
      ? new SyncReadError(reason.code, reason.message, resourceKey)
      : reason
  }
  return new SyncReadError(fallbackCode, 'synchronization failed', resourceKey)
}
