// @vitest-environment jsdom
import { render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SyncApplicationProvider } from '../applicationContext'
import { useProjectSessions, useSessionView } from './useSyncApplication'
import { createSyncApplication } from '../sync/applicationComposition'

function Probe({ projectID, sessionID }: { projectID: string; sessionID: string }) {
  useProjectSessions(projectID)
  useSessionView(sessionID)
  return null
}

describe('page-facing sync selectors', () => {
  it('keep external-store callbacks stable and release exactly on id changes', () => {
    const application = createSyncApplication()
    const sessionIndex = application.repositories.sessionIndex
    const sessionContent = application.repositories.sessionContent
    const projectReleases: string[] = []
    const sessionReleases: string[] = []
    const originalProjectSubscribe = sessionIndex.subscribeProject.bind(sessionIndex)
    const originalSessionObserve = sessionContent.observe.bind(sessionContent)
    const projectSubscribe = vi.spyOn(sessionIndex, 'subscribeProject').mockImplementation((projectID, listener) => {
      const release = originalProjectSubscribe(projectID, listener)
      return () => {
        projectReleases.push(projectID)
        release()
      }
    })
    const sessionObserve = vi.spyOn(sessionContent, 'observe').mockImplementation((sessionID, listener) => {
      const release = originalSessionObserve(sessionID, listener)
      return () => {
        sessionReleases.push(sessionID)
        release()
      }
    })

    const view = render(
      <SyncApplicationProvider application={application} startOnMount={false}>
        <Probe projectID="project_a" sessionID="session_a" />
      </SyncApplicationProvider>,
    )
    expect(projectSubscribe).toHaveBeenCalledTimes(1)
    expect(sessionObserve).toHaveBeenCalledTimes(1)
    expect(projectReleases).toEqual([])
    expect(sessionReleases).toEqual([])

    view.rerender(
      <SyncApplicationProvider application={application} startOnMount={false}>
        <Probe projectID="project_a" sessionID="session_a" />
      </SyncApplicationProvider>,
    )
    expect(projectSubscribe).toHaveBeenCalledTimes(1)
    expect(sessionObserve).toHaveBeenCalledTimes(1)
    expect(projectReleases).toEqual([])
    expect(sessionReleases).toEqual([])

    view.rerender(
      <SyncApplicationProvider application={application} startOnMount={false}>
        <Probe projectID="project_b" sessionID="session_b" />
      </SyncApplicationProvider>,
    )
    expect(projectSubscribe).toHaveBeenCalledTimes(2)
    expect(sessionObserve).toHaveBeenCalledTimes(2)
    expect(projectReleases).toEqual(['project_a'])
    expect(sessionReleases).toEqual(['session_a'])

    view.unmount()
    expect(projectReleases).toEqual(['project_a', 'project_b'])
    expect(sessionReleases).toEqual(['session_a', 'session_b'])
    application.dispose()
  })
})
