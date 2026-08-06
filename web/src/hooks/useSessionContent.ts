import { useSyncExternalStore } from 'react'
import type { SessionContentRepository, SessionView, SessionRunState } from '../repositories/sessionContent'

export function useSessionView(repository: SessionContentRepository, sessionID: string): SessionView {
  return useSyncExternalStore(
    (listener) => repository.observe(sessionID, listener),
    () => repository.get(sessionID),
    () => repository.get(sessionID),
  )
}

export function useSessionRunState(repository: SessionContentRepository, sessionID: string): SessionRunState | undefined {
  return useSessionView(repository, sessionID).runState
}

export const useSessionContent = useSessionView
