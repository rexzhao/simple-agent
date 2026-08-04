import { describe, expect, it } from 'vitest'
import type { Session } from '../types'
import { buildSessionTree } from './session'
import { reduceLifecycleEvent, type SessionMaps } from './lifecycleReducer'

function session(id: string, options: Partial<Session> = {}): Session {
  return {
    id,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    display_name: id,
    created_by: 'agent',
    root_session_id: 'root',
    spawn_depth: 1,
    archived: false,
    last_used_at: '2026-01-01T00:00:00Z',
    provider: 'fake',
    model_profile: 'default',
    model_id: 'fake-model',
    project_id: 'project-1',
    created_cwd: '/workspace',
    last_seq: 0,
    full_access: false,
    ...options,
  }
}

const initial: SessionMaps = {
  active: { 'project-1': [session('root', { created_by: 'user', root_session_id: 'root', spawn_depth: 0 })] },
  archived: { 'project-1': [] },
}

describe('reduceLifecycleEvent', () => {
  it('puts an agent-created child in its project tree immediately', () => {
    const child = session('child', { parent_session_id: 'root', root_session_id: 'root' })
    const next = reduceLifecycleEvent(initial, { type: 'session.created', session: child })

    const tree = buildSessionTree(next.active['project-1'])
    expect(tree).toHaveLength(1)
    expect(tree[0].children.map((node) => node.session.id)).toEqual(['child'])
  })

  it('moves archived and restored sessions between maps and removes cascades', () => {
    const child = session('child', { parent_session_id: 'root', root_session_id: 'root' })
    const withChild = reduceLifecycleEvent(initial, { type: 'session.created', session: child })
    const archived = reduceLifecycleEvent(withChild, {
      type: 'session.archived',
      session: { ...child, archived: true },
    })
    expect(archived.active['project-1']).toHaveLength(1)
    expect(archived.archived['project-1'].map((item) => item.id)).toEqual(['child'])

    const restored = reduceLifecycleEvent(archived, {
      type: 'session.updated',
      session: { ...child, archived: false, updated_at: '2026-01-02T00:00:00Z' },
    })
    expect(restored.active['project-1'].map((item) => item.id)).toContain('child')
    const deleted = reduceLifecycleEvent(restored, {
      type: 'session.deleted',
      session: 'root',
      session_id: 'root',
      descendants: ['child'],
      project_id: 'project-1',
    })
    expect(deleted.active['project-1']).toEqual([])
    expect(deleted.archived['project-1']).toEqual([])
  })
})

