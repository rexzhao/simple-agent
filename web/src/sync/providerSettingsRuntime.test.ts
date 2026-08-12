import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { ProviderSettingsRepository as DomainRepository } from '../repositories/providerSettings'
import { ProviderSettingsStore } from './providerSettingsStore'
import { SyncRuntime, type RuntimeTransport } from './runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'

class ProviderSettingsFakeTransport implements RuntimeTransport {
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
  emitReady(): void {
    for (const listener of [...this.ready]) listener({ generation: this.connectionGeneration, serverEpoch: this.serverEpoch, connectionID: `connection_${this.connectionGeneration}`, heartbeatIntervalMS: 1000, maxMessageBytes: 1024 * 1024 })
  }
  emitClose(): void {
    this.isReady = false
    for (const listener of [...this.closed]) listener({ generation: this.connectionGeneration, willRetry: true })
  }
  lastSubscribe(): Extract<ProtocolMessage, { type: 'subscribe' }> {
    const message = [...this.sent].reverse().find((candidate) => candidate.type === 'subscribe')
    if (!message || message.type !== 'subscribe') throw new Error('missing provider settings subscribe')
    return message
  }
}

const resource = { type: 'provider_settings' as const, id: 'server' }

function provider(modelID: string): Record<string, unknown> {
  return {
    name: 'alpha', base_url: 'https://example.test/v1', api_key_configured: true, auth_file: '', request_timeout: '', http_proxy: '', https_proxy: '', max_concurrent_requests: 0,
    models: [{ profile: 'fast', id: modelID, type: '', compatibility: '', input: ['text'], developer_role: '', context_window: 32000, input_limit: 0, output_limit: 0, reasoning_config: { type: 'budget_tokens', parameter: 'budget_tokens', default: 'medium', levels: [{ name: 'budget_tokens', value: 1234 }] }, pricing: null }],
  }
}

function frame(type: string, id: string, payload: unknown): ProtocolMessage {
  return decodeMessage(JSON.stringify({ version: 1, type, id, payload }))
}

function subscribed(subscriptionID: string, sequence: string): ProtocolMessage {
  return frame('subscribed', 'subscribed_' + sequence, { subscription_id: subscriptionID, resource, stream_epoch: 'stream_1', sequence })
}

function snapshot(subscriptionID: string): ProtocolMessage {
  return frame('snapshot', 'snapshot_0', { subscription_id: subscriptionID, resource, stream_epoch: 'stream_1', sequence: '0', resource_revision: '0', content: { inline: { server_root: '/srv', config_path: '/srv/sai.yaml', default_provider: 'alpha', default_model: 'fast', providers: [provider('initial')] } } })
}

function change(subscriptionID: string, sequence: string, modelID: string): ProtocolMessage {
  return frame('change', 'change_' + sequence, { subscription_id: subscriptionID, resource, stream_epoch: 'stream_1', sequence, previous_sequence: String(Number(sequence) - 1), resource_revision: sequence, operations: [{ op: 'upsert', key: 'alpha', value: provider(modelID) }] })
}

async function settle(): Promise<void> { await Promise.resolve(); await Promise.resolve(); await Promise.resolve() }

describe('ProviderSettings through SyncRuntime', () => {
  it('applies snapshot/change and reconnect replay while the domain repository sees only availability and atomic state', async () => {
    const transport = new ProviderSettingsFakeTransport()
    const runtime = new SyncRuntime({ transport })
    const store = new ProviderSettingsStore(runtime.replica)
    const repository = new DomainRepository(store)
    const release = runtime.subscribe(resource)
    runtime.start()

    const first = transport.lastSubscribe()
    transport.emit(subscribed(first.payload.subscription_id, '0'))
    transport.emit(snapshot(first.payload.subscription_id))
    await settle()
    expect(repository.getSnapshot().availability.status).toBe('ready')
    expect(repository.getProvider('alpha')?.models[0].id).toBe('initial')
    expect(repository.getSnapshot()).not.toHaveProperty('sequence')
    expect(repository.getSnapshot()).not.toHaveProperty('blob')

    transport.emit(change(first.payload.subscription_id, '1', 'changed'))
    expect(repository.getSnapshot().availability.status).toBe('ready')
    expect(repository.getModel('alpha', 'fast')?.id).toBe('changed')

    transport.emitClose()
    expect(repository.getSnapshot().availability.status).toBe('stale')
    transport.connectionGeneration = 2
    transport.isReady = true
    transport.emitReady()
    const resumed = transport.lastSubscribe()
    expect(resumed.payload.resume).toEqual({ stream_epoch: 'stream_1', sequence: '1' })
    transport.emit(subscribed(resumed.payload.subscription_id, '2'))
    expect(repository.getSnapshot().availability.status).toBe('stale')
    transport.emit(change(resumed.payload.subscription_id, '2', 'reconnected'))
    expect(repository.getSnapshot().availability.status).toBe('ready')
    expect(repository.getModel('alpha', 'fast')?.id).toBe('reconnected')

    release()
    runtime.stop()
    store.dispose()
  })
})
