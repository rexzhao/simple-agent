import type { ActiveRun, QueuedPrompt, RunEvent } from '../types'
import { appendModelOutput, appendReasoning, updateToolStep } from './runSteps'

/** Applies events that only change the in-memory representation of a run.
 * Events requiring I/O (resync and settled) remain orchestrated by App.
 */
export function reduceRunEvent(run: ActiveRun, event: RunEvent): ActiveRun {
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
    case 'provider.retrying':
      return {
        ...run,
        providerRetry: {
          attempt: Number(event.attempt ?? 0),
          maxAttempts: Number(event.max_attempts ?? 0),
          delayMS: Number(event.delay_ms ?? 0),
        },
      }
    case 'run.prompt_queue':
      return {
        ...run,
        queuedPrompts: Array.isArray(event.prompts)
          ? event.prompts
              .map((prompt) => (prompt && typeof prompt === 'object'
                ? { id: String((prompt as QueuedPrompt).id ?? ''), content: String((prompt as QueuedPrompt).content ?? '') }
                : null))
              .filter((prompt): prompt is QueuedPrompt => Boolean(prompt?.id))
          : [],
      }
    case 'run.prompt_appended': { // A drained prompt becomes part of the visible run.
      const prompts = Array.isArray(event.prompts) ? event.prompts.map(String).filter((text) => text.trim()) : []
      if (prompts.length === 0) return run
      const iteration = run.agentIteration > 0 ? run.agentIteration : 1
      const turnID = String(event.turn_id ?? run.turnID ?? '') || undefined
      const steps = [...run.steps]
      prompts.forEach((text, index) => {
        steps.push({ kind: 'user', id: `appended-${run.id}-${steps.length}-${index}`, text, iteration })
      })
      return { ...run, turnID, steps }
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
    case 'text.delta':
      return { ...run, assistantText: run.assistantText + String(event.text ?? '') }
    case 'reasoning.delta':
      return { ...run, steps: appendReasoning(run.steps, String(event.text ?? ''), Number(event.agent_iteration ?? run.agentIteration)) }
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
    case 'usage.updated':
      return {
        ...run,
        inputTokens: Number(event.input_tokens ?? 0),
        totalTokens: Number(event.total_tokens ?? 0),
        cachedTokens: Number(event.cached_tokens ?? 0),
        cacheWriteTokens: Number(event.cache_write_tokens ?? 0),
        reasoningTokens: Number(event.reasoning_tokens ?? 0),
      }
    case 'turn.failed':
      return { ...run, status: 'failed', error: String(event.message ?? 'Run failed') }
    default:
      return run
  }
}
