import { describe, expect, it } from 'vitest'
import type { Session } from '../types'
import { buildSessionTree, flattenSessionTree } from './session'

function session(id: string, options: Partial<Session> = {}): Session {
  return {
    id,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    display_name: id,
    created_by: 'user',
    root_session_id: id,
    spawn_depth: 0,
    archived: false,
    last_used_at: '2026-01-01T00:00:00Z',
    provider: 'fake',
    model_profile: 'default',
    model_id: 'fake-model',
    project_id: 'project-1',
    created_cwd: '/workspace',
    last_seq: 0,
    ...options,
  }
}

describe('buildSessionTree', () => {
  it('nests descendants and lifts a root by descendant activity', () => {
    const olderRoot = session('root-a')
    const child = session('child-a', {
      created_by: 'agent',
      parent_session_id: olderRoot.id,
      root_session_id: olderRoot.id,
      spawn_depth: 1,
      last_used_at: '2026-01-04T00:00:00Z',
    })
    const grandchild = session('grandchild-a', {
      created_by: 'agent',
      parent_session_id: child.id,
      root_session_id: olderRoot.id,
      spawn_depth: 2,
      last_used_at: '2026-01-03T00:00:00Z',
    })
    const newerRoot = session('root-b', { last_used_at: '2026-01-02T00:00:00Z' })

    const roots = buildSessionTree([newerRoot, grandchild, olderRoot, child])
    expect(roots.map((node) => node.session.id)).toEqual(['root-a', 'root-b'])
    expect(roots[0].children.map((node) => node.session.id)).toEqual(['child-a'])
    expect(roots[0].children[0].children.map((node) => node.session.id)).toEqual(['grandchild-a'])
    expect(flattenSessionTree(roots).map((item) => item.id)).toEqual(['root-a', 'child-a', 'grandchild-a', 'root-b'])
  })

  it('keeps missing parents and cycles visible as orphan roots', () => {
    const missing = session('missing-child', {
      created_by: 'agent', parent_session_id: 'archived-parent', root_session_id: 'archived-parent', spawn_depth: 1,
    })
    const cycleA = session('cycle-a', { parent_session_id: 'cycle-b', root_session_id: 'cycle-a' })
    const cycleB = session('cycle-b', { parent_session_id: 'cycle-a', root_session_id: 'cycle-a' })

    const roots = buildSessionTree([missing, cycleA, cycleB])
    expect(new Set(roots.map((node) => node.session.id))).toEqual(new Set(['missing-child', 'cycle-a', 'cycle-b']))
    expect(roots.every((node) => node.orphaned)).toBe(true)
  })
})
