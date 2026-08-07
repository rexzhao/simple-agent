import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { createCurrentCodexLoginProviderSignal, CodexLoginInterestPolicy } from './interestPolicy'
import { CodexLoginStore } from './codexLoginStore'
import { SyncRuntime, type RuntimeTransport } from './runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'

class CodexFakeTransport implements RuntimeTransport {
  isReady = true
  connectionGeneration = 1
  serverEpoch = 'server_1'
  sent: ProtocolMessage[] = []
  private messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private ready = new Set<(event: TransportReadyEvent) => void>()
  private closed = new Set<(event: TransportCloseEvent) => void>()

  start(): void { this.isReady = true }
  stop(): void { this.isReady = false }
  send(message: ProtocolMessage): void { this.sent.push(message) }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(listener: (event: TransportCloseEvent) => void): () => void { this.closed.add(listener); return () => this.closed.delete(listener) }
  emit(message: unknown, generation = this.connectionGeneration): void {
    const decoded = typeof message === 'string' ? decodeMessage(message) : message as ProtocolMessage
    for (const listener of [...this.messages]) listener(decoded, generation)
  }
  emitClose(): void {
    this.isReady = false
    for (const listener of [...this.closed]) listener({ generation: this.connectionGeneration, willRetry: true })
  }
  lastSubscribe(provider: string): Extract<ProtocolMessage, { type: 'subscribe' }> {
    const message = [...this.sent].reverse().find((candidate) => candidate.type === 'subscribe' && candidate.payload.resource.id === provider)
    if (!message || message.type !== 'subscribe') throw new Error(`missing subscribe for ${provider}`)
    return message
  }
}

function frame(type: string, id: string, payload: unknown): ProtocolMessage {
  return decodeMessage(JSON.stringify({ version: 1, type, id, payload }))
}

function resource(provider: string) {
  return { type: 'codex_login' as const, id: provider }
}

function signedOut(provider: string): Record<string, unknown> {
  return { provider, status: 'signed_out', login_id: '', user_code: '', verification_url: '', refreshable: false, error_code: '', error_message: '' }
}

function pending(provider: string): Record<string, unknown> {
  return { provider, status: 'pending', login_id: 'login-1', user_code: 'ABCD-1234', verification_url: 'https://example.test/device', refreshable: false, error_code: '', error_message: '' }
}

function signedIn(provider: string): Record<string, unknown> {
  return { provider, status: 'signed_in', login_id: 'login-1', user_code: '', verification_url: '', refreshable: true, error_code: '', error_message: '' }
}

function subscribed(provider: string, subscriptionID: string): ProtocolMessage {
  return frame('subscribed', `subscribed-${provider}`, { subscription_id: subscriptionID, resource: resource(provider), stream_epoch: 'stream_1', sequence: '0' })
}

function snapshot(provider: string, subscriptionID: string): ProtocolMessage {
  return frame('snapshot', `snapshot-${provider}`, { subscription_id: subscriptionID, resource: resource(provider), stream_epoch: 'stream_1', sequence: '0', resource_revision: '0', content: { inline: signedOut(provider) } })
}

function change(provider: string, subscriptionID: string, sequence: string, value: Record<string, unknown>): ProtocolMessage {
  return frame('change', `change-${provider}-${sequence}`, { subscription_id: subscriptionID, resource: resource(provider), stream_epoch: 'stream_1', sequence, previous_sequence: String(Number(sequence) - 1), resource_revision: sequence, operations: [{ op: 'replace', key: provider, value }] })
}

describe('CodexLogin through SyncRuntime', () => {
  it('projects signed-out → pending → signed-in → signed-out, isolates provider switches, and marks disconnect stale', () => {
    const transport = new CodexFakeTransport()
    const runtime = new SyncRuntime({ transport })
    const store = new CodexLoginStore(runtime.replica)
    const signal = createCurrentCodexLoginProviderSignal('alpha')
    const policy = new CodexLoginInterestPolicy(runtime, signal)
    policy.start()
    runtime.start()

    const alpha = transport.lastSubscribe('alpha')
    transport.emit(subscribed('alpha', alpha.payload.subscription_id))
    transport.emit(snapshot('alpha', alpha.payload.subscription_id))
    expect(store.getSnapshot('alpha').login?.status).toBe('signed_out')

    transport.emit(change('alpha', alpha.payload.subscription_id, '1', pending('alpha')))
    expect(store.getSnapshot('alpha').login?.status).toBe('pending')
    transport.emit(change('alpha', alpha.payload.subscription_id, '2', signedIn('alpha')))
    expect(store.getSnapshot('alpha').login?.status).toBe('signed_in')
    transport.emit(change('alpha', alpha.payload.subscription_id, '3', signedOut('alpha')))
    expect(store.getSnapshot('alpha').login?.status).toBe('signed_out')

    signal.set('beta')
    const beta = transport.lastSubscribe('beta')
    transport.emit(subscribed('beta', beta.payload.subscription_id))
    transport.emit(snapshot('beta', beta.payload.subscription_id))
    expect(store.getSnapshot('alpha').status).toBe('loading')
    expect(store.getSnapshot('beta').login?.status).toBe('signed_out')

    // A message already in flight for the released provider must not revive
    // its evicted replica or alter the currently selected provider.
    transport.emit(change('alpha', alpha.payload.subscription_id, '4', signedIn('alpha')))
    expect(store.getSnapshot('alpha').status).toBe('loading')
    expect(store.getSnapshot('beta').login?.status).toBe('signed_out')

    transport.emit(change('beta', beta.payload.subscription_id, '1', pending('beta')))
    expect(store.getSnapshot('beta').login?.status).toBe('pending')
    transport.emitClose()
    expect(store.getSnapshot('beta').availability.status).toBe('stale')

    policy.stop()
    runtime.stop()
    store.dispose()
  })
})
