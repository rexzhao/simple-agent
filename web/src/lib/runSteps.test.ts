import { describe, expect, it } from 'vitest'
import type { RunStep } from '../types'
import { foldToolGroups } from './runSteps'

describe('foldToolGroups', () => {
  it('merges tool activity across agent iterations', () => {
    const steps: RunStep[] = [
      { kind: 'tool', id: 'one', name: 'read', iteration: 1, status: 'finished' },
      { kind: 'reasoning', id: 'reason', text: 'next', iteration: 2 },
      { kind: 'tool', id: 'two', name: 'write', iteration: 2, status: 'running' },
    ]

    const nodes = foldToolGroups(steps)
    expect(nodes).toHaveLength(1)
    expect(nodes[0]).toMatchObject({ kind: 'tool-group' })
    if (nodes[0].kind === 'tool-group') {
      expect(nodes[0].flats.map((flat) => flat.step.id)).toEqual(['one', 'reason', 'two'])
    }
  })
})
