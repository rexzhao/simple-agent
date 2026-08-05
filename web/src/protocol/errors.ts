export type ProtocolErrorCode =
  | 'invalid_json'
  | 'invalid_message'
  | 'invalid_field'
  | 'unsupported_version'
  | 'unknown_type'

export class ProtocolDecodeError extends Error {
  readonly code: ProtocolErrorCode
  readonly field?: string

  constructor(code: ProtocolErrorCode, message: string, field?: string) {
    super(message)
    this.name = 'ProtocolDecodeError'
    this.code = code
    this.field = field
  }
}
