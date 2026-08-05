import type { RunEvent, RunStep, ToolActivity } from '../types'

export function appendReasoning(steps: RunStep[], text: string, iteration: number, turnID = '', itemID = ''): RunStep[] {
	if (!text) return steps
	const normalizedIteration = normalizedAgentIteration(iteration)
	const last = steps[steps.length - 1]
	const normalizedTurnID = turnID.trim()
	const normalizedItemID = itemID.trim()
	if (last?.kind === 'reasoning' && last.iteration === normalizedIteration &&
		(last.turnID ?? '') === normalizedTurnID && (last.itemID ?? '') === normalizedItemID) {
		return [...steps.slice(0, -1), { ...last, text: last.text + text }]
	}
	return [...steps, {
		kind: 'reasoning',
		id: `reasoning-${normalizedTurnID || 'legacy'}-${normalizedItemID || 'unbound'}-${normalizedIteration}-${steps.length}`,
		text,
		iteration: normalizedIteration,
		...(normalizedTurnID ? { turnID: normalizedTurnID } : {}),
		...(normalizedItemID ? { itemID: normalizedItemID } : {}),
	}]
}

export function appendModelOutput(steps: RunStep[], text: string, iteration: number): RunStep[] {
	if (!text) return steps
	const normalizedIteration = normalizedAgentIteration(iteration)
	return [...steps, { kind: 'output', id: `output-${normalizedIteration}-${steps.length}`, text, iteration: normalizedIteration }]
}

export function updateToolStep(steps: RunStep[], event: RunEvent, iteration: number): RunStep[] {
	const fields = event as Record<string, unknown>
	const id = String(fields.tool_call_id ?? '')
	const name = String(fields.name ?? 'tool')
	const status: ToolActivity['status'] = event.type === 'tool.requested'
		? 'requested'
		: event.type === 'tool.started'
			? 'running'
			: Boolean(fields.is_error) ? 'error' : 'finished'
	const index = steps.findIndex((step) => step.kind === 'tool' && step.id === id)
	const current = index >= 0 ? steps[index] as ToolActivity : null
	const tool: ToolActivity = {
		kind: 'tool',
		id: id || `${name}-${steps.length}`,
		name,
		iteration: current?.iteration ?? normalizedAgentIteration(iteration),
		arguments: String(fields.arguments ?? current?.arguments ?? '') || undefined,
		result: String(fields.content ?? current?.result ?? '') || undefined,
		status,
	}
	if (index < 0) return [...steps, tool]
	return steps.map((step, stepIndex) => stepIndex === index ? tool : step)
}

export function groupProcessSteps(steps: RunStep[]): Array<{ number: number; steps: RunStep[] }> {
	const groups = new Map<number, RunStep[]>()
	for (const step of steps) {
		const iteration = normalizedAgentIteration(step.iteration)
		groups.set(iteration, [...(groups.get(iteration) ?? []), step])
	}
	const rank = (step: RunStep) => step.kind === 'reasoning' ? 0 : step.kind === 'output' ? 1 : 2
	return [...groups.entries()]
		.sort(([left], [right]) => left - right)
		.map(([number, turnSteps]) => ({ number, steps: [...turnSteps].sort((left, right) => rank(left) - rank(right)) }))
}

export interface FlatProcessStep {
	step: RunStep
	iteration: number
	iterationStart: boolean
}

export type ProcessDisplayNode =
	| { kind: 'step'; flat: FlatProcessStep }
	| { kind: 'tool-group'; id: string; flats: FlatProcessStep[] }

// foldToolGroups collapses maximal runs of consecutive tool steps into one
// display group, across agent iterations. Reasoning counts as part of a tool
// run and never breaks it; assistant output does. A run folds when it holds
// at least one tool call and two steps —
// shorter runs render as individual rows.
export function foldToolGroups(steps: RunStep[]): ProcessDisplayNode[] {
	const flats: FlatProcessStep[] = []
	for (const iteration of groupProcessSteps(steps)) {
		iteration.steps.forEach((step, index) => {
			flats.push({ step, iteration: iteration.number, iterationStart: index === 0 })
		})
	}
	const nodes: ProcessDisplayNode[] = []
	let run: FlatProcessStep[] = []
	const flush = () => {
		const toolCount = run.filter((flat) => flat.step.kind === 'tool').length
		if (run.length >= 2 && toolCount >= 1) {
			nodes.push({ kind: 'tool-group', id: `tool-group-${run[0].step.id}`, flats: run })
		} else {
			for (const flat of run) nodes.push({ kind: 'step', flat })
		}
		run = []
	}
	for (const flat of flats) {
		if (flat.step.kind === 'tool' || flat.step.kind === 'reasoning') {
			run.push(flat)
			continue
		}
		flush()
		nodes.push({ kind: 'step', flat })
	}
	flush()
	return nodes
}

function normalizedAgentIteration(iteration: number): number {
	return Number.isFinite(iteration) && iteration > 0 ? iteration : 1
}
