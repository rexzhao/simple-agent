import type { Project, Session, SessionItem, SessionModelOption } from '../types'

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

export function sessionName(session: Session): string {
  return session.display_name || `Session ${session.id.slice(-6)}`
}

function sessionTimestamp(session: Session): number {
  return new Date(session.last_used_at || session.updated_at || session.created_at).getTime()
}

export function orderSessions(sessions: Session[]): Session[] {
  return [...sessions].sort((a, b) => sessionTimestamp(b) - sessionTimestamp(a))
}

export interface SessionTreeNode {
  session: Session
  children: SessionTreeNode[]
  /** The session names a parent that is unavailable or would form a cycle. */
  orphaned: boolean
}

/**
 * Builds a deterministic project-local session forest. Missing/archived
 * parents become explicit roots instead of hiding their visible descendants,
 * and corrupted cycles are broken into orphan roots. A descendant's recent
 * activity lifts its whole root branch in the ordering.
 */
export function buildSessionTree(sessions: Session[]): SessionTreeNode[] {
  const ordered = orderSessions(sessions)
  const sessionsByID = new Map(ordered.map((session) => [session.id, session]))
  const nodesByID = new Map<string, SessionTreeNode>(ordered.map((session) => [session.id, {
    session,
    children: [],
    orphaned: false,
  }]))
  const roots: SessionTreeNode[] = []

  const parentWouldCycle = (session: Session): boolean => {
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

  const branchTimestamp = (node: SessionTreeNode): number => Math.max(
    sessionTimestamp(node.session),
    ...node.children.map(branchTimestamp),
  )
  const sortBranch = (nodes: SessionTreeNode[]) => {
    for (const node of nodes) sortBranch(node.children)
    nodes.sort((a, b) => branchTimestamp(b) - branchTimestamp(a) || a.session.id.localeCompare(b.session.id))
  }
  sortBranch(roots)
  return roots
}

export function flattenSessionTree(nodes: SessionTreeNode[]): Session[] {
  return nodes.flatMap((node) => [node.session, ...flattenSessionTree(node.children)])
}

/**
 * Returns the ids of every descendant of rootID inside sessions, following
 * parent links breadth-first. Cycle-safe: corrupted lineage can never loop.
 * The backend archives/removes a whole subtree together, so this count lets
 * confirmation dialogs spell out the cascade.
 */
export function sessionDescendantIDs(sessions: Session[], rootID: string): string[] {
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
