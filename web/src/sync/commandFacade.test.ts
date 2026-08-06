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

  it('creates with separate cryptographic entity/request IDs and resends an identical payload across epochs', async () => {
    const transport = new FakeCommandTransport()
    let requestNumber = 0
    const facade = new CommandFacade({
      transport,
      requestIDGenerator: () => `request_create_${++requestNumber}`,
      sessionIDGenerator: () => 'session_create_stable',
    })
    const pending = facade.create('project_1', { displayName: 'Created', fullAccess: false })
    const initial = transport.sent[0]
    if (initial.type !== 'command') throw new Error('wrong initial command')
    expect(initial.payload.name).toBe('session.create')
    expect(initial.payload.arguments).toEqual({
      session_id: 'session_create_stable', project_id: 'project_1', display_name: 'Created', full_access: false,
    })
    expect(initial.payload.request_id).toBe('request_create_1')
    expect(initial.payload.request_id).not.toBe(initial.payload.arguments.session_id)

    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    const resent = transport.sent[1]
    if (resent.type !== 'command') throw new Error('wrong resent command')
    expect(resent.payload).toEqual(initial.payload)
    transport.emit(commandMessage('command_result', resent.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_create_stable', project_id: 'project_1' },
    }), 2)
    await expect(pending).resolves.toEqual({ session_id: 'session_create_stable', project_id: 'project_1' })
    await expect(facade.create('project_1', { displayName: 'other' })).rejects.toMatchObject({ code: 'id_generation', details: { collision: true } })

    const failedTransport = new FakeCommandTransport()
    const failed = new CommandFacade({ transport: failedTransport, sessionIDGenerator: () => { throw new Error('no entropy') } })
    await expect(failed.create('project_1')).rejects.toMatchObject({ code: 'id_generation' })
    expect(failedTransport.sent).toHaveLength(0)
    facade.stop()
    failed.stop()
  })

  it('allows explicit stable session IDs to retry past the local collision cache', async () => {
    const transport = new FakeCommandTransport()
    let requestNumber = 0
    const facade = new CommandFacade({ transport, requestIDGenerator: () => `request_explicit_${++requestNumber}` })
    const first = facade.create('project_1', { sessionID: 'session_explicit_stable', displayName: 'Created' })
    const firstMessage = transport.sent[0]
    if (firstMessage.type !== 'command') throw new Error('wrong first command')
    transport.emit(commandMessage('command_result', firstMessage.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_explicit_stable', project_id: 'project_1' },
    }))
    await expect(first).resolves.toEqual({ session_id: 'session_explicit_stable', project_id: 'project_1' })

    // This is a new request after the first result (the same case also occurs
    // after a timeout/page restore). The explicit entity ID bypasses only the
    // process-local cache; the server remains responsible for dedupe/conflict.
    const retry = facade.create('project_1', { sessionID: 'session_explicit_stable', displayName: 'Created' })
    const retryMessage = transport.sent[1]
    if (retryMessage.type !== 'command') throw new Error('wrong retry command')
    expect(retryMessage.payload.arguments).toEqual(firstMessage.payload.arguments)
    expect(retryMessage.payload.request_id).not.toBe(firstMessage.payload.request_id)
    transport.emit(commandMessage('command_result', retryMessage.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_explicit_stable', project_id: 'project_1' },
    }))
    await expect(retry).resolves.toEqual({ session_id: 'session_explicit_stable', project_id: 'project_1' })

    const conflict = facade.create('project_1', { sessionID: 'session_explicit_stable', displayName: 'Different' })
    const conflictMessage = transport.sent[2]
    if (conflictMessage.type !== 'command') throw new Error('wrong conflict command')
    transport.emit(commandMessage('command_result', conflictMessage.payload.request_id, {
      status: 'failed', error: { code: 'idempotency_conflict', message: 'session identity is already claimed' },
    }))
    await expect(conflict).rejects.toMatchObject({ code: 'idempotency_conflict' })

    for (const sessionID of ['../not-valid', 'blobs.', 'BLOBS..', '.session-claims', '.SESSION-CLAIMS.']) {
      await expect(facade.create('project_1', { sessionID })).rejects.toMatchObject({ code: 'invalid' })
    }
    expect(transport.sent).toHaveLength(3)
    facade.stop()
  })

  it('starts a durable run with a stable client identity and identical cross-epoch payload', async () => {
    const transport = new FakeCommandTransport()
    let requestNumber = 0
    const facade = new CommandFacade({
      transport,
      requestIDGenerator: () => `request_run_${++requestNumber}`,
      runIDGenerator: () => 'run_generated_stable',
    })
    const pending = facade.start('session_1', 'hello')
    const initial = transport.sent[0]
    if (initial.type !== 'command') throw new Error('wrong initial command')
    expect(initial.payload.name).toBe('run.start')
    expect(initial.payload.arguments).toEqual({ session_id: 'session_1', run_id: 'run_generated_stable', content: 'hello' })
    expect(initial.payload.request_id).not.toBe(initial.payload.arguments.run_id)

    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    const resent = transport.sent[1]
    if (resent.type !== 'command') throw new Error('wrong resent command')
    expect(resent.payload).toEqual(initial.payload)
    transport.emit(commandMessage('command_result', resent.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_generated_stable', status: 'running' },
    }), 2)
    await expect(pending).resolves.toEqual({ session_id: 'session_1', run_id: 'run_generated_stable', status: 'running' })

    const explicit = facade.startRun('session_1', 'hello', { runID: 'run_explicit_stable' })
    const retryRequest = transport.sent[2]
    if (retryRequest.type !== 'command') throw new Error('wrong explicit command')
    transport.emit(commandMessage('command_result', retryRequest.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_explicit_stable', status: 'interrupted' },
    }), 2)
    await expect(explicit).resolves.toEqual({ session_id: 'session_1', run_id: 'run_explicit_stable', status: 'interrupted' })

    const explicitRetry = facade.startRun('session_1', 'hello', { runID: 'run_explicit_stable' })
    const explicitRetryMessage = transport.sent[3]
    if (explicitRetryMessage.type !== 'command') throw new Error('wrong explicit retry command')
    expect(explicitRetryMessage.payload.arguments).toEqual(retryRequest.payload.arguments)
    expect(explicitRetryMessage.payload.request_id).not.toBe(retryRequest.payload.request_id)
    transport.emit(commandMessage('command_result', explicitRetryMessage.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_explicit_stable', status: 'interrupted' },
    }), 2)
    await expect(explicitRetry).resolves.toMatchObject({ status: 'interrupted' })

    const collision = facade.start('session_1', 'different')
    await expect(collision).rejects.toMatchObject({ code: 'id_generation', details: { collision: true } })
    facade.stop()
  })

  it('rejects invalid and over-sized durable run input and strict result shapes', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport, runIDGenerator: () => 'run_invalid_test' })
    await expect(facade.start('', 'hello')).rejects.toMatchObject({ code: 'invalid' })
    await expect(facade.start('session_1', 'x'.repeat(256 * 1024 + 1))).rejects.toMatchObject({ code: 'invalid' })
    const pending = facade.start('session_1', 'hello', { runID: 'run_result_shape' })
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    transport.emit(commandMessage('command_result', command.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_result_shape', status: 'running', extra: true },
    }))
    await expect(pending).rejects.toMatchObject({ code: 'invalid' })
    facade.stop()
  })

  it('continues with only session/run identity, separates request IDs, and retries the exact payload', async () => {
    const transport = new FakeCommandTransport()
    let requestNumber = 0
    const facade = new CommandFacade({
      transport,
      requestIDGenerator: () => `request_continue_${++requestNumber}`,
      runIDGenerator: () => 'run_continue_generated',
    })
    const pending = facade.continueRun('session_1')
    const initial = transport.sent[0]
    if (initial.type !== 'command') throw new Error('wrong command')
    expect(initial.payload.name).toBe('run.continue')
    expect(initial.payload.arguments).toEqual({ session_id: 'session_1', run_id: 'run_continue_generated' })
    expect(initial.payload.request_id).not.toBe(initial.payload.arguments.run_id)

    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    const resent = transport.sent[1]
    if (resent.type !== 'command') throw new Error('wrong resent command')
    expect(resent.payload).toEqual(initial.payload)
    transport.emit(commandMessage('command_result', resent.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_continue_generated', status: 'running' },
    }), 2)
    await expect(pending).resolves.toEqual({ session_id: 'session_1', run_id: 'run_continue_generated', status: 'running' })

    const explicit = facade.continueRun('session_1', { runID: 'run_continue_explicit' })
    const explicitMessage = transport.sent[2]
    if (explicitMessage.type !== 'command') throw new Error('wrong explicit command')
    transport.emit(commandMessage('command_result', explicitMessage.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_continue_explicit', status: 'failed' },
    }), 2)
    await expect(explicit).resolves.toEqual({ session_id: 'session_1', run_id: 'run_continue_explicit', status: 'failed' })

    const explicitRetry = facade.continueRun('session_1', { runID: 'run_continue_explicit' })
    const retryMessage = transport.sent[3]
    if (retryMessage.type !== 'command') throw new Error('wrong explicit retry')
    expect(retryMessage.payload.arguments).toEqual(explicitMessage.payload.arguments)
    expect(retryMessage.payload.request_id).not.toBe(explicitMessage.payload.request_id)
    transport.emit(commandMessage('command_result', retryMessage.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_continue_explicit', status: 'failed' },
    }), 2)
    await expect(explicitRetry).resolves.toMatchObject({ status: 'failed' })

    await expect(facade.continueRun('')).rejects.toMatchObject({ code: 'invalid' })
    await expect(facade.continueRun('session_1', { runID: 'not/valid' })).rejects.toMatchObject({ code: 'invalid' })
    const collision = facade.continueRun('session_1')
    await expect(collision).rejects.toMatchObject({ code: 'id_generation', details: { collision: true } })
    facade.stop()
  })

  it('rejects a continue result that is not the compact requested identity', async () => {
    const transport = new FakeCommandTransport()
    const facade = new CommandFacade({ transport })
    const pending = facade.continueRun('session_1', { runID: 'run_continue_shape' })
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    transport.emit(commandMessage('command_result', command.payload.request_id, {
      status: 'succeeded', result: { session_id: 'session_1', run_id: 'run_continue_shape', status: 'committed', extra: true },
    }))
    await expect(pending).rejects.toMatchObject({ code: 'invalid' })
    facade.stop()
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
    const statuses = ['running', 'committed', 'failed', 'interrupted', 'cancelled'] as const
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

  it('appends a stable prompt operation, resends the exact payload, and strictly decodes the acknowledgement', async () => {
    const transport = new FakeCommandTransport()
    let requestNumber = 0
    const facade = new CommandFacade({
      transport,
      requestIDGenerator: () => `request_append_${++requestNumber}`,
      operationIDGenerator: () => 'operation_append_stable',
    })
    const content = '\n  exact prompt  \t'
    const pending = facade.appendPrompt('session_1', 'run_1', content)
    const initial = transport.sent[0]
    if (initial.type !== 'command') throw new Error('wrong initial command')
    expect(initial.payload.name).toBe('run.prompt.append')
    expect(initial.payload.arguments).toEqual({ session_id: 'session_1', run_id: 'run_1', operation_id: 'operation_append_stable', content })
    expect(initial.payload.request_id).not.toBe(initial.payload.arguments.operation_id)

    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    const resent = transport.sent[1]
    if (resent.type !== 'command') throw new Error('wrong resent command')
    expect(resent.payload).toEqual(initial.payload)
    transport.emit(commandMessage('command_result', resent.payload.request_id, {
      status: 'succeeded', result: { operation_id: 'operation_append_stable', session_id: 'session_1', run_id: 'run_1', accepted: true },
    }), 2)
    await expect(pending).resolves.toEqual({ operation_id: 'operation_append_stable', session_id: 'session_1', run_id: 'run_1', accepted: true })

    // Explicit operation IDs bypass the process-local collision cache and
    // use a new request_id for a page-restore/timeout retry.
    const explicit = facade.appendPrompt('session_1', 'run_1', content, { operationID: 'operation_append_stable' })
    const retry = transport.sent[2]
    if (retry.type !== 'command') throw new Error('wrong explicit retry')
    expect(retry.payload.arguments).toEqual(initial.payload.arguments)
    expect(retry.payload.request_id).not.toBe(initial.payload.request_id)
    transport.emit(commandMessage('command_result', retry.payload.request_id, {
      status: 'succeeded', result: { operation_id: 'operation_append_stable', session_id: 'session_1', run_id: 'run_1', accepted: true },
    }), 2)
    await expect(explicit).resolves.toMatchObject({ accepted: true })

    await expect(facade.appendPrompt('session_1', 'run_1', 'different')).rejects.toMatchObject({ code: 'id_generation', details: { collision: true } })
    await expect(facade.appendPrompt('session_1', 'run_1', '')).rejects.toMatchObject({ code: 'invalid' })
    await expect(facade.appendPrompt('session_1', 'run_1', 'x'.repeat(64 * 1024 + 1), { operationID: 'operation_other' })).rejects.toMatchObject({ code: 'invalid' })

    const malformed = facade.appendPrompt('session_1', 'run_1', 'ok', { operationID: 'operation_result_shape' })
    const malformedMessage = transport.sent[3]
    if (malformedMessage.type !== 'command') throw new Error('wrong malformed command')
    transport.emit(commandMessage('command_result', malformedMessage.payload.request_id, {
      status: 'succeeded', result: { operation_id: 'operation_result_shape', session_id: 'session_1', run_id: 'run_1', accepted: true, extra: true },
    }))
    await expect(malformed).rejects.toMatchObject({ code: 'invalid' })
    facade.stop()
  })
})
