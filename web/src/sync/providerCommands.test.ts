import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { CommandFacade } from './commandFacade'
import type { BlobClient } from './blobClient'
import type { RuntimeTransport } from './runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'
import { decodeProviderDiscoverResult, encodeProviderTarget } from './providerCommandCodec'
import { isProviderCreateName } from '../domain/providerIdentity'

class ProviderCommandTransport implements RuntimeTransport {
  isReady = true
  connectionGeneration = 1
  serverEpoch = 'epoch_1'
  sent: ProtocolMessage[] = []
  private messages = new Set<(message: ProtocolMessage, generation: number) => void>()
  private ready = new Set<(event: TransportReadyEvent) => void>()
  start(): void { this.isReady = true }
  stop(): void { this.isReady = false }
  send(message: ProtocolMessage): void { this.sent.push(message) }
  onMessage(listener: (message: ProtocolMessage, generation: number) => void): () => void { this.messages.add(listener); return () => this.messages.delete(listener) }
  onReady(listener: (event: TransportReadyEvent) => void): () => void { this.ready.add(listener); return () => this.ready.delete(listener) }
  onClose(_listener: (event: TransportCloseEvent) => void): () => void { return () => undefined }
  emitResult(requestID: string, result: unknown, generation = this.connectionGeneration): void {
    const message = decodeMessage(JSON.stringify({ version: 1, type: 'command_result', id: 'result', payload: { request_id: requestID, status: 'succeeded', result } }))
    for (const listener of [...this.messages]) listener(message, generation)
  }
  emitReady(epoch: string, previous: string): void {
    this.serverEpoch = epoch
    for (const listener of [...this.ready]) listener({ generation: this.connectionGeneration, serverEpoch: epoch, previousServerEpoch: previous, connectionID: 'connection', heartbeatIntervalMS: 1000, maxMessageBytes: 1024 })
  }
}

const blobDescriptor = { id: 'blob_1', url: '/api/blobs/blob_1', content_type: 'application/json', size: 20, sha256: 'a'.repeat(64), etag: '"etag"', expires_at: '2099-01-01T00:00:00Z' }

class RecordingBlobClient {
  readonly calls: Array<{ signal?: AbortSignal; resolve: (value: unknown) => void; reject: (reason?: unknown) => void }> = []

  getJSON(_descriptor: unknown, options: { signal?: AbortSignal } = {}): Promise<unknown> {
    return new Promise((resolve, reject) => {
      const call = { signal: options.signal, resolve, reject }
      this.calls.push(call)
      options.signal?.addEventListener('abort', () => reject(new Error('blob read aborted')), { once: true })
    })
  }
}

const target = {
  base_url_mode: 'replace' as const,
  base_url: 'https://例.example/v1',
  api_key: 'secret-do-not-echo',
  keep_api_key: false,
  auth_file_mode: 'replace' as const,
  auth_file: '',
  request_timeout: '30s',
  http_proxy_mode: 'replace' as const,
  http_proxy: '',
  https_proxy_mode: 'replace' as const,
  https_proxy: '',
  max_concurrent_requests: 2,
  models: [{
    profile: '主 模型', id: 'model-主', type: '', compatibility: '', input: ['text'], developer_role: '',
    context_window: 1000, input_limit: 900, output_limit: 100,
    parameters_mode: 'replace' as const,
    parameters: { temperature: 0.2, enabled: true, nested: { value: null } },
    reasoning_config: { parameter: 'reasoning_effort', default: 'medium', levels: { low: 'low', medium: 2, off: false, none: null } },
    pricing: null,
  }],
} as const

