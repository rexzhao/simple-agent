import { describe, expect, it } from 'vitest'
import { decodeMessage } from '../protocol/decode'
import type { ProtocolMessage } from '../protocol/types'
import { CommandFacade } from './commandFacade'
import type { BlobClient } from './blobClient'
import type { RuntimeTransport } from './runtime'
import type { TransportCloseEvent, TransportReadyEvent } from './transport'
import { decodeProviderDiscoverResult, decodeModelCatalogSearchResult, encodeProviderTarget } from './providerCommandCodec'
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

const readModel = {
  provider: 'codex',
  model_profile: 'fast',
  model_id: 'gpt-5',
  reasoning_levels: ['low', 'medium'],
  default_reasoning_level: 'medium',
}

const readUsage = {
  user_id: 'user_1',
  account_id: 'account_1',
  email: 'user@example.test',
  plan_type: 'pro',
  rate_limit: {
    allowed: true,
    limit_reached: false,
    primary_window: { used_percent: 20, limit_window_seconds: 3600, reset_after_seconds: 1800, reset_at: 1_900_000_000 },
    secondary_window: null,
  },
  additional_rate_limits: null,
  credits: null,
}

const { additional_rate_limits: _unusedAdditionalRates, credits: _unusedCredits, ...decodedReadUsage } = readUsage
const nullableReadUsage = {
  ...readUsage,
  rate_limit: { allowed: true, limit_reached: false, primary_window: null, secondary_window: null },
}

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

  it('decodes model catalog inline results with exactly query+models', async () => {
    const inline = await decodeModelCatalogSearchResult({
      query: 'deepseek',
      models: [{ id: 'deepseek-v4-flash', name: 'DeepSeek V4 Flash', provider: 'deepseek', input: ['text'], output_limit: 384000 }],
    }, 'deepseek')
    expect(inline.query).toBe('deepseek')
    expect(inline.models).toHaveLength(1)
    expect(inline.models[0].id).toBe('deepseek-v4-flash')
    expect(inline.models[0].output_limit).toBe(384000)
  })

  it('decodes model catalog blob results and rejects the old null-blob shape', async () => {
    const blobClient = { getJSON: async () => [{ id: 'gemini-2.5-flash', name: 'Gemini', provider: 'google', input: ['text', 'image'] }] } as unknown as BlobClient
    const blob = await decodeModelCatalogSearchResult({ query: 'gemini', models: [], blob: blobDescriptor }, 'gemini', blobClient)
    expect(blob.models[0].id).toBe('gemini-2.5-flash')
    // The pre-fix backend emitted blob:null even for inline results. That
    // shape must be rejected rather than silently accepted.
    await expect(decodeModelCatalogSearchResult({ query: 'gemini', models: [], blob: null }, 'gemini', blobClient)).rejects.toThrow()
  })

  it('rejects model catalog results with a mismatched query or unknown shape', async () => {
    await expect(decodeModelCatalogSearchResult({ query: 'a', models: [] }, 'b')).rejects.toThrow()
    await expect(decodeModelCatalogSearchResult({ query: 'a', models: [], extra: 1 }, 'a')).rejects.toThrow()
    await expect(decodeModelCatalogSearchResult({ query: 'a' }, 'a')).rejects.toThrow()
  })

  it('reads project models and Codex usage through typed commands with target and blob validation', async () => {
    const transport = new ProviderCommandTransport()
    const blobClient = new RecordingBlobClient()
    const facade = new CommandFacade({ transport, blobClient: blobClient as unknown as BlobClient })

    const modelsPending = facade.readModels('project_1')
    const modelsCommand = transport.sent[0]
    if (modelsCommand.type !== 'command') throw new Error('wrong models command')
    expect(modelsCommand.payload.name).toBe('project.models.read')
    expect(modelsCommand.payload.schema_version).toBe(1)
    expect(modelsCommand.payload.arguments).toEqual({ project_id: 'project_1' })
    transport.emitResult(modelsCommand.payload.request_id, {
      project_id: 'project_1',
      models: [readModel],
      default_provider: 'codex',
      default_model: 'gpt-5',
      blob: null,
    })
    await expect(modelsPending).resolves.toEqual({
      project_id: 'project_1', models: [readModel], default_provider: 'codex', default_model: 'gpt-5',
    })

    const usagePending = facade.readCodexUsage('codex')
    const usageCommand = transport.sent[1]
    if (usageCommand.type !== 'command') throw new Error('wrong usage command')
    expect(usageCommand.payload.name).toBe('provider.codex_usage.read')
    expect(usageCommand.payload.schema_version).toBe(1)
    expect(usageCommand.payload.arguments).toEqual({ provider: 'codex' })
    transport.emitResult(usageCommand.payload.request_id, { provider: 'codex', usage: readUsage, blob: null })
    await expect(usagePending).resolves.toEqual({ provider: 'codex', usage: decodedReadUsage })

    const nullableUsagePending = facade.readCodexUsage('codex')
    const nullableUsageCommand = transport.sent[2]
    if (nullableUsageCommand.type !== 'command') throw new Error('wrong nullable usage command')
    transport.emitResult(nullableUsageCommand.payload.request_id, { provider: 'codex', usage: nullableReadUsage, blob: null })
    await expect(nullableUsagePending).resolves.toMatchObject({
      provider: 'codex',
      usage: { rate_limit: { allowed: true, limit_reached: false, primary_window: null, secondary_window: null } },
    })
    facade.stop()

    const blobTransport = new ProviderCommandTransport()
    const blobReader = new RecordingBlobClient()
    const blobFacade = new CommandFacade({ transport: blobTransport, blobClient: blobReader as unknown as BlobClient })
    const blobModels = blobFacade.readModels('project_1')
    const blobModelsCommand = blobTransport.sent[0]
    if (blobModelsCommand.type !== 'command') throw new Error('wrong blob models command')
    blobTransport.emitResult(blobModelsCommand.payload.request_id, {
      project_id: 'project_1', models: null, default_provider: 'codex', default_model: 'gpt-5', blob: blobDescriptor,
    })
    expect(blobReader.calls).toHaveLength(1)
    blobReader.calls[0].resolve([readModel])
    await expect(blobModels).resolves.toEqual({
      project_id: 'project_1', models: [readModel], default_provider: 'codex', default_model: 'gpt-5',
    })

    const blobUsage = blobFacade.readCodexUsage('codex')
    const blobUsageCommand = blobTransport.sent[1]
    if (blobUsageCommand.type !== 'command') throw new Error('wrong blob usage command')
    blobTransport.emitResult(blobUsageCommand.payload.request_id, { provider: 'codex', usage: null, blob: blobDescriptor })
    expect(blobReader.calls).toHaveLength(2)
    blobReader.calls[1].resolve(readUsage)
    await expect(blobUsage).resolves.toEqual({ provider: 'codex', usage: decodedReadUsage })
    blobFacade.stop()

    const mismatchTransport = new ProviderCommandTransport()
    const mismatchFacade = new CommandFacade({ transport: mismatchTransport })
    const mismatchProject = mismatchFacade.readModels('project_1')
    const mismatchProjectCommand = mismatchTransport.sent[0]
    if (mismatchProjectCommand.type !== 'command') throw new Error('wrong mismatch project command')
    mismatchTransport.emitResult(mismatchProjectCommand.payload.request_id, {
      project_id: 'project_2', models: [readModel], default_provider: 'codex', default_model: 'gpt-5', blob: null,
    })
    await expect(mismatchProject).rejects.toMatchObject({ code: 'invalid' })
    const extraUsage = mismatchFacade.readCodexUsage('codex')
    const extraUsageCommand = mismatchTransport.sent[1]
    if (extraUsageCommand.type !== 'command') throw new Error('wrong extra usage command')
    mismatchTransport.emitResult(extraUsageCommand.payload.request_id, { provider: 'codex', usage: readUsage, blob: null, secret: 'must-reject' })
    await expect(extraUsage).rejects.toMatchObject({ code: 'invalid' })
    mismatchFacade.stop()
  })

  it('retries typed read blobs across epochs and aborts the stale blob read', async () => {
    const transport = new ProviderCommandTransport()
    const blobClient = new RecordingBlobClient()
    const facade = new CommandFacade({ transport, blobClient: blobClient as unknown as BlobClient })
    const pending = facade.readCodexUsage('codex')
    const first = transport.sent[0]
    if (first.type !== 'command') throw new Error('wrong first usage command')
    transport.emitResult(first.payload.request_id, { provider: 'codex', usage: null, blob: blobDescriptor })
    transport.connectionGeneration = 2
    transport.emitReady('epoch_2', 'epoch_1')
    const retry = transport.sent[1]
    if (retry.type !== 'command') throw new Error('wrong retried usage command')
    transport.emitResult(retry.payload.request_id, { provider: 'codex', usage: null, blob: blobDescriptor }, 2)
    expect(blobClient.calls).toHaveLength(2)
    expect(blobClient.calls[0].signal?.aborted).toBe(true)
    blobClient.calls[0].resolve({ ...readUsage, user_id: 'stale' })
    blobClient.calls[1].resolve(readUsage)
    await expect(pending).resolves.toEqual({ provider: 'codex', usage: decodedReadUsage })
    facade.stop()
  })

  it('rejects oversized read Blob descriptors before fetch and oversized decoded payloads', async () => {
    const oversizedDescriptor = { ...blobDescriptor, size: 8 * 1024 * 1024 + 1 }
    const descriptorTransport = new ProviderCommandTransport()
    const descriptorBlobClient = new RecordingBlobClient()
    const descriptorFacade = new CommandFacade({ transport: descriptorTransport, blobClient: descriptorBlobClient as unknown as BlobClient })

    const projectPending = descriptorFacade.readModels('project_1')
    const projectCommand = descriptorTransport.sent[0]
    if (projectCommand.type !== 'command') throw new Error('wrong oversized project command')
    descriptorTransport.emitResult(projectCommand.payload.request_id, {
      project_id: 'project_1', models: null, default_provider: 'codex', default_model: 'gpt-5', blob: oversizedDescriptor,
    })
    await expect(projectPending).rejects.toMatchObject({ code: 'invalid' })
    expect(descriptorBlobClient.calls).toHaveLength(0)

    const usagePending = descriptorFacade.readCodexUsage('codex')
    const usageCommand = descriptorTransport.sent[1]
    if (usageCommand.type !== 'command') throw new Error('wrong oversized usage command')
    descriptorTransport.emitResult(usageCommand.payload.request_id, { provider: 'codex', usage: null, blob: oversizedDescriptor })
    await expect(usagePending).rejects.toMatchObject({ code: 'invalid' })
    expect(descriptorBlobClient.calls).toHaveLength(0)
    descriptorFacade.stop()

    const payloadTransport = new ProviderCommandTransport()
    const payloadBlobClient = new RecordingBlobClient()
    const payloadFacade = new CommandFacade({ transport: payloadTransport, blobClient: payloadBlobClient as unknown as BlobClient })
    const projectPayloadPending = payloadFacade.readModels('project_1')
    const projectPayloadCommand = payloadTransport.sent[0]
    if (projectPayloadCommand.type !== 'command') throw new Error('wrong oversized project payload command')
    payloadTransport.emitResult(projectPayloadCommand.payload.request_id, { project_id: 'project_1', models: null, default_provider: 'codex', default_model: 'gpt-5', blob: blobDescriptor })
    payloadBlobClient.calls[0].resolve([{ ...readModel, ignored: 'x'.repeat(8 * 1024 * 1024) }])
    await expect(projectPayloadPending).rejects.toMatchObject({ code: 'invalid' })

    const usagePayloadPending = payloadFacade.readCodexUsage('codex')
    const usagePayloadCommand = payloadTransport.sent[1]
    if (usagePayloadCommand.type !== 'command') throw new Error('wrong oversized usage payload command')
    payloadTransport.emitResult(usagePayloadCommand.payload.request_id, { provider: 'codex', usage: null, blob: blobDescriptor })
    payloadBlobClient.calls[1].resolve({ ...readUsage, ignored: 'x'.repeat(8 * 1024 * 1024) })
    await expect(usagePayloadPending).rejects.toMatchObject({ code: 'invalid' })
    payloadFacade.stop()
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
