import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { CommandFacade, CommandFacadeError } from './commandFacade'
import type { RuntimeTransport } from './runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'

class FakeCommandTransport implements RuntimeTransport {
  isReady = true
  connectionGeneration = 1
  serverEpoch: string | undefined = 'epoch_1'
  sent: ProtocolMessage[] = []
  private messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private ready = new Set<(event: TransportReadyEvent) => void>()
  private close = new Set<(event: TransportCloseEvent) => void>()
  start(): void { this.isReady = true }
  stop(): void { this.isReady = false }
  send(message: ProtocolMessage): void { this.sent.push(message) }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.close.add(listener); return () => this.close.delete(listener) }
  emit(value: unknown, generation = this.connectionGeneration): void {
    const message = typeof value === 'string' ? decodeMessage(value) : value as ProtocolMessage
    for (const listener of [...this.messages]) listener(message, generation)
  }
  emitReady(epoch = this.serverEpoch ?? 'epoch_1', previousServerEpoch?: string): void {
    this.serverEpoch = epoch
    for (const listener of [...this.ready]) listener({ generation: this.connectionGeneration, serverEpoch: epoch, previousServerEpoch, connectionID: 'connection', heartbeatIntervalMS: 1000, maxMessageBytes: 1024 })
  }
}

function commandMessage(type: string, requestID: string, payload: unknown): ProtocolMessage {
  return decodeMessage(JSON.stringify({ version: 1, type, id: `${type}_1`, payload: { request_id: requestID, ...payload as object } }))
}

class TimerBox {
  next = 1
  timers = new Map<number, () => void>()
  set = (handler: () => void): number => { const id = this.next++; this.timers.set(id, handler); return id }
  clear = (id: number): void => { this.timers.delete(id) }
  runOnlyTimer(): void {
    const entry = this.timers.entries().next().value as [number, () => void] | undefined
    if (!entry) throw new Error('timer not found')
    this.timers.delete(entry[0])
    entry[1]()
  }
}

