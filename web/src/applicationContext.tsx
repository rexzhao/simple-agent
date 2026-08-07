import { createContext, useContext, useEffect, type PropsWithChildren } from 'react'
import type { ApplicationLifecycle, ApplicationPageServices } from './applicationServices'

const SyncApplicationContext = createContext<ApplicationPageServices | null>(null)

export interface SyncApplicationProviderProps extends PropsWithChildren {
  readonly application: ApplicationLifecycle
  /** The pre-cutover entrypoint opts out; a cutover entrypoint can opt in. */
  readonly startOnMount?: boolean
}

/**
 * React only owns the bridge lifecycle. It never creates a second graph, and
 * it never subscribes to a resource. Passing the same application through a
 * StrictMode remount therefore exercises the same idempotent lifecycle.
 */
export function SyncApplicationProvider({ application, startOnMount = true, children }: SyncApplicationProviderProps) {
  useEffect(() => {
    if (!startOnMount) return
    application.start()
    return () => application.stop()
  }, [application, startOnMount])

  return <SyncApplicationContext.Provider value={application.page}>{children}</SyncApplicationContext.Provider>
}

export function useSyncApplicationContext(): ApplicationPageServices {
  const services = useContext(SyncApplicationContext)
  if (!services) throw new Error('useSyncApplication must be used within SyncApplicationProvider')
  return services
}
