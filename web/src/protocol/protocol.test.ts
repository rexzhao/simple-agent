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
