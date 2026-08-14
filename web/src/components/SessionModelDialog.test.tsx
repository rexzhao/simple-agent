// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { SessionModelDialog } from './SessionModelDialog'

afterEach(cleanup)

describe('SessionModelDialog', () => {
  it('lets a new session disable automatic context compaction', () => {
    const onAutomaticCompaction = vi.fn()
    render(
      <SessionModelDialog
        state={{
          projectID: 'project-1',
          models: [{ provider: 'test', model_profile: 'default', model_id: 'test-model' }],
          selectedKey: 'test\u0000default',
          defaultProvider: 'test',
          defaultModel: 'default',
          reasoningLevel: '',
          fullAccess: false,
          automaticCompaction: true,
          loading: false,
        }}
        creating={false}
        onSelect={vi.fn()}
        onReasoningLevel={vi.fn()}
        onFullAccess={vi.fn()}
        onAutomaticCompaction={onAutomaticCompaction}
        onCancel={vi.fn()}
        onCreate={vi.fn()}
      />,
    )

    const checkbox = screen.getByRole('checkbox', { name: /Automatic context compaction/ }) as HTMLInputElement
    expect(checkbox.checked).toBe(true)
    fireEvent.click(checkbox)
    expect(onAutomaticCompaction).toHaveBeenCalledWith(false)
  })
})
