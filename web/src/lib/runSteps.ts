import type { RunStep } from '../types'

export interface FlatProcessStep {
	step: RunStep
	iteration: number
	iterationStart: boolean
}

/** Adds display metadata without changing the provider/event order. */
export function flattenProcessSteps(steps: RunStep[]): FlatProcessStep[] {
	let previousIteration: number | undefined
	return steps.map((step) => {
		const iteration = normalizedAgentIteration(step.iteration)
		const flat = {
			step,
			iteration,
			iterationStart: previousIteration !== iteration,
		}
		previousIteration = iteration
		return flat
	})
}

function normalizedAgentIteration(iteration: number): number {
	return Number.isFinite(iteration) && iteration > 0 ? iteration : 1
}
