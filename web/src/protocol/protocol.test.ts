import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { decodeMessage } from './decode'
import { ProtocolDecodeError } from './errors'
import type { MessageType } from './types'

interface Fixture {
  name: string
  message: Record<string, unknown>
}

interface FixtureFile {
  valid: Fixture[]
  invalid: Fixture[]
}

const fixtures = JSON.parse(
  readFileSync(new URL('../../../internal/protocol/testdata/fixtures.json', import.meta.url), 'utf8'),
) as FixtureFile

describe('shared protocol golden fixtures', () => {
  it.each(fixtures.valid)('decodes valid fixture $name', (fixture) => {
    const message = decodeMessage(JSON.stringify(fixture.message))
    expect(message.type).toBe(fixture.message.type as MessageType)
    expect(message.version).toBe(1)
  })

  it.each(fixtures.invalid)('rejects invalid fixture $name', (fixture) => {
    expect(() => decodeMessage(JSON.stringify(fixture.message))).toThrow(ProtocolDecodeError)
  })

  it('rejects shared debug fixtures for their malformed fields', () => {
    const missingFocused = fixtures.invalid.find((fixture) => fixture.name === 'debug_missing_focused')
    const invalidEpoch = fixtures.invalid.find((fixture) => fixture.name === 'debug_invalid_epoch')
    expect(missingFocused).toBeDefined()
    expect(invalidEpoch).toBeDefined()

    expect(() => decodeMessage(JSON.stringify(missingFocused?.message))).toThrow(
      expect.objectContaining({ code: 'invalid_field', field: 'payload.focused' }),
    )
    expect(() => decodeMessage(JSON.stringify(invalidEpoch?.message))).toThrow(
      expect.objectContaining({ code: 'invalid_field', field: 'payload.page_epoch' }),
    )
  })

  it.each([
    'debug_register',
    'debug_registered',
    'debug_focus',
    'debug_focused',
    'debug_unregister',
    'debug_unregistered',
  ] as MessageType[])('decodes the stage-1 debug control message %s', (type) => {
    const message = decodeMessage(JSON.stringify({
      version: 1,
      type,
      id: `message-${type}`,
      payload: {
        page_id: 'page 1',
        page_epoch: 'epoch-1',
        session_id: 'session-1',
        focused: false,
        future_optional_field: 'ignored',
      },
    }))
    expect(message.type).toBe(type)
  })

  it.each([
    ['missing focused', 'focused', undefined],
    ['null focused', 'focused', null],
    ['string focused', 'focused', 'true'],
    ['numeric focused', 'focused', 1],
    ['empty page id', 'page_id', ''],
    ['leading whitespace page epoch', 'page_epoch', ' epoch-1'],
    ['trailing whitespace session id', 'session_id', 'session-1\n'],
    ['control character page id', 'page_id', 'page\u0001'],
    ['oversized session id', 'session_id', 'x'.repeat(4097)],
  ] as [string, string, unknown][])('rejects malformed debug payload: %s', (_name, field, value) => {
    const payload: Record<string, unknown> = {
      page_id: 'page-1',
      page_epoch: 'epoch-1',
      session_id: 'session-1',
      focused: true,
    }
    if (value === undefined) delete payload[field]
    else payload[field] = value

    expect(() => decodeMessage(JSON.stringify({
      version: 1,
      type: 'debug_register',
      id: 'debug-malformed',
      payload,
    }))).toThrow(expect.objectContaining({ code: 'invalid_field', field: `payload.${field}` }))
  })

  it('enforces the execution channel status, timeout, and inline result bounds', () => {
    const execute = {
      version: 1,
      type: 'debug_execute',
      id: 'execute-1',
      payload: {
        execution_id: 'execution-1', page_id: 'page-1', page_epoch: 'epoch-1', session_id: 'session-1',
        code: '1 + 1', timeout_ms: 500,
      },
    }
    expect(decodeMessage(JSON.stringify(execute)).type).toBe('debug_execute')
    execute.payload.timeout_ms = 99
    expect(() => decodeMessage(JSON.stringify(execute))).toThrow(
      expect.objectContaining({ field: 'payload.timeout_ms' }),
    )

    const result = {
      version: 1,
      type: 'debug_execution_result',
      id: 'result-1',
      payload: {
        execution_id: 'execution-1', page_id: 'page-1', page_epoch: 'epoch-1', session_id: 'session-1',
        status: 'succeeded' as string, value: null as unknown,
      },
    }
    expect(decodeMessage(JSON.stringify(result)).type).toBe('debug_execution_result')
    result.payload.status = 'pending'
    expect(() => decodeMessage(JSON.stringify(result))).toThrow(
      expect.objectContaining({ field: 'payload.status' }),
    )
    result.payload.status = 'succeeded'
    result.payload.value = 'x'.repeat(64 * 1024)
    expect(() => decodeMessage(JSON.stringify(result))).toThrow(
      expect.objectContaining({ field: 'payload' }),
    )
  })

  it('ignores unknown optional fields', () => {
    const message = decodeMessage(JSON.stringify({
      version: 1,
      type: 'ping',
      id: 'ping_1',
      future_optional: { ignored: true },
      payload: { future_payload_field: 'ignored' },
    }))
    expect(message.type).toBe('ping')
  })

  it('decodes failed command results with a typed error payload', () => {
    const fixture = fixtures.valid.find((candidate) => candidate.name === 'command_result_failed')
    expect(fixture).toBeDefined()
    const message = decodeMessage(JSON.stringify(fixture?.message))
    if (message.type !== 'command_result') throw new Error('wrong command result type')
    expect(message.payload.status).toBe('failed')
    expect(message.payload.error).toMatchObject({ code: 'conflict' })
  })

  it('retains resource-specific operation and event fields at the raw boundary', () => {
    const changeFixture = fixtures.valid.find((fixture) => fixture.name === 'change')
    const eventFixture = fixtures.valid.find((fixture) => fixture.name === 'subscription_event')
    expect(changeFixture).toBeDefined()
    expect(eventFixture).toBeDefined()

    const change = decodeMessage(JSON.stringify(changeFixture?.message))
    const event = decodeMessage(JSON.stringify(eventFixture?.message))
    if (change.type !== 'change' || event.type !== 'subscription_event') {
      throw new Error('raw boundary fixtures decoded with the wrong type')
    }
    expect(change.payload.operations[0]).toMatchObject({
      op: 'metadata.replace',
      metadata: { display_name: 'Renamed' },
    })
    expect(change.payload.operations[1]).toMatchObject({
      op: 'remove',
      reason: 'archived',
    })
    expect(event.payload.event).toMatchObject({
      type: 'text.delta',
      session_id: 'session_2',
      run_id: 'run_9',
      run_cursor: '17',
      item_id: 'item_3',
      delta: '...',
    })
  })

  it('rejects malformed JSON with a stable error code', () => {
    expect(() => decodeMessage('{"version":1')).toThrowError(
      expect.objectContaining({ code: 'invalid_json' }),
    )
  })

  it('rejects a session-content event whose identity names another resource', () => {
    const fixture = fixtures.valid.find((candidate) => candidate.name === 'subscription_event')
    expect(fixture).toBeDefined()
    const message = structuredClone(fixture?.message) as Record<string, any>
    message.payload.event.session_id = 'different-session'
    expect(() => decodeMessage(JSON.stringify(message))).toThrow(ProtocolDecodeError)
  })

  it('decodes a bounded typed turn failure and rejects an oversized reason', () => {
    const fixture = fixtures.valid.find((candidate) => candidate.name === 'subscription_event')
    expect(fixture).toBeDefined()
    const message = structuredClone(fixture?.message) as Record<string, any>
    message.payload.event = {
      type: 'turn.failed',
      session_id: 'session_2',
      run_id: 'run_1',
      run_cursor: '3',
      turn_id: 'turn-1',
      code: 'model_http_error',
      message: '429: slow down and try again',
    }
    const decoded = decodeMessage(JSON.stringify(message))
    if (decoded.type !== 'subscription_event') throw new Error('wrong subscription event type')
    expect(decoded.payload.event).toMatchObject({ type: 'turn.failed', code: 'model_http_error' })

    message.payload.event.message = '🦄'.repeat(600)
    expect(() => decodeMessage(JSON.stringify(message))).not.toThrow()
    message.payload.event.message = 'x'.repeat(601)
    expect(() => decodeMessage(JSON.stringify(message))).toThrow(ProtocolDecodeError)
  })

  it('rejects an ambiguous settlement watermark', () => {
    const fixture = fixtures.valid.find((candidate) => candidate.name === 'subscription_event')
    expect(fixture).toBeDefined()
    const message = structuredClone(fixture?.message) as Record<string, any>
    message.payload.event = {
      type: 'run.settled',
      session_id: 'session_2',
      run_id: 'run_1',
      run_cursor: '3',
      status: 'committed',
      durable_settlement_watermark: {
        resource_revision: '4',
        run_cursor: '2',
        verified: false,
        covered_items: [{ turn_id: 'turn-1', agent_iteration: 1, item_id: 'item-1', run_cursor: '1' }],
      },
    }
    expect(() => decodeMessage(JSON.stringify(message))).toThrow(ProtocolDecodeError)
  })
})
