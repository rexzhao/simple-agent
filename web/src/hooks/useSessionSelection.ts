import { useCallback, useRef, useState } from 'react'

export function useSessionSelection() {
  const [selectedProjectID, setSelectedProjectID] = useState('')
  const [selectedSessionID, setSelectedSessionID] = useState('')
  const selectedProjectRef = useRef('')
  selectedProjectRef.current = selectedProjectID

  const select = useCallback((projectID: string, sessionID = '') => {
    setSelectedProjectID(projectID)
    setSelectedSessionID(sessionID)
  }, [])

  return {
    selectedProjectID,
    selectedSessionID,
    selectedProjectRef,
    setSelectedProjectID,
    setSelectedSessionID,
    select,
  }
}
