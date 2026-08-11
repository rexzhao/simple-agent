import { describe, expect, it } from 'vitest'
import type { RunStep } from '../types'
import { flattenProcessSteps } from './runSteps'

describe('flattenProcessSteps', () => {
  it('keeps interleaved tool and reasoning steps in provider order', () => {
    const steps: RunStep[] = [
      { kind: 'tool', id: 'one', name: 'read', iteration: 1, status: 'finished' },
      { kind: 'reasoning', id: 'reason-one', text: 'between', iteration: 1 },
      { kind: 'tool', id: 'two', name: 'write', iteration: 2, status: 'running' },
      { kind: 'reasoning', id: 'reason-two', text: 'after', iteration: 1 },
    ]

    const flat = flattenProcessSteps(steps)
    expect(flat.map((entry) => entry.step.id)).toEqual(['one', 'reason-one', 'two', 'reason-two'])
    expect(flat.map((entry) => entry.iterationStart)).toEqual([true, false, true, true])
  })
})