describe('CommandFacade session.mark_read', () => {
  it('correlates accepted/result in either order and never patches a replica', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport, timeoutMS: 1000 })
    const pending = facade.markRead('session_1', 'run_1', 'project_1')
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    expect(command.payload.name).toBe('session.mark_read')
    expect(command.payload.schema_version).toBe(1)
    expect(command.payload.arguments).toEqual({ session_id: 'session_1', run_id: 'run_1', project_id: 'project_1' })
    const requestID = command.payload.request_id
    transport.emit(commandMessage('command_result', requestID, { status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_1', marked_read: true } }))
    transport.emit(commandMessage('command_accepted', requestID, {}))
    await expect(pending).resolves.toEqual({ session_id: 'session_1', run_id: 'run_1', marked_read: true })
    facade.stop()
  })

  it('retries idempotently in the same epoch and safely across an epoch change', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport, timeoutMS: 1000 })
    const pending = facade.markRead('session_1', 'run_1')
    expect(transport.sent).toHaveLength(1)
    const initial = transport.sent[0]
    if (initial.type !== 'command') throw new Error('wrong initial command')
    transport.connectionGeneration = 2
    transport.emitReady('epoch_1', 'epoch_1')
    expect(transport.sent).toHaveLength(2)
    const sameEpochRetry = transport.sent[1]
    if (sameEpochRetry.type !== 'command') throw new Error('wrong same-epoch retry')
    expect(sameEpochRetry.payload.request_id).toBe(initial.payload.request_id)
    transport.connectionGeneration = 3
    transport.emitReady('epoch_2', 'epoch_1')
    expect(transport.sent).toHaveLength(3)
    const command = transport.sent[2]
    if (command.type !== 'command') throw new Error('wrong command')
    expect(command.payload.request_id).toBe(initial.payload.request_id)
    transport.emit(commandMessage('command_result', command.payload.request_id, { status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_1', marked_read: true } }), 3)
    await expect(pending).resolves.toMatchObject({ marked_read: true })
  })

  it('rejects on timeout, cancellation, and stop while clearing pending timers', async () => {
    const timers = new TimerBox()
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport, timeoutMS: 10, setTimeout: timers.set, clearTimeout: timers.clear })
    const timedOut = facade.markRead('session_1', 'run_1')
    timers.runOnlyTimer()
    await expect(timedOut).rejects.toBeInstanceOf(CommandFacadeError)
    await expect(timedOut).rejects.toMatchObject({ code: 'timeout' })

    const controller = new AbortController()
    const cancelled = facade.markRead('session_2', 'run_2', undefined, { signal: controller.signal })
    controller.abort()
    await expect(cancelled).rejects.toMatchObject({ code: 'cancelled' })
    const stopped = facade.markRead('session_3', 'run_3')
    facade.stop()
    await expect(stopped).rejects.toMatchObject({ code: 'stopped' })
    expect(timers.timers.size).toBe(0)
  })

  it('uses non-counter request IDs and rejects before sending when ID generation fails', async () => {
    const firstTransport = new FakeCommandTransport()
    const secondTransport = new FakeCommandTransport()
    const first = new CommandFacade({ transport: firstTransport })
    const second = new CommandFacade({ transport: secondTransport })
    const firstPending = first.markRead('session_1', 'run_1')
    const secondPending = second.markRead('session_1', 'run_1')
    const firstID = firstTransport.sent[0].type === 'command' ? firstTransport.sent[0].payload.request_id : ''
    const secondID = secondTransport.sent[0].type === 'command' ? secondTransport.sent[0].payload.request_id : ''
    expect(firstID).toMatch(/^request_[0-9a-f-]{36}$/i)
    expect(secondID).toMatch(/^request_[0-9a-f-]{36}$/i)
    expect(secondID).not.toBe(firstID)
    first.stop()
    second.stop()
    await expect(firstPending).rejects.toMatchObject({ code: 'stopped' })
    await expect(secondPending).rejects.toMatchObject({ code: 'stopped' })

    const failedTransport = new FakeCommandTransport()
    const failed = new CommandFacade({ transport: failedTransport, requestIDGenerator: () => { throw new Error('no entropy') } })
    await expect(failed.markRead('session_2', 'run_2')).rejects.toMatchObject({ code: 'id_generation' })
    expect(failedTransport.sent).toHaveLength(0)
  })

  it('ignores old-generation result and error frames after an idempotent resend', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport })
    const pending = facade.markRead('session_1', 'run_1')
    const first = transport.sent[0]
    if (first.type !== 'command') throw new Error('wrong initial command')
    transport.connectionGeneration = 2
    transport.emitReady('epoch_1', 'epoch_1')
    const resent = transport.sent.at(-1)
    if (!resent || resent.type !== 'command') throw new Error('missing resend')
    expect(resent.payload.request_id).toBe(first.payload.request_id)

    transport.emit(commandMessage('command_result', first.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_1', marked_read: true },
    }), 1)
    transport.emit(commandMessage('error', first.payload.request_id, { code: 'old', message: 'old generation' }), 1)
    let settled = false
    void pending.finally(() => { settled = true })
    await Promise.resolve()
    expect(settled).toBe(false)

    transport.emit(commandMessage('command_result', resent.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_1', marked_read: true },
    }), 2)
    await expect(pending).resolves.toEqual({ session_id: 'session_1', run_id: 'run_1', marked_read: true })
    facade.stop()
  })

  it('rejects request ID collisions without replacing the first pending promise', async () => {
    const timers = new TimerBox()
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({
      transport,
      requestIDGenerator: () => 'request_collision',
      timeoutMS: 10,
      setTimeout: timers.set,
      clearTimeout: timers.clear,
    })
    const first = facade.markRead('session_1', 'run_1')
    const second = facade.markRead('session_2', 'run_2')
    await expect(second).rejects.toMatchObject({ code: 'id_generation', details: { collision: true } })
    expect(transport.sent).toHaveLength(1)
    expect(timers.timers.size).toBe(1)
    facade.stop()
    await expect(first).rejects.toMatchObject({ code: 'stopped' })
    expect(timers.timers.size).toBe(0)

    const completed = new CommandFacade({ transport, requestIDGenerator: () => 'request_done' })
    const done = completed.markRead('session_3', 'run_3')
    const command = transport.sent.at(-1)
    if (!command || command.type !== 'command') throw new Error('missing completed command')
    transport.emit(commandMessage('command_result', command.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_3', run_id: 'run_3', marked_read: true },
    }))
    await expect(done).resolves.toMatchObject({ marked_read: true })
    await expect(completed.markRead('session_4', 'run_4')).rejects.toMatchObject({ code: 'id_generation', details: { collision: true } })
    completed.stop()
  })

  it('submits every E1 typed command with exact arguments and validates exact results', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport })
    const pending = [
      [facade.rename('session_1', 'Renamed'), 'session.rename', { session_id: 'session_1', display_name: 'Renamed' }, { session_id: 'session_1', display_name: 'Renamed' }],
      [facade.archive('session_1'), 'session.archive', { session_id: 'session_1' }, { session_id: 'session_1', archived: true }],
      [facade.restore('session_1'), 'session.restore', { session_id: 'session_1' }, { session_id: 'session_1', archived: false }],
      [facade.setFullAccess('session_1', false), 'session.set_full_access', { session_id: 'session_1', full_access: false }, { session_id: 'session_1', full_access: false }],
      [facade.setDebug('session_1', true), 'session.set_debug', { session_id: 'session_1', request_bodies: true }, { session_id: 'session_1', request_bodies: true }],
    ] as const
    expect(transport.sent).toHaveLength(pending.length)
    for (let index = 0; index < pending.length; index += 1) {
      const command = transport.sent[index]
      if (command.type !== 'command') throw new Error('wrong command')
      expect(command.payload.name).toBe(pending[index][1])
      expect(command.payload.arguments).toEqual(pending[index][2])
      transport.emit(commandMessage('command_result', command.payload.request_id, { status: 'succeeded', result: pending[index][3] }))
    }
    await expect(pending[0][0]).resolves.toEqual({ session_id: 'session_1', display_name: 'Renamed' })
    await expect(pending[1][0]).resolves.toEqual({ session_id: 'session_1', archived: true })
    await expect(pending[2][0]).resolves.toEqual({ session_id: 'session_1', archived: false })
    await expect(pending[3][0]).resolves.toEqual({ session_id: 'session_1', full_access: false })
    await expect(pending[4][0]).resolves.toEqual({ session_id: 'session_1', request_bodies: true })
    facade.stop()
  })

  it('rejects typed business errors without exposing or translating a result payload', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport })
    const pending = facade.rename('session_1', 'Renamed')
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    transport.emit(commandMessage('command_result', command.payload.request_id, {
      status: 'failed', error: { code: 'session_busy', message: 'session is busy' },
    }))
    await expect(pending).rejects.toMatchObject({ code: 'session_busy', message: 'session is busy' })
    facade.stop()
  })

  it('accepts every SessionRunStatus emitted by the Go run coordinator', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport })
    const statuses = ['running', 'committed', 'failed', 'cancelled'] as const
    const pending = statuses.map((status, index) => {
      const promise = facade.cancelRun(`run_${index}`)
      const command = transport.sent[index]
      if (command.type !== 'command') throw new Error('wrong command')
      transport.emit(commandMessage('command_result', command.payload.request_id, {
        status: 'succeeded', result: { run_id: `run_${index}`, status },
      }))
      return promise
    })
    for (let index = 0; index < statuses.length; index += 1) {
      await expect(pending[index]).resolves.toEqual({ run_id: `run_${index}`, status: statuses[index] })
    }
    facade.stop()
  })

  it('rejects unknown result fields and reports unsafe commands as outcome_unknown after an epoch change', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport, timeoutMS: 1000 })
    const invalid = facade.setDebug('session_1', true)
    const invalidCommand = transport.sent[0]
    if (invalidCommand.type !== 'command') throw new Error('wrong command')
    transport.emit(commandMessage('command_result', invalidCommand.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', request_bodies: true, unexpected: false },
    }))
    await expect(invalid).rejects.toMatchObject({ code: 'invalid' })

    const mismatched = facade.rename('session_1', 'Requested name')
    const mismatchedCommand = transport.sent[1]
    if (mismatchedCommand.type !== 'command') throw new Error('wrong command')
    transport.emit(commandMessage('command_result', mismatchedCommand.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', display_name: 'Different name' },
    }))
    await expect(mismatched).rejects.toMatchObject({ code: 'invalid' })

    const unsafe = facade.cancelRun('run_1')
    const unsafeCommand = transport.sent[2]
    if (unsafeCommand.type !== 'command') throw new Error('wrong command')
    expect(transport.sent).toHaveLength(3)
    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    expect(transport.sent).toHaveLength(3)
    await expect(unsafe).rejects.toMatchObject({ code: 'outcome_unknown' })
    facade.stop()
  })
})
