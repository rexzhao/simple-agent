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

  it('controls browser-only protocol logs and reports clipboard failures', async () => {
    const onToggle = vi.fn()
    const onCopy = vi.fn(async () => { throw new Error('Clipboard access failed') })
    const onDownload = vi.fn()
    const onClear = vi.fn()
    render(
      <DebugSettingsDialog
        session={session}
        saving={false}
        onSave={vi.fn(async () => {})}
        frontendLogging={{ enabled: false, records: [{ log_seq: 1 } as never], droppedCount: 2 }}
        onFrontendProtocolLoggingToggle={onToggle}
        onCopyFrontendProtocolLogs={onCopy}
        onDownloadFrontendProtocolLogs={onDownload}
        onClearFrontendProtocolLogs={onClear}
        onClose={vi.fn()}
      />,
    )

    fireEvent.click(screen.getByRole('switch', { name: 'Enable frontend protocol logging' }))
    expect(onToggle).toHaveBeenCalledWith(true)
    fireEvent.click(screen.getByRole('button', { name: 'Copy JSONL' }))
    await vi.waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Clipboard access failed'))
    fireEvent.click(screen.getByRole('button', { name: 'Download JSONL' }))
    fireEvent.click(screen.getByRole('button', { name: 'Clear' }))
    expect(onDownload).toHaveBeenCalledTimes(1)
    expect(onClear).toHaveBeenCalledTimes(1)
  })
})
