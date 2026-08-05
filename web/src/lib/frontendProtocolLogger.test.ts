// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  FRONTEND_PROTOCOL_LOG_CAPACITY,
  FrontendProtocolLogger,
  copyFrontendProtocolJSONL,
  downloadFrontendProtocolJSONL,
  frontendProtocolLogFilename,
  frontendProtocolLogger,
} from './frontendProtocolLogger'

describe('FrontendProtocolLogger', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
    frontendProtocolLogger.resetForTesting()
    vi.restoreAllMocks()
  })

  it('keeps per-session bounded JSONL records and clones payloads immediately', () => {
    const logger = new FrontendProtocolLogger()
    vi.spyOn(console, 'log').mockImplementation(() => {})
    logger.setEnabled('session-1', true)
    const payload = { nested: { text: 'before' }, list: ['one'] }
    logger.log({ sessionID: 'session-1', source: 'test', kind: 'event', data: payload })
    payload.nested.text = 'after'
    payload.list.push('changed')

    expect(logger.getSnapshot('session-1').records[0].data).toEqual({ nested: { text: 'before' }, list: ['one'] })
    const metadata = JSON.parse(logger.jsonl('session-1').split('\n')[0]) as Record<string, unknown>
    expect(metadata).toMatchObject({
      source: 'frontend_protocol_logger',
      kind: 'metadata',
      session_id: 'session-1',
      retained_count: 1,
      dropped_count: 0,
      capacity: FRONTEND_PROTOCOL_LOG_CAPACITY,
    })
    expect(metadata.log_seq).toBeUndefined()

    for (let index = 0; index < FRONTEND_PROTOCOL_LOG_CAPACITY; index++) {
      logger.log({ sessionID: 'session-1', source: 'test', kind: 'bounded', index })
    }
    const snapshot = logger.getSnapshot('session-1')
    expect(snapshot.records).toHaveLength(FRONTEND_PROTOCOL_LOG_CAPACITY)
    expect(snapshot.droppedCount).toBe(1)
    expect(snapshot.records[0].kind).toBe('bounded')
    expect(snapshot.records.at(-1)?.log_seq).toBe(FRONTEND_PROTOCOL_LOG_CAPACITY + 1)
    expect(JSON.parse(logger.jsonl('session-1').split('\n')[0])).toMatchObject({
      retained_count: FRONTEND_PROTOCOL_LOG_CAPACITY,
      dropped_count: 1,
    })
  })

  it('filters by enabled session and stops recording without losing retained records', () => {
    const logger = new FrontendProtocolLogger()
    vi.spyOn(console, 'log').mockImplementation(() => {})
    logger.setEnabled('session-1', true)
    logger.log({ sessionID: 'session-1', source: 'test', kind: 'one' })
    logger.log({ sessionID: 'session-2', source: 'test', kind: 'not-enabled' })
    logger.setEnabled('session-1', false)
    logger.log({ sessionID: 'session-1', source: 'test', kind: 'disabled' })

    expect(logger.getSnapshot('session-1').records.map((record) => record.kind)).toEqual(['one'])
    expect(logger.getSnapshot('session-2').records).toHaveLength(0)
    expect(logger.getSnapshot('session-1').enabled).toBe(false)
  })

  it('keeps an earlier snapshot immutable when a record is appended', () => {
    const logger = new FrontendProtocolLogger()
    vi.spyOn(console, 'log').mockImplementation(() => {})
    logger.setEnabled('session-1', true)
    const before = logger.getSnapshot('session-1')

    logger.log({ sessionID: 'session-1', source: 'test', kind: 'event' })

    expect(before.records).toHaveLength(0)
    expect(logger.getSnapshot('session-1').records).toHaveLength(1)
  })

  it('batches notifications per session while keeping toggle and clear immediate', () => {
    vi.useFakeTimers()
    const logger = new FrontendProtocolLogger()
    vi.spyOn(console, 'log').mockImplementation(() => {})
    logger.setEnabled('session-1', true)
    const sessionOneListener = vi.fn()
    const sessionTwoListener = vi.fn()
    const unsubscribeOne = logger.subscribe('session-1', sessionOneListener)
    const unsubscribeTwo = logger.subscribe('session-2', sessionTwoListener)

    logger.log({ sessionID: 'session-1', source: 'test', kind: 'delta-1' })
    logger.log({ sessionID: 'session-1', source: 'test', kind: 'delta-2' })
    expect(sessionOneListener).not.toHaveBeenCalled()
    expect(sessionTwoListener).not.toHaveBeenCalled()
    vi.advanceTimersByTime(99)
    expect(sessionOneListener).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    expect(sessionOneListener).toHaveBeenCalledTimes(1)

    logger.setEnabled('session-2', true)
    expect(sessionTwoListener).toHaveBeenCalledTimes(1)
    expect(sessionOneListener).toHaveBeenCalledTimes(1)
    logger.clear('session-1')
    expect(sessionOneListener).toHaveBeenCalledTimes(2)

    logger.log({ sessionID: 'session-1', source: 'test', kind: 'after-unsubscribe' })
    unsubscribeOne()
    unsubscribeTwo()
    vi.advanceTimersByTime(100)
    expect(sessionOneListener).toHaveBeenCalledTimes(2)
  })

  it('downloads a JSONL Blob with session filename and revokes its ObjectURL', () => {
    vi.useFakeTimers()
    const createObjectURL = vi.fn().mockReturnValue('blob:frontend-log')
    const revokeObjectURL = vi.fn()
    vi.stubGlobal('URL', { createObjectURL, revokeObjectURL })
    let clickedAnchor: HTMLAnchorElement | undefined
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clickedAnchor = this
    })
    frontendProtocolLogger.setEnabled('download-session', true)
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.log({ sessionID: 'download-session', source: 'test', kind: 'event' })

    downloadFrontendProtocolJSONL('download-session')

    expect(createObjectURL).toHaveBeenCalledTimes(1)
    expect(createObjectURL.mock.calls[0][0]).toBeInstanceOf(Blob)
    expect(clickedAnchor?.download).toBe(frontendProtocolLogFilename('download-session'))
    expect(revokeObjectURL).not.toHaveBeenCalled()
    vi.advanceTimersByTime(0)
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:frontend-log')
  })

  it('copies metadata and records as JSONL and reports clipboard rejection', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    frontendProtocolLogger.setEnabled('copy-session', true)
    vi.spyOn(console, 'log').mockImplementation(() => {})
    frontendProtocolLogger.log({ sessionID: 'copy-session', source: 'test', kind: 'event', data: { text: 'complete' } })

    await copyFrontendProtocolJSONL('copy-session')

    const copied = String(writeText.mock.calls[0][0])
    const lines = copied.trimEnd().split('\n').map((line) => JSON.parse(line) as Record<string, unknown>)
    expect(lines[0]).toMatchObject({ kind: 'metadata', session_id: 'copy-session', retained_count: 1, dropped_count: 0 })
    expect(lines[1]).toMatchObject({ kind: 'event', session_id: 'copy-session', data: { text: 'complete' } })

    writeText.mockRejectedValueOnce(new Error('denied'))
    await expect(copyFrontendProtocolJSONL('copy-session')).rejects.toThrow('Clipboard access failed')
  })
})
