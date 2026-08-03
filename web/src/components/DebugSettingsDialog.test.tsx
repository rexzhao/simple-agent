// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { DebugSettingsDialog } from './DebugSettingsDialog'
import type { Session } from '../types'

afterEach(cleanup)

const session: Session = {
  id: 's1', project_id: 'p', display_name: 'Session s1', last_seq: 0,
  provider: 'fake', model_profile: 'default', model_id: 'fake-model',
  created_at: '', updated_at: '', created_by: 'user', root_session_id: 's1', spawn_depth: 0,
  archived: false, last_used_at: '', full_access: false, created_cwd: '',
  debug: { request_bodies: false },
}

describe('DebugSettingsDialog', () => {
  it('edits and saves the per-session request-body setting', async () => {
    const onSave = vi.fn(async () => {})
    render(<DebugSettingsDialog session={session} saving={false} onSave={onSave} onClose={vi.fn()} />)

    const checkbox = screen.getByRole('checkbox') as HTMLInputElement
    expect(checkbox.checked).toBe(false)
    fireEvent.click(checkbox)
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }))

    expect(onSave).toHaveBeenCalledWith({ request_bodies: true })
  })
})
