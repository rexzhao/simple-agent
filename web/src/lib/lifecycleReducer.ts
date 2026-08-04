import type { LifecycleEvent, Session } from '../types'
import { orderSessions } from './session'

export interface SessionMaps {
  active: Record<string, Session[]>
  archived: Record<string, Session[]>
}

function sessionFromEvent(event: LifecycleEvent): Session | null {
  if (event.session && typeof event.session === 'object') return event.session
  if (event.metadata) return event.metadata
  if (event.session_metadata) return event.session_metadata
  return null
}

function removeSession(maps: SessionMaps, sessionID: string): SessionMaps {
  if (!sessionID) return maps
  const active = Object.fromEntries(Object.entries(maps.active).map(([projectID, sessions]) => [
    projectID,
    sessions.filter((session) => session.id !== sessionID),
  ]))
  const archived = Object.fromEntries(Object.entries(maps.archived).map(([projectID, sessions]) => [
    projectID,
    sessions.filter((session) => session.id !== sessionID),
  ]))
  return { active, archived }
}

function upsertSession(maps: SessionMaps, session: Session, archived: boolean): SessionMaps {
  const withoutSession = removeSession(maps, session.id)
  const projectID = session.project_id
  if (!projectID) return withoutSession
  const target = archived ? withoutSession.archived : withoutSession.active
  const nextTarget = {
    ...target,
    [projectID]: orderSessions([...(target[projectID] ?? []), { ...session, archived }]),
  }
  return archived
    ? { active: withoutSession.active, archived: nextTarget }
    : { active: nextTarget, archived: withoutSession.archived }
}

/**
 * Applies one durable lifecycle notification to the sidebar session maps.
 * The reducer deliberately knows nothing about React or network state, so a
 * reconnect bootstrap and an event stream can share the same tree semantics.
 */
export function reduceLifecycleEvent(maps: SessionMaps, event: LifecycleEvent): SessionMaps {
  const session = sessionFromEvent(event)
  switch (event.type) {
    case 'session.created':
    case 'session.updated':
    case 'run.settled':
      return session ? upsertSession(maps, session, session.archived) : maps
    case 'session.archived':
      if (!session) return maps
      return (event.descendants ?? []).reduce((current, sessionID) => {
        const descendant = [...(maps.active[session.project_id] ?? []), ...(maps.archived[session.project_id] ?? [])]
          .find((item) => item.id === sessionID)
        return descendant ? upsertSession(current, descendant, true) : current
      }, upsertSession(maps, session, true))
    case 'session.deleted': {
      const ids = [
        typeof event.session === 'string' ? event.session : '',
        event.session_id ?? '',
        ...(event.descendants ?? []),
      ]
      return ids.reduce(removeSession, maps)
    }
    default:
      return maps
  }
}

