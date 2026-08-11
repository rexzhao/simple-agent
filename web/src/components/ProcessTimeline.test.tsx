// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, fireEvent } from '@testing-library/react'
import { ProcessTimeline } from './ProcessTimeline'
import type { RunStep, ToolActivity } from '../types'

afterEach(cleanup)

const tool = (overrides: Partial<ToolActivity>): ToolActivity => ({
	kind: 'tool',
	id: 'call-1',
	name: 'read_file',
	iteration: 1,
	status: 'finished',
	...overrides,
})

describe('ProcessTimeline session tools', () => {
	it('uses a colored leading dot and accessible status instead of trailing status text', () => {
		const { container } = render(<ProcessTimeline steps={[
			tool({ id: 'requested', status: 'requested' }),
			tool({ id: 'running', status: 'running' }),
			tool({ id: 'finished', status: 'finished' }),
			tool({ id: 'error', status: 'error' }),
		]} />)

		expect(container.querySelectorAll('.tool-row-header small')).toHaveLength(0)
		expect(container.querySelectorAll('.tool-status-dot')).toHaveLength(4)
		expect(container.querySelector('.tool-status-dot.requested')).not.toBeNull()
		expect(container.querySelector('.tool-status-dot.running')).not.toBeNull()
		expect(container.querySelector('.tool-status-dot.finished')).not.toBeNull()
		expect(container.querySelector('.tool-status-dot.error')).not.toBeNull()
		expect(screen.getByRole('img', { name: 'Tool status: Requested' })).not.toBeNull()
		expect(screen.getByRole('img', { name: 'Tool status: Running' })).not.toBeNull()
		expect(screen.getByRole('img', { name: 'Tool status: Done' })).not.toBeNull()
		expect(screen.getByRole('img', { name: 'Tool status: Failed' })).not.toBeNull()
		expect(container.textContent).not.toMatch(/Requested|Running|Done|Failed/)
	})

	it('shows only a compact reasoning summary and marks only the current reasoning step as running', () => {
		const reasoning: RunStep = { kind: 'reasoning', id: 'reason-a', text: '😀a中', iteration: 1 }
		const view = render(<ProcessTimeline steps={[reasoning]} live />)

		expect(view.container.querySelector('.reasoning-step summary')?.textContent).toContain('Thinking')
		expect(view.container.querySelector('.reasoning-step summary')?.textContent).toContain('3 chars')
		expect(view.container.querySelector('.reasoning-step summary')?.textContent).toContain('—')
		expect(view.container.querySelector('.reasoning-step pre')).toBeNull()
		expect(view.container.querySelector('[aria-label="Reasoning status: Thinking"]')).not.toBeNull()

		view.rerender(<ProcessTimeline steps={[reasoning]} live={false} />)
		expect(view.container.querySelector('.reasoning-step summary')?.textContent).toContain('Thinking complete')
		expect(view.container.querySelector('[aria-label="Reasoning status: Thinking"]')).toBeNull()
		expect(view.container.querySelector('.reasoning-step pre')).toBeNull()
	})

	it('does not mark an earlier reasoning step when a later tool is the current activity', () => {
		const reasoning: RunStep = { kind: 'reasoning', id: 'reason-before-tool', text: 'thinking', iteration: 1 }
		const { container } = render(<ProcessTimeline steps={[reasoning, tool({ id: 'tool-after-reasoning', status: 'running' })]} live />)

		expect(container.querySelector('.reasoning-step [aria-label="Reasoning status: Thinking"]')).toBeNull()
		expect(container.querySelector('.tool-status-dot.running')).not.toBeNull()
	})

	it('uses event timing for a handed-off reasoning step and keeps it after remount', () => {
		vi.useFakeTimers()
		try {
			vi.setSystemTime(new Date('2025-01-01T00:00:00.000Z'))
			const reasoning: RunStep = {
				kind: 'reasoning', id: 'timed-reasoning', text: 'thinking', iteration: 1,
				reasoningTiming: { startedAt: '2025-01-01T00:00:01.000Z', endedAt: '2025-01-01T00:00:02.500Z' },
			}
			const toolStep = tool({ id: 'after-reasoning', status: 'running' })
			const view = render(<ProcessTimeline steps={[reasoning, toolStep]} live />)
			expect(view.container.querySelector('.reasoning-step summary')?.textContent).toContain('1.5s')
			expect(view.container.querySelector('[aria-label="Reasoning status: Thinking"]')).toBeNull()

			view.unmount()
			const remounted = render(<ProcessTimeline steps={[reasoning]} />)
			expect(remounted.container.querySelector('.reasoning-step summary')?.textContent).toContain('1.5s')
		} finally {
			vi.useRealTimers()
		}
	})

	it('does not invent a zero duration when history has no event timing', () => {
		const reasoning: RunStep = { kind: 'reasoning', id: 'historical-reasoning', text: 'old', iteration: 1 }
		const { container } = render(<ProcessTimeline steps={[reasoning]} />)
		const summary = container.querySelector('.reasoning-step summary')?.textContent ?? ''
		expect(summary).toContain('—')
		expect(summary).not.toContain('0ms')
	})

	it('shows start_line and line_count on the read_file row', () => {
		const steps: RunStep[] = [tool({
			arguments: JSON.stringify({ path: 'notes.ts', start_line: 42, line_count: 10 }),
		})]
		render(<ProcessTimeline steps={steps} />)

		expect(screen.getByText('read_file')).not.toBeNull()
		expect(screen.getByText('notes.ts:42+10')).not.toBeNull()
	})

	it('omits the trailing +count when read_file has no line_count', () => {
		const steps: RunStep[] = [tool({
			arguments: JSON.stringify({ path: 'notes.ts', start_line: 42 }),
		})]
		render(<ProcessTimeline steps={steps} />)

		expect(screen.getByText('read_file')).not.toBeNull()
		expect(screen.getByText('notes.ts:42')).not.toBeNull()
	})

	it('shows a summary target on the collapsed session_start row', () => {
		const steps: RunStep[] = [tool({
			name: 'session_start',
			arguments: JSON.stringify({ name: 'Review', provider: 'paperhub', model: 'grok-4.5', prompt: 'please review the plan' }),
		})]
		render(<ProcessTimeline steps={steps} />)

		expect(screen.getByText('session_start')).not.toBeNull()
		expect(screen.getByText('"Review" · grok-4.5')).not.toBeNull()
	})

	it('shows request arguments and the result when the session tool row expands', () => {
		const args = JSON.stringify({ session_id: 'sess-1', mode: 'steer', message: 'please retry' })
		const steps: RunStep[] = [tool({
			name: 'session_send',
			arguments: args,
			result: '{"delivery":"started","ok":true}',
		})]
		render(<ProcessTimeline steps={steps} sessionNames={{ 'sess-1': 'Review session' }} />)

		// Collapsed summary resolves the session display name and snippets the message.
		expect(screen.getByText(/Review session/)).not.toBeNull()

		const summary = screen.getByText('session_send').closest('summary')
		expect(summary).not.toBeNull()
		fireEvent.click(summary as Element)

		expect(screen.getByText('Arguments')).not.toBeNull()
		expect(screen.getByText(/"mode": "steer"/)).not.toBeNull()
		expect(screen.getByText('Output')).not.toBeNull()
		expect(screen.getByText(/"ok": true/)).not.toBeNull()
	})

	it('keeps session tool rows expandable on pending results without output', () => {
		const steps: RunStep[] = [tool({
			name: 'session_wait',
			arguments: JSON.stringify({ session_id: 'sess-9', timeout_ms: 1000 }),
			status: 'running',
		})]
		render(<ProcessTimeline steps={steps} />)

		expect(screen.getByText('session_wait')).not.toBeNull()
		expect(screen.getByText('sess-9')).not.toBeNull()
		expect(screen.queryByText('Output')).toBeNull()
	})

	it('renders interleaved tool and reasoning as separate rows in input order', () => {
		const { container } = render(<ProcessTimeline steps={[
			tool({ id: 'tool-a', iteration: 1 }),
			{ kind: 'reasoning', id: 'reason-a', text: 'between', iteration: 1 },
			tool({ id: 'tool-b', name: 'shell', iteration: 2 }),
			{ kind: 'reasoning', id: 'reason-b', text: 'after', iteration: 1 },
		]} />)

		expect(container.querySelector('.tool-group')).toBeNull()
		expect(Array.from(container.querySelectorAll('.process-timeline > *')).map((element) => element.classList[0])).toEqual([
			'tool-row', 'reasoning-step', 'tool-row', 'reasoning-step',
		])
		expect(container.querySelectorAll('.tool-row')).toHaveLength(2)
		expect(container.querySelectorAll('.reasoning-step')).toHaveLength(2)
	})
})
