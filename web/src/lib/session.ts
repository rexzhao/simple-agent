import type { Project, SessionItem, SessionModelOption } from '../types'
import type { SessionSummary } from '../repositories/sessionIndex'

/** The smallest navigation shape shared by the Session Index and legacy
 * session-content DTOs.  Navigation must not require the content snapshot's
 * provider/history fields. */
export interface SessionNavigation {
  id: string
  project_id: string
  display_name: string
  parent_session_id?: string | null
  archived: boolean
  status?: string
  updated_at: string
  created_at?: string
  last_used_at?: string
  has_unread_result?: boolean
}

/** Convert an index summary to the small shape used by the tree helpers.  It
 * is intentionally a lossy view: provider/model/content fields remain owned
 * by the opened-session snapshot. */
export function navigationSession(summary: SessionSummary): SessionNavigation {
  return {
    id: summary.session_id,
    project_id: summary.project_id,
    display_name: summary.display_name,
    parent_session_id: summary.parent_session_id,
    archived: summary.archived,
    status: summary.status,
    updated_at: summary.updated_at,
    last_used_at: summary.updated_at,
    has_unread_result: summary.has_unread_result,
  }
}

export function modelKey(model?: SessionModelOption): string {
  return model ? `${model.provider}/${model.model_profile}` : ''
}

export function reasoningLevelLabel(level: string): string {
  return { off: 'Off', minimal: 'Minimal', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra high', max: 'Max' }[level] ?? level
}

export function projectName(project: Project): string {
  if (project.display_name) return project.display_name
  return project.root.split(/[\\/]/).filter(Boolean).pop() || project.root
}

export function sessionName(session: Pick<SessionNavigation, 'id' | 'display_name'>): string {
  return session.display_name || `Session ${session.id.slice(-6)}`
}

function sessionTimestamp(session: SessionNavigation): number {
  return new Date(session.last_used_at || session.updated_at || session.created_at || '').getTime()
}

export function orderSessions<T extends SessionNavigation>(sessions: readonly T[]): T[] {
  return [...sessions].sort((a, b) => sessionTimestamp(b) - sessionTimestamp(a))
}

export interface SessionTreeNode<T extends SessionNavigation = SessionNavigation> {
  session: T
  children: SessionTreeNode<T>[]
  /** The session names a parent that is unavailable or would form a cycle. */
  orphaned: boolean
}

/**
 * Builds a deterministic project-local session forest. Missing/archived
 * parents become explicit roots instead of hiding their visible descendants,
 * and corrupted cycles are broken into orphan roots. A descendant's recent
 * activity lifts its whole root branch in the ordering.
 */
export function buildSessionTree<T extends SessionNavigation>(sessions: readonly T[]): SessionTreeNode<T>[] {
  const ordered = orderSessions(sessions)
  const sessionsByID = new Map(ordered.map((session) => [session.id, session]))
  const nodesByID = new Map<string, SessionTreeNode<T>>(ordered.map((session) => [session.id, {
    session,
    children: [],
    orphaned: false,
  }]))
  const roots: SessionTreeNode<T>[] = []

  const parentWouldCycle = (session: T): boolean => {
    const seen = new Set([session.id])
    let parentID = session.parent_session_id
    while (parentID) {
      if (seen.has(parentID)) return true
      seen.add(parentID)
      parentID = sessionsByID.get(parentID)?.parent_session_id
    }
    return false
  }

  for (const session of ordered) {
    const node = nodesByID.get(session.id)!
    const parentID = session.parent_session_id
    const parent = parentID ? nodesByID.get(parentID) : undefined
    if (parent && !parentWouldCycle(session)) {
      parent.children.push(node)
      continue
    }
    node.orphaned = Boolean(parentID)
    roots.push(node)
  }

  const branchTimestamp = (node: SessionTreeNode<T>): number => Math.max(
    sessionTimestamp(node.session),
    ...node.children.map(branchTimestamp),
  )
  const sortBranch = (nodes: SessionTreeNode<T>[]) => {
    for (const node of nodes) sortBranch(node.children)
    nodes.sort((a, b) => branchTimestamp(b) - branchTimestamp(a) || a.session.id.localeCompare(b.session.id))
  }
  sortBranch(roots)
  return roots
}

export function flattenSessionTree<T extends SessionNavigation>(nodes: SessionTreeNode<T>[]): T[] {
  return nodes.flatMap((node) => [node.session, ...flattenSessionTree(node.children)])
}

export interface SessionSubPanelContext<T extends SessionNavigation = SessionNavigation> {
  /** The parent session shown at the top of the sub-panel. */
  parent: T
  /** Direct children sorted newest-first by creation time. */
  children: T[]
}

/**
 * Resolves the sub-panel context for the currently selected session.
 *
 * If the selected session has children, it is the parent. If it is itself a
 * child, its parent is the parent and its siblings (including itself) are the
 * children. Returns null when the selected session has no parent and no
 * children, meaning there is nothing to show in the sub-panel.
 */
export function sessionSubPanelContext<T extends SessionNavigation>(sessions: readonly T[], selectedID: string): SessionSubPanelContext<T> | null {
  const selected = sessions.find((s) => s.id === selectedID)
  if (!selected) return null
  const parentID = selected.parent_session_id
  const parent = parentID ? sessions.find((s) => s.id === parentID) : undefined
  const effectiveParent = parent ?? selected
  const children = sessions
    .filter((s) => s.parent_session_id === effectiveParent.id)
    .sort((a, b) => new Date(b.created_at || b.updated_at).getTime() - new Date(a.created_at || a.updated_at).getTime())
  if (children.length === 0) return null
  return { parent: effectiveParent, children }
}

/**
 * Returns the ids of every descendant of rootID inside sessions, following
 * parent links breadth-first. Cycle-safe: corrupted lineage can never loop.
 * The backend archives/removes a whole subtree together, so this count lets
 * confirmation dialogs spell out the cascade.
 */
export function sessionDescendantIDs<T extends SessionNavigation>(sessions: readonly T[], rootID: string): string[] {
  const childrenByParent = new Map<string, string[]>()
  for (const session of sessions) {
    if (!session.parent_session_id) continue
    const siblings = childrenByParent.get(session.parent_session_id) ?? []
    siblings.push(session.id)
    childrenByParent.set(session.parent_session_id, siblings)
  }
  const descendants: string[] = []
  const seen = new Set([rootID])
  const queue = [rootID]
  while (queue.length > 0) {
    const parent = queue.shift()!
    for (const child of childrenByParent.get(parent) ?? []) {
      if (seen.has(child)) continue
      seen.add(child)
      descendants.push(child)
      queue.push(child)
    }
  }
  return descendants
}

export function sessionTreeContains(node: SessionTreeNode, sessionIDs: ReadonlySet<string>): boolean {
  return sessionIDs.has(node.session.id) || node.children.some((child) => sessionTreeContains(child, sessionIDs))
}

export function itemText(item: SessionItem): string {
	return item.message?.content?.inline || item.message?.content?.preview || ''
}

/** Stable durable identity. Text, sequence and array position are never part
 * of item identity. The empty turn/iteration values are the legacy shape for
 * append-only user records. */
export function sessionItemIdentityKey(item: Pick<SessionItem, 'id' | 'turn_id' | 'agent_iteration'>): string {
  return JSON.stringify([item.turn_id?.trim() ?? '', Number(item.agent_iteration ?? 0), item.id])
}
