import type { ActiveRun, QueuedPrompt, RunEvent, SessionItemProjectionEvent } from '../types'
import { frontendProtocolLogger, protocolLogIdentity } from './frontendProtocolLogger'
import { appendModelOutput, appendReasoning, updateToolStep } from './runSteps'

/** Applies events that only change the in-memory representation of a run.
 * Events requiring I/O (resync and settled) remain orchestrated by App.
 */
export function reduceRunEvent(run: ActiveRun, event: RunEvent): ActiveRun {
  const next = reduceRunEventInternal(run, event)
  if (frontendProtocolLogger.isEnabled(run.sessionID)) {
    frontendProtocolLogger.log({
      sessionID: run.sessionID,
      source: 'run.reducer',
      kind: 'reduce',
      ...protocolLogIdentity(event as unknown as Record<string, unknown>, run.id),
      before: run,
      after: next,
    })
  }
  return next
}

function reduceRunEventInternal(run: ActiveRun, event: RunEvent): ActiveRun {
  if (run.providerRetry && ['text.delta', 'reasoning.delta', 'tool.requested', 'tool.started', 'tool.finished', 'usage.updated'].includes(event.type)) {
    run = { ...run, providerRetry: undefined }
  }
  switch (event.type) {
    case 'turn.started':
      return { ...run, turnID: String(event.turn_id ?? '') }
    case 'compaction.started':
      return {
        ...run,
        compaction: {
          trigger: event.trigger === 'manual' ? 'manual' : 'auto',
          status: 'running',
        },
      }
    case 'compaction.completed':
      return {
        ...run,
        compaction: {
          trigger: event.trigger === 'manual' ? 'manual' : 'auto',
          status: 'completed',
          activeContextTokens: Number(event.active_context_tokens ?? 0) || undefined,
          contextWindow: Number(event.context_window ?? 0) || undefined,
        },
      }
    case 'provider.retrying': {
      const agentIteration = Number(event.agent_iteration ?? run.agentIteration)
      const usageEvents = run.usageEvents?.filter((usage) => usage.agentIteration !== agentIteration)
      return {
        ...run,
        ...(usageEvents ? { usageEvents, usage: sumUsageEvents(usageEvents) } : {}),
        providerRetry: {
          attempt: Number(event.attempt ?? 0),
          maxAttempts: Number(event.max_attempts ?? 0),
          delayMS: Number(event.delay_ms ?? 0),
        },
      }
    }
    case 'run.prompt_queue':
      return {
        ...run,
        queuedPrompts: Array.isArray(event.prompts)
          ? event.prompts
              .map((prompt): QueuedPrompt | null => (prompt && typeof prompt === 'object'
                ? {
                    id: String((prompt as QueuedPrompt).id ?? ''),
                    content: String((prompt as QueuedPrompt).content ?? ''),
                    steer: Boolean((prompt as QueuedPrompt).steer),
                  }
                : null))
              .filter((prompt): prompt is QueuedPrompt => Boolean(prompt?.id))
          : [],
      }
    case 'run.prompt_appended': {
      // Prompt admission/draining is not a conversation projection. The
      // committed user item will arrive through the projection event stream.
      const turnID = typeof event.turn_id === 'string' ? event.turn_id : ''
      const prompts = Array.isArray(event.prompts)
        ? event.prompts.map(String).filter((prompt) => prompt.trim())
        : []
      if (!turnID && prompts.length === 0) return run
      const boundaryID = `prompt-boundary-${run.id}-${(run.processBoundaries?.length ?? 0) + 1}`
      return {
        ...run,
        ...(turnID && turnID !== run.turnID ? { turnID } : {}),
        processBoundaries: [
          ...(run.processBoundaries ?? []),
          { id: boundaryID, stepIndex: run.steps.length },
        ],
      }
    }
    case 'agent.iteration.started': {
      const agentIteration = Number(event.agent_iteration ?? 0)
      if (agentIteration <= 0) return run
      return {
        ...run,
        agentIteration,
        assistantText: '',
        steps: appendModelOutput(run.steps, run.assistantText, run.agentIteration),
      }
    }
    case 'text.delta': {
      const delta = event as {
        text?: unknown
        item_id?: string
        turn_id?: string
        agent_iteration?: number
        durable_text_length?: number
        durable_checkpointed?: boolean
      }
      const text = String(delta.text ?? '')
      if (!delta.item_id) return { ...run, assistantText: run.assistantText + text }
      const key = assistantOutputKey(delta.turn_id ?? '', Number(delta.agent_iteration ?? run.agentIteration))
      const previous = run.assistantItems?.[key]
      const sameItem = previous?.itemID === delta.item_id
      const eventLength = Number(delta.durable_text_length)
      const durableTextLength = Number.isFinite(eventLength) && eventLength >= 0
        ? eventLength
        : (previous?.durableTextLength ?? 0)
      const assistantItems = {
        ...(run.assistantItems ?? {}),
        [key]: {
          itemID: delta.item_id,
          durableTextLength: sameItem ? Math.max(previous?.durableTextLength ?? 0, durableTextLength) : durableTextLength,
        },
      }
      if (key !== assistantOutputKey(run.turnID ?? '', run.agentIteration)) {
        return { ...run, assistantItems }
      }
      // A checkpoint is committed before its corresponding transient delta is
      // fanned out. It is therefore already represented by the durable row;
      // only the uncheckpointed tail belongs in ActiveRun.
      return {
        ...run,
        assistantItems,
        assistantText: delta.durable_checkpointed ? '' : run.assistantText + text,
      }
    }
    case 'reasoning.delta':
      return {
        ...run,
        steps: appendReasoning(
          run.steps,
          String(event.text ?? ''),
          Number(event.agent_iteration ?? run.agentIteration),
          String(event.turn_id ?? run.turnID ?? ''),
          String(event.item_id ?? ''),
        ),
      }
    case 'tool.requested':
      return {
        ...run,
        assistantText: '',
        steps: updateToolStep(
          appendModelOutput(run.steps, run.assistantText, Number(event.agent_iteration ?? run.agentIteration)),
          event,
          Number(event.agent_iteration ?? run.agentIteration),
        ),
      }
    case 'tool.started':
    case 'tool.finished':
      return { ...run, steps: updateToolStep(run.steps, event, Number(event.agent_iteration ?? run.agentIteration)) }
    case 'usage.updated': {
      const usage = {
        inputTokens: Number(event.input_tokens ?? 0),
        outputTokens: Number(event.output_tokens ?? 0),
        totalTokens: Number(event.total_tokens ?? 0),
        cachedTokens: Number(event.cached_tokens ?? 0),
        cacheWriteTokens: Number(event.cache_write_tokens ?? 0),
        reasoningTokens: Number(event.reasoning_tokens ?? 0),
      }
      const agentIteration = Number(event.agent_iteration ?? run.agentIteration)
      const usageEvents = [
        ...(run.usageEvents ?? []).filter((previous) => previous.agentIteration !== agentIteration),
        { ...usage, agentIteration },
      ]
      return {
        ...run,
        // Keep the latest request at the top level for the context-window
        // meter; cost reporting uses the additive turn usage below.
        inputTokens: usage.inputTokens,
        outputTokens: usage.outputTokens,
        totalTokens: usage.totalTokens,
        cachedTokens: usage.cachedTokens,
        cacheWriteTokens: usage.cacheWriteTokens,
        reasoningTokens: usage.reasoningTokens,
        usage: sumUsageEvents(usageEvents),
        usageEvents,
      }
    }
    case 'item.appended':
    case 'item.created':
    case 'item.updated': {
      const projection = event as SessionItemProjectionEvent
      const item = projection.item
      if (item.message?.role !== 'assistant') return run
      const turnID = projection.turn_id ?? item.turn_id ?? run.turnID ?? ''
      const iteration = Number(item.agent_iteration ?? 0)
      const key = assistantOutputKey(turnID, iteration)
      const previous = run.assistantItems?.[key]
      const length = Number(projection.assistant_text_length)
      const sameItem = previous?.itemID === projection.item_id
      const durableTextLength = Number.isFinite(length) && length >= 0
        ? (sameItem ? Math.max(previous?.durableTextLength ?? 0, length) : length)
        : (previous?.durableTextLength ?? 0)
      const assistantItems = {
        ...(run.assistantItems ?? {}),
        [key]: {
          itemID: projection.item_id,
          durableTextLength,
        },
      }
      return {
        ...run,
        assistantItems,
        ...(key === assistantOutputKey(run.turnID ?? '', run.agentIteration) ? { assistantText: '' } : {}),
      }
    }
    case 'turn.failed':
      // Terminal failure: drop any pending retry notice so it does not
      // linger next to the Turn failed banner.
      return { ...run, status: 'failed', error: String(event.message ?? 'Run failed'), providerRetry: undefined }
    default:
      return run
  }
}

function assistantOutputKey(turnID: string, agentIteration: number): string {
  return `${turnID}:${agentIteration}`
}

function sumUsageEvents(events: NonNullable<ActiveRun['usageEvents']>): NonNullable<ActiveRun['usage']> | undefined {
  if (events.length === 0) return undefined
  return events.reduce((total, event) => ({
    inputTokens: total.inputTokens + event.inputTokens,
    outputTokens: total.outputTokens + event.outputTokens,
    totalTokens: total.totalTokens + event.totalTokens,
    cachedTokens: total.cachedTokens + event.cachedTokens,
    cacheWriteTokens: total.cacheWriteTokens + event.cacheWriteTokens,
    reasoningTokens: total.reasoningTokens + event.reasoningTokens,
  }), {
    inputTokens: 0,
    outputTokens: 0,
    totalTokens: 0,
    cachedTokens: 0,
    cacheWriteTokens: 0,
    reasoningTokens: 0,
  })
}
