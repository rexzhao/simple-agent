import { decodeMessage } from './decode'
import type { ProtocolMessage } from './types'

/**
 * Outbound V1 frames use the same strict schema boundary as inbound frames.
 * Validating the JSON after serialization prevents a transport caller from
 * accidentally creating a second, looser protocol representation.
 */
export function encodeMessage(message: ProtocolMessage): string {
  const encoded = JSON.stringify(message)
  if (typeof encoded !== 'string') throw new Error('message could not be serialized')
  decodeMessage(encoded)
  return encoded
}
