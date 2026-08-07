import { useCallback, useSyncExternalStore } from 'react'
import type { ApplicationCommands, ApplicationPageServices, ApplicationRepositories, ApplicationSignals } from '../applicationServices'
import { useSyncApplicationContext } from '../applicationContext'

/** Page-facing application services. No protocol, transport, or Blob object crosses this boundary. */
export function useSyncApplication(): ApplicationPageServices {
  return useSyncApplicationContext()
}

export function useSyncRepositories(): ApplicationRepositories {
  return useSyncApplication().repositories
}

export function useSyncCommands(): ApplicationCommands {
  return useSyncApplication().commands
}

export function useSyncSignals(): ApplicationSignals {
  return useSyncApplication().signals
}

export function useCurrentProjectID(): string | null {
  const { currentProject } = useSyncSignals()
  const subscribe = useCallback((listener: () => void) => currentProject.subscribe(listener), [currentProject])
  const getSnapshot = useCallback(() => currentProject.get(), [currentProject])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useCurrentSessionID(): string | null {
  const { currentSession } = useSyncSignals()
  const subscribe = useCallback((listener: () => void) => currentSession.subscribe(listener), [currentSession])
  const getSnapshot = useCallback(() => currentSession.get(), [currentSession])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useProviderSettingsApplicationState() {
  const { providerSettings } = useSyncSignals()
  const subscribe = useCallback((listener: () => void) => providerSettings.subscribe(listener), [providerSettings])
  const getSnapshot = useCallback(() => providerSettings.get(), [providerSettings])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useCurrentCodexLoginProvider(): string | null {
  const { codexLoginProvider } = useSyncSignals()
  const subscribe = useCallback((listener: () => void) => codexLoginProvider.subscribe(listener), [codexLoginProvider])
  const getSnapshot = useCallback(() => codexLoginProvider.get(), [codexLoginProvider])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useProjectIndex() {
  const { projectIndex } = useSyncRepositories()
  const subscribe = useCallback((listener: () => void) => projectIndex.subscribe(listener), [projectIndex])
  const getSnapshot = useCallback(() => projectIndex.getSnapshot(), [projectIndex])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useProjectSessions(projectID: string) {
  const { sessionIndex } = useSyncRepositories()
  const subscribe = useCallback((listener: () => void) => sessionIndex.subscribeProject(projectID, listener), [sessionIndex, projectID])
  const getSnapshot = useCallback(() => sessionIndex.getProjectReadModel(projectID), [sessionIndex, projectID])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useSessionView(sessionID: string) {
  const { sessionContent } = useSyncRepositories()
  const subscribe = useCallback((listener: () => void) => sessionContent.observe(sessionID, listener), [sessionContent, sessionID])
  const getSnapshot = useCallback(() => sessionContent.get(sessionID), [sessionContent, sessionID])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useProviderSettings() {
  const { providerSettings } = useSyncRepositories()
  const subscribe = useCallback((listener: () => void) => providerSettings.subscribe(listener), [providerSettings])
  const getSnapshot = useCallback(() => providerSettings.getSnapshot(), [providerSettings])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export function useCodexLogin(provider: string) {
  const { codexLogin } = useSyncRepositories()
  const subscribe = useCallback((listener: () => void) => codexLogin.subscribe(listener), [codexLogin])
  const getSnapshot = useCallback(() => codexLogin.getSnapshot(provider), [codexLogin, provider])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}
