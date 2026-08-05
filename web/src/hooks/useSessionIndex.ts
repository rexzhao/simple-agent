import { useSyncExternalStore } from 'react'
import type { SessionIndexReadModel, SessionIndexRepository, SessionReadModel } from '../repositories/sessionIndex'

export function useSessionIndex(repository: SessionIndexRepository, projectID: string): SessionIndexReadModel {
  return useSyncExternalStore(
    (listener) => repository.subscribeProject(projectID, listener),
    () => repository.getProjectReadModel(projectID),
    () => repository.getProjectReadModel(projectID),
  )
}

export function useSession(repository: SessionIndexRepository, projectID: string, sessionID: string): SessionReadModel {
  return useSyncExternalStore(
    (listener) => repository.subscribeSession(projectID, sessionID, listener),
    () => repository.getSessionReadModel(projectID, sessionID),
    () => repository.getSessionReadModel(projectID, sessionID),
  )
}

export const useSessionSummary = useSession