describe('ProviderCommands', () => {
  it('permits an empty safe endpoint only for an existing-provider preserve target', () => {
    const encoded = encodeProviderTarget({ ...target, base_url: '', base_url_mode: 'preserve' })
    expect(encoded.base_url).toBe('')
    expect(() => encodeProviderTarget({ ...target, base_url: '', base_url_mode: 'replace' })).toThrow()
  })

  it('submits a complete create target, strictly checks result identity, and only retries in the same epoch', async () => {
    const transport = new ProviderCommandTransport()
    const facade = new CommandFacade({ transport, operationIDGenerator: () => 'operation_provider_create', requestIDGenerator: () => 'request_provider_create' })
    const pending = facade.createProvider('created.provider', target, { operationID: 'operation_provider_create' })
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    expect(command.payload.name).toBe('provider.create')
    expect(command.payload.arguments).toMatchObject({ operation_id: 'operation_provider_create', provider: 'created.provider', api_key: 'secret-do-not-echo' })

    transport.connectionGeneration = 2
    transport.emitReady('epoch_1', 'epoch_1')
    expect(transport.sent).toHaveLength(2)
    expect(transport.sent[1]).toEqual(command)
    const retry = transport.sent[1]
    if (retry.type !== 'command') throw new Error('wrong retry')
    transport.emitResult(retry.payload.request_id, { operation_id: 'operation_provider_create', provider: 'created.provider', status: 'applied', changed: true }, 2)
    await expect(pending).resolves.toEqual({ operation_id: 'operation_provider_create', provider: 'created.provider', status: 'applied', changed: true })
    facade.stop()

    const unsafeTransport = new ProviderCommandTransport()
    const unsafeFacade = new CommandFacade({ transport: unsafeTransport, requestIDGenerator: () => 'request_provider_create_unsafe' })
    const unsafe = unsafeFacade.createProvider('created.provider', target, { operationID: 'operation_provider_create_unsafe' })
    expect(unsafeTransport.sent).toHaveLength(1)
    unsafeTransport.connectionGeneration = 2
    unsafeTransport.emitReady('epoch_2', 'epoch_1')
    expect(unsafeTransport.sent).toHaveLength(1)
    await expect(unsafe).rejects.toMatchObject({ code: 'outcome_unknown' })
    unsafeFacade.stop()
  })

  it('mirrors the execution create-only filename boundary and does not send invalid creates', async () => {
    for (const name of ['CON', 'provider.', 'provider ', 'bad:name', 'provider?query', 'COM1.profile']) expect(isProviderCreateName(name)).toBe(false)
    expect(isProviderCreateName('valid.provider')).toBe(true)
    const tooLong = 'x'.repeat(253)
    expect(isProviderCreateName(tooLong)).toBe(false)
    for (const provider of ['CON', 'provider.', 'provider ', 'bad:name']) {
      const transport = new ProviderCommandTransport()
      const facade = new CommandFacade({ transport, requestIDGenerator: () => `request_invalid_create_${provider}` })
      await expect(facade.createProvider(provider, target, { operationID: 'operation_invalid_create' })).rejects.toMatchObject({ code: 'invalid' })
      expect(transport.sent).toHaveLength(0)
      facade.stop()
    }
    const transport = new ProviderCommandTransport()
    const facade = new CommandFacade({ transport, requestIDGenerator: () => 'request_invalid_create_target' })
    await expect(facade.createProvider('valid.provider', { ...target, models: [] }, { operationID: 'operation_invalid_create_target' })).rejects.toMatchObject({ code: 'invalid' })
    expect(transport.sent).toHaveLength(0)
    facade.stop()
  })

  it('does not consume an automatically generated operation ID when target validation fails', async () => {
    const transport = new ProviderCommandTransport()
    let generated = 0
    const facade = new CommandFacade({
      transport,
      operationIDGenerator: () => {
        generated += 1
        return 'operation_provider_reusable'
      },
      requestIDGenerator: () => 'request_provider_reusable',
    })
    await expect(facade.createProvider('provider-reusable', { ...target, models: [] })).rejects.toMatchObject({ code: 'invalid' })
    expect(generated).toBe(0)
    expect(transport.sent).toHaveLength(0)

    const pending = facade.createProvider('provider-reusable', target)
    expect(generated).toBe(1)
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    expect(command.payload.arguments).toMatchObject({ operation_id: 'operation_provider_reusable' })
    transport.emitResult(command.payload.request_id, { operation_id: 'operation_provider_reusable', provider: 'provider-reusable', status: 'applied', changed: true })
    await expect(pending).resolves.toEqual({ operation_id: 'operation_provider_reusable', provider: 'provider-reusable', status: 'applied', changed: true })
    facade.stop()
  })

  it('rejects create acknowledgements with extra fields or mismatched identity', async () => {
    const transport = new ProviderCommandTransport()
    const facade = new CommandFacade({ transport, requestIDGenerator: () => 'request_provider_create_result' })
    const extra = facade.createProvider('provider-result', target, { operationID: 'operation_provider_result' })
    const extraCommand = transport.sent[0]
    if (extraCommand.type !== 'command') throw new Error('wrong command')
    transport.emitResult(extraCommand.payload.request_id, { operation_id: 'operation_provider_result', provider: 'provider-result', status: 'applied', secret: 'must-not-be-accepted' })
    await expect(extra).rejects.toMatchObject({ code: 'invalid' })
    facade.stop()

    const mismatchTransport = new ProviderCommandTransport()
    const mismatchFacade = new CommandFacade({ transport: mismatchTransport, requestIDGenerator: () => 'request_provider_create_mismatch' })
    const mismatch = mismatchFacade.createProvider('provider-result', target, { operationID: 'operation_provider_result' })
    const mismatchCommand = mismatchTransport.sent[0]
    if (mismatchCommand.type !== 'command') throw new Error('wrong command')
    mismatchTransport.emitResult(mismatchCommand.payload.request_id, { operation_id: 'operation_other', provider: 'provider-result', status: 'applied', changed: true })
    await expect(mismatch).rejects.toMatchObject({ code: 'invalid' })
    mismatchFacade.stop()
  })

  it('submits a complete Unicode target and never decodes a credential-bearing result', async () => {
    const transport = new ProviderCommandTransport()
    const facade = new CommandFacade({ transport, requestIDGenerator: () => 'request_provider_update' })
    const pending = facade.update('空 白 provider', target)
    const command = transport.sent[0]
    expect(command.type).toBe('command')
    if (command.type !== 'command') throw new Error('wrong command')
    expect(command.payload.name).toBe('provider.update')
    expect(command.payload.arguments).toMatchObject({ provider: '空 白 provider', api_key: 'secret-do-not-echo', models: [{ parameters: target.models[0].parameters }] })
    transport.emitResult(command.payload.request_id, { provider: '空 白 provider', status: 'applied', changed: false })
    await expect(pending).resolves.toEqual({ provider: '空 白 provider', status: 'applied', changed: false })
    facade.stop()
  })

  it('strictly decodes default acknowledgement and retries safe commands across epochs', async () => {
    const transport = new ProviderCommandTransport()
    let request = 0
    const facade = new CommandFacade({ transport, requestIDGenerator: () => `request_default_${++request}` })
    const pending = facade.setDefault('space provider', '主 模型')
    const first = transport.sent[0]
    if (first.type !== 'command') throw new Error('wrong command')
    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    const retry = transport.sent[1]
    expect(retry).toEqual(first)
    if (retry.type !== 'command') throw new Error('wrong retry')
    transport.emitResult(retry.payload.request_id, { provider: 'space provider', model: '主 模型', status: 'applied' }, 2)
    await expect(pending).resolves.toEqual({ provider: 'space provider', model: '主 模型', status: 'applied' })
    const malformed = facade.setDefault('space provider', 'another')
    const malformedCommand = transport.sent[2]
    if (malformedCommand.type !== 'command') throw new Error('wrong command')
    transport.emitResult(malformedCommand.payload.request_id, { provider: 'space provider', model: 'another', status: 'applied', extra: true })
    await expect(malformed).rejects.toMatchObject({ code: 'invalid' })
    facade.stop()
  })

  it('decodes inline/blob discovery only to domain models and rejects unsorted output', async () => {
    const inline = await decodeProviderDiscoverResult({ provider: 'p', models: ['alpha', 'zeta'] }, 'p')
    expect(inline).toEqual({ provider: 'p', models: ['alpha', 'zeta'] })
    await expect(decodeProviderDiscoverResult({ provider: 'p', models: ['zeta', 'alpha'] }, 'p')).rejects.toThrow()
    const blobClient = { getJSON: async () => ['alpha', 'zeta'] } as unknown as BlobClient
    const blob = await decodeProviderDiscoverResult({ provider: 'p', blob: blobDescriptor }, 'p', blobClient)
    expect(blob).toEqual({ provider: 'p', models: ['alpha', 'zeta'] })
    for (const invalid of [
      { ...blobDescriptor, content_type: 'text/plain' },
      { ...blobDescriptor, size: Number.MAX_SAFE_INTEGER + 1 },
      { ...blobDescriptor, sha256: 'not-a-digest' },
      { ...blobDescriptor, url: 'https://user:password@example.test/blob' },
    ]) {
      await expect(decodeProviderDiscoverResult({ provider: 'p', blob: invalid }, 'p', blobClient)).rejects.toThrow()
    }
  })

  it('rejects target values that could be changed by JSON serialization before sending', async () => {
    let deep: unknown = null
    for (let index = 0; index < 40; index += 1) deep = { nested: deep }
    const tooManyFields = Object.fromEntries(Array.from({ length: 16_385 }, (_, index) => [`field_${index}`, true]))
    const tooLarge = { text: 'x'.repeat(1_048_577) }
    const invalidTargets: unknown[] = [
      { ...target, max_concurrent_requests: Number.NaN },
      { ...target, max_concurrent_requests: Number.POSITIVE_INFINITY },
      { ...target, max_concurrent_requests: Number.MAX_SAFE_INTEGER + 1 },
      { ...target, max_concurrent_requests: -1 },
      { ...target, max_concurrent_requests: 1_000_000_001 },
      { ...target, base_url: '\ud800' },
      { ...target, models: [{ ...target.models[0], parameters: { secret: '\ud800' } }] },
      { ...target, models: [{ ...target.models[0], parameters: { value: Number.MAX_SAFE_INTEGER + 1 } }] },
      { ...target, models: [{ ...target.models[0], parameters: { value: Number.NaN } }] },
      { ...target, models: [{ ...target.models[0], parameters: { value: Number.POSITIVE_INFINITY } }] },
      { ...target, models: [{ ...target.models[0], parameters: deep }] },
      { ...target, models: [{ ...target.models[0], parameters: { values: Array.from({ length: 4_097 }, () => 1) } }] },
      { ...target, models: [{ ...target.models[0], parameters: tooManyFields }] },
      { ...target, models: [{ ...target.models[0], parameters: tooLarge }] },
    ]
    for (const invalid of invalidTargets) {
      const transport = new ProviderCommandTransport()
      const facade = new CommandFacade({ transport, requestIDGenerator: () => 'request_invalid_target' })
      await expect(facade.update('p', invalid as never)).rejects.toMatchObject({ code: 'invalid' })
      expect(transport.sent).toHaveLength(0)
      facade.stop()
    }
  })

  it('decodes a blob once per generation and aborts stale or cancelled reads', async () => {
    const transport = new ProviderCommandTransport()
    const blobClient = new RecordingBlobClient()
    const facade = new CommandFacade({ transport, blobClient: blobClient as unknown as BlobClient, requestIDGenerator: () => 'request_blob_once' })
    const pending = facade.discoverModels('p')
    const command = transport.sent[0]
    if (command.type !== 'command') throw new Error('wrong command')
    transport.emitResult(command.payload.request_id, { provider: 'p', blob: blobDescriptor })
    transport.emitResult(command.payload.request_id, { provider: 'p', blob: blobDescriptor })
    expect(blobClient.calls).toHaveLength(1)
    blobClient.calls[0].resolve(['alpha', 'zeta'])
    await expect(pending).resolves.toEqual({ provider: 'p', models: ['alpha', 'zeta'] })
    facade.stop()

    const retryTransport = new ProviderCommandTransport()
    const retryBlobClient = new RecordingBlobClient()
    let requestNumber = 0
    const retryFacade = new CommandFacade({ transport: retryTransport, blobClient: retryBlobClient as unknown as BlobClient, requestIDGenerator: () => `request_blob_retry_${++requestNumber}` })
    const retried = retryFacade.discoverModels('p')
    const first = retryTransport.sent[0]
    if (first.type !== 'command') throw new Error('wrong command')
    retryTransport.emitResult(first.payload.request_id, { provider: 'p', blob: blobDescriptor }, 1)
    retryTransport.connectionGeneration = 2
    retryTransport.emitReady('epoch_2', 'epoch_1')
    const resent = retryTransport.sent[1]
    if (resent.type !== 'command') throw new Error('wrong retry command')
    retryTransport.emitResult(resent.payload.request_id, { provider: 'p', blob: blobDescriptor }, 2)
    expect(retryBlobClient.calls).toHaveLength(2)
    expect(retryBlobClient.calls[0].signal?.aborted).toBe(true)
    retryBlobClient.calls[0].resolve(['old'])
    retryBlobClient.calls[1].resolve(['alpha', 'zeta'])
    await expect(retried).resolves.toEqual({ provider: 'p', models: ['alpha', 'zeta'] })
    retryFacade.stop()
  })

  it('aborts an in-flight blob read on timeout, stop, and caller cancellation', async () => {
    const timer = { next: 1, handlers: new Map<number, () => void>() }
    const setTimeout = (handler: () => void): number => {
      const id = timer.next++
      timer.handlers.set(id, handler)
      return id
    }
    const clearTimeout = (id: number): void => { timer.handlers.delete(id) }
    const runTimer = (): void => {
      const entry = timer.handlers.entries().next().value as [number, () => void] | undefined
      if (!entry) throw new Error('timer not found')
      timer.handlers.delete(entry[0])
      entry[1]()
    }

    const timeoutTransport = new ProviderCommandTransport()
    const timeoutBlobClient = new RecordingBlobClient()
    const timeoutFacade = new CommandFacade({ transport: timeoutTransport, blobClient: timeoutBlobClient as unknown as BlobClient, timeoutMS: 1000, setTimeout, clearTimeout, requestIDGenerator: () => 'request_blob_timeout' })
    const timedOut = timeoutFacade.discoverModels('p')
    const timeoutCommand = timeoutTransport.sent[0]
    if (timeoutCommand.type !== 'command') throw new Error('wrong command')
    timeoutTransport.emitResult(timeoutCommand.payload.request_id, { provider: 'p', blob: blobDescriptor })
    runTimer()
    await expect(timedOut).rejects.toMatchObject({ code: 'timeout' })
    expect(timeoutBlobClient.calls[0].signal?.aborted).toBe(true)

    const stopTransport = new ProviderCommandTransport()
    const stopBlobClient = new RecordingBlobClient()
    const stopFacade = new CommandFacade({ transport: stopTransport, blobClient: stopBlobClient as unknown as BlobClient, requestIDGenerator: () => 'request_blob_stop' })
    const stopped = stopFacade.discoverModels('p')
    const stopCommand = stopTransport.sent[0]
    if (stopCommand.type !== 'command') throw new Error('wrong command')
    stopTransport.emitResult(stopCommand.payload.request_id, { provider: 'p', blob: blobDescriptor })
    stopFacade.stop()
    await expect(stopped).rejects.toMatchObject({ code: 'stopped' })
    expect(stopBlobClient.calls[0].signal?.aborted).toBe(true)

    const abortTransport = new ProviderCommandTransport()
    const abortBlobClient = new RecordingBlobClient()
    const abortFacade = new CommandFacade({ transport: abortTransport, blobClient: abortBlobClient as unknown as BlobClient, requestIDGenerator: () => 'request_blob_abort' })
    const controller = new AbortController()
    const aborted = abortFacade.discoverModels('p', { signal: controller.signal })
    const abortCommand = abortTransport.sent[0]
    if (abortCommand.type !== 'command') throw new Error('wrong command')
    abortTransport.emitResult(abortCommand.payload.request_id, { provider: 'p', blob: blobDescriptor })
    controller.abort()
    await expect(aborted).rejects.toMatchObject({ code: 'cancelled' })
    expect(abortBlobClient.calls[0].signal?.aborted).toBe(true)
    abortFacade.stop()
  })
})
