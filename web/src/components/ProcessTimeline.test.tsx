// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, fireEvent } from '@testing-library/react'
import { calculatePopoverPosition, PROCESS_HOVER_HIDE_DELAY_MS, PROCESS_HOVER_REPLACE_DELAY_MS, PROCESS_POINTER_POPOVER_GAP_PX, ProcessTimeline } from './ProcessTimeline'
import type { RunStep, ToolActivity } from '../types'

afterEach(() => {
	vi.useRealTimers()
	cleanup()
})

const tool = (overrides: Partial<ToolActivity>): ToolActivity => ({
	kind: 'tool',
	id: 'call-1',
	name: 'read_file',
	iteration: 1,
	status: 'finished',
	...overrides,
})

function mockRect(element: Element, top: number, left = 80, width = 220, height = 32): void {
	vi.spyOn(element, 'getBoundingClientRect').mockReturnValue({
		top, bottom: top + height, left, right: left + width, width, height,
		x: left, y: top, toJSON: () => ({}),
	} as DOMRect)
}

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

	it('keeps Cancel outside the click trigger without opening details', () => {
		const onCancelTool = vi.fn()
		const { container } = render(<ProcessTimeline steps={[tool({ id: 'cancel-me', status: 'running' })]} onCancelTool={onCancelTool} />)
		const cancel = screen.getByRole('button', { name: 'Cancel tool' })
		expect(cancel.closest('[role="button"]')).toBeNull()
		fireEvent.click(cancel)
		expect(onCancelTool).toHaveBeenCalledWith('cancel-me')
		expect(screen.queryByRole('tooltip')).toBeNull()
	})

	it('shows only a compact reasoning summary and marks only the current reasoning step as running', () => {
		const reasoning: RunStep = { kind: 'reasoning', id: 'reason-a', text: '😀a中', iteration: 1 }
		const view = render(<ProcessTimeline steps={[reasoning]} live />)

		expect(view.container.querySelector('.reasoning-trigger')?.textContent).toContain('Thinking')
		expect(view.container.querySelector('.reasoning-trigger')?.textContent).toContain('3 chars')
		expect(view.container.querySelector('.reasoning-trigger')?.textContent).toContain('—')
		expect(view.container.querySelector('.reasoning-step pre')).toBeNull()
		expect(view.container.querySelector('[aria-label="Reasoning status: Thinking"]')).not.toBeNull()

		view.rerender(<ProcessTimeline steps={[reasoning]} live={false} />)
		expect(view.container.querySelector('.reasoning-trigger')?.textContent).toContain('Thinking complete')
		expect(view.container.querySelector('[aria-label="Reasoning status: Thinking"]')).toBeNull()
		expect(view.container.querySelector('.reasoning-step pre')).toBeNull()
	})

	it('does not open tool details on hover or focus, but opens them on click', () => {
		const { container } = render(<ProcessTimeline steps={[tool({ name: 'shell', arguments: JSON.stringify({ command: 'show me' }) })]} />)
		const trigger = container.querySelector('.tool-row-header') as HTMLElement
		fireEvent.mouseEnter(trigger)
		fireEvent.focus(trigger)
		expect(screen.queryByRole('tooltip')).toBeNull()
		fireEvent.click(trigger)
		expect(screen.getByRole('tooltip').textContent).toContain('show me')
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
			expect(view.container.querySelector('.reasoning-trigger')?.textContent).toContain('1.5s')
			expect(view.container.querySelector('[aria-label="Reasoning status: Thinking"]')).toBeNull()

			view.unmount()
			const remounted = render(<ProcessTimeline steps={[reasoning]} />)
			expect(remounted.container.querySelector('.reasoning-trigger')?.textContent).toContain('1.5s')
		} finally {
			vi.useRealTimers()
		}
	})

	it('does not invent a zero duration when history has no event timing', () => {
		const reasoning: RunStep = { kind: 'reasoning', id: 'historical-reasoning', text: 'old', iteration: 1 }
		const { container } = render(<ProcessTimeline steps={[reasoning]} />)
		const summary = container.querySelector('.reasoning-trigger')?.textContent ?? ''
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

	it('shows request arguments and the result in the session tool hover popup', () => {
		const args = JSON.stringify({ session_id: 'sess-1', mode: 'steer', message: 'please retry' })
		const steps: RunStep[] = [tool({
			name: 'session_send',
			arguments: args,
			result: '{"delivery":"started","ok":true}',
		})]
		render(<ProcessTimeline steps={steps} sessionNames={{ 'sess-1': 'Review session' }} />)

		// Collapsed summary resolves the session display name and snippets the message.
		expect(screen.getByText(/Review session/)).not.toBeNull()

		const trigger = screen.getByText('session_send').closest('.tool-row-header')
		expect(trigger).not.toBeNull()
		fireEvent.click(trigger as Element)

		expect(screen.getByText('Arguments')).not.toBeNull()
		expect(screen.getByText(/"mode": "steer"/)).not.toBeNull()
		expect(screen.getByText('Output')).not.toBeNull()
		expect(screen.getByText(/"ok": true/)).not.toBeNull()
	})

	it('shows a popup for pending session tools without output', () => {
		const steps: RunStep[] = [tool({
			name: 'session_wait',
			arguments: JSON.stringify({ session_id: 'sess-9', timeout_ms: 1000 }),
			status: 'running',
		})]
		render(<ProcessTimeline steps={steps} />)

		expect(screen.getByText('session_wait')).not.toBeNull()
		expect(screen.getByText('sess-9')).not.toBeNull()
		fireEvent.click(screen.getByText('session_wait').closest('.tool-row-header') as Element)
		expect(screen.queryByText('Output')).toBeNull()
		expect(screen.getByRole('tooltip').textContent).toContain('Arguments')
	})

	it('places the portal below an upper trigger and above a lower trigger', () => {
		vi.useFakeTimers()
		const { container } = render(<ProcessTimeline steps={[
			tool({ id: 'upper', name: 'shell', arguments: JSON.stringify({ command: 'upper command' }) }),
			tool({ id: 'lower', name: 'shell', arguments: JSON.stringify({ command: 'lower command' }) }),
		]} />)
		const triggers = Array.from(container.querySelectorAll('.tool-row-header'))
		mockRect(triggers[0], 300)
		mockRect(triggers[1], 680)

		fireEvent.click(triggers[0])
		const below = screen.getByRole('tooltip')
		const belowTop = Number.parseFloat(below.style.top)
		const belowMaxHeight = Number.parseFloat(below.style.maxHeight)
		const upperBottom = 332
		const viewportHeight = window.innerHeight
		expect(below.getAttribute('data-placement')).toBe('below')
		expect(belowTop).toBeGreaterThanOrEqual(upperBottom + 8)
		expect(belowTop).toBe(upperBottom + 8)
		expect(belowMaxHeight).toBe(Math.min(520, viewportHeight - 12 - upperBottom - 8))
		expect(belowTop + belowMaxHeight).toBeLessThanOrEqual(viewportHeight - 12)
		expect(below.textContent).toContain('upper command')
		fireEvent.click(triggers[1])
		expect(screen.getByRole('tooltip').textContent).toContain('upper command')
		act(() => { vi.advanceTimersByTime(PROCESS_HOVER_REPLACE_DELAY_MS) })
		const above = screen.getByRole('tooltip')
		const aboveTop = Number.parseFloat(above.style.top)
		const aboveMaxHeight = Number.parseFloat(above.style.maxHeight)
		const lowerTop = 680
		expect(above.getAttribute('data-placement')).toBe('above')
		expect(aboveTop + aboveMaxHeight).toBeLessThanOrEqual(lowerTop - 8)
		expect(aboveTop).toBe(lowerTop - 8 - aboveMaxHeight)
		expect(above.textContent).toContain('lower command')
	})

	it('anchors clicked tool and reasoning popovers to the same pointer offset', () => {
		vi.useFakeTimers()
		const reasoning: RunStep = { kind: 'reasoning', id: 'reasoning-anchor', text: 'private thoughts', iteration: 1 }
		const { container } = render(<ProcessTimeline steps={[reasoning, tool({ id: 'tool-anchor', name: 'shell', arguments: JSON.stringify({ command: 'details' }) })]} />)
		const reasoningTrigger = container.querySelector('.reasoning-trigger') as Element
		const toolTrigger = container.querySelector('.tool-row-header') as Element
		mockRect(reasoningTrigger, 300, 80, 400, 12)
		mockRect(toolTrigger, 340, 82, 496, 35)

		fireEvent.click(reasoningTrigger, { clientX: 200, clientY: 306 })
		const reasoningPopup = screen.getByRole('tooltip')
		const reasoningLeft = Number.parseFloat(reasoningPopup.style.left)
		expect(reasoningPopup.getAttribute('data-placement')).toBe('below')
		expect(reasoningLeft).toBe(200 + PROCESS_POINTER_POPOVER_GAP_PX)

		fireEvent.click(toolTrigger, { clientX: 200, clientY: 346 })
		act(() => { vi.advanceTimersByTime(PROCESS_HOVER_REPLACE_DELAY_MS) })
		const toolPopup = screen.getByRole('tooltip')
		const toolLeft = Number.parseFloat(toolPopup.style.left)
		expect(toolPopup.getAttribute('data-placement')).toBe('below')
		expect(toolLeft).toBe(reasoningLeft)
	})

	it('uses the right-side pointer gap, flips left when needed, and keeps vertical placement trigger-based', () => {
		const trigger = { top: 100, bottom: 132, left: 80, right: 300, width: 220, height: 32 }
		const popup = { top: 0, bottom: 100, left: 0, right: 300, width: 300, height: 100 }
		const reference = { top: 100, bottom: 132, left: 80, right: 600, width: 520, height: 32 }
		const right = calculatePopoverPosition(trigger, popup, reference, { clientX: 200, clientY: 110 })
		expect(right.left).toBe(200 + PROCESS_POINTER_POPOVER_GAP_PX)
		const flipped = calculatePopoverPosition(trigger, popup, reference, { clientX: window.innerWidth - 40, clientY: 110 })
		expect(flipped.left).toBe(window.innerWidth - 40 - PROCESS_POINTER_POPOVER_GAP_PX - popup.width)
		expect(flipped.left + popup.width).toBeLessThanOrEqual(window.innerWidth - 12)
		expect(calculatePopoverPosition(trigger, popup, reference, { clientX: 200, clientY: 700 }).top).toBe(right.top)
		const keyboardFallback = calculatePopoverPosition(trigger, popup, reference)
		expect(keyboardFallback.left).toBe(reference.left + (reference.width - popup.width) / 2)
	})

	it('keeps the popup during trigger-leave delay and hides after popup-leave delay', () => {
		vi.useFakeTimers()
		const { container } = render(<ProcessTimeline steps={[tool({ id: 'delayed', name: 'shell', arguments: JSON.stringify({ command: 'copy me' }) })]} />)
		const trigger = container.querySelector('.tool-row-header') as Element
		mockRect(trigger, 100)
		fireEvent.click(trigger)
		fireEvent.mouseLeave(trigger)
		act(() => { vi.advanceTimersByTime(PROCESS_HOVER_HIDE_DELAY_MS - 1) })
		expect(screen.getByRole('tooltip')).not.toBeNull()
		act(() => { vi.advanceTimersByTime(1) })
		expect(screen.queryByRole('tooltip')).toBeNull()
	})

	it('keeps the popup while the pointer enters it, then delays its final removal', () => {
		vi.useFakeTimers()
		const { container } = render(<ProcessTimeline steps={[tool({ id: 'copy', name: 'shell', arguments: JSON.stringify({ command: 'copyable output' }) })]} />)
		const trigger = container.querySelector('.tool-row-header') as Element
		mockRect(trigger, 100)
		fireEvent.click(trigger)
		fireEvent.mouseLeave(trigger)
		const popup = screen.getByRole('tooltip')
		fireEvent.mouseEnter(popup)
		act(() => { vi.advanceTimersByTime(PROCESS_HOVER_HIDE_DELAY_MS + 20) })
		expect(screen.getByRole('tooltip')).toBe(popup)
		fireEvent.mouseLeave(popup)
		act(() => { vi.advanceTimersByTime(PROCESS_HOVER_HIDE_DELAY_MS - 1) })
		expect(screen.getByRole('tooltip')).toBe(popup)
		act(() => { vi.advanceTimersByTime(1) })
		expect(screen.queryByRole('tooltip')).toBeNull()
	})

	it('delays replacement, permits entering the old popup, and keeps detail text selectable', () => {
		vi.useFakeTimers()
		const { container } = render(<ProcessTimeline steps={[
			tool({ id: 'old', name: 'shell', arguments: JSON.stringify({ command: 'old details' }) }),
			tool({ id: 'new', name: 'shell', arguments: JSON.stringify({ command: 'new details <img>' }) }),
		]} />)
		const triggers = Array.from(container.querySelectorAll('.tool-row-header'))
		mockRect(triggers[0], 100)
		mockRect(triggers[1], 120)
		fireEvent.click(triggers[0])
		const popup = screen.getByRole('tooltip')
		fireEvent.click(triggers[1])
		expect(screen.getByRole('tooltip')).toBe(popup)
		expect(screen.getByRole('tooltip').textContent).toContain('old details')
		fireEvent.mouseEnter(popup)
		act(() => { vi.advanceTimersByTime(PROCESS_HOVER_REPLACE_DELAY_MS + 20) })
		expect(screen.getByRole('tooltip')).toBe(popup)
		expect(screen.getByRole('tooltip').textContent).toContain('old details')
		// The portal is outside the timeline DOM and its content is plain text,
		// so copying it cannot turn tool output into markup.
		expect(container.querySelector('.process-hover-popover')).toBeNull()
		expect(popup.querySelector('pre')?.textContent).toContain('old details')
		expect(popup.querySelector('img')).toBeNull()
	})

	it('renders shell SGR output as styled text without interpreting HTML', () => {
		const output = '\x1b[31mred\x1b[1;4m bold\x1b[0m plain\n<img src=x onerror=alert(1)>'
		const { container } = render(<ProcessTimeline steps={[tool({
			name: 'shell',
			arguments: JSON.stringify({ command: 'printf colored' }),
			result: output,
		})]} />)
		const trigger = container.querySelector('.tool-row-header') as Element
		fireEvent.click(trigger)

		const pre = screen.getByRole('tooltip').querySelector('pre.ansi-output')
		expect(pre).not.toBeNull()
		expect(pre?.textContent).toBe('red bold plain\n<img src=x onerror=alert(1)>')
		expect(pre?.querySelector('.ansi-fg-red')?.textContent).toBe('red')
		expect(pre?.querySelector('.ansi-bold.ansi-underline')?.textContent).toBe(' bold')
		expect(pre?.querySelector('img')).toBeNull()
	})

	it('does not open on hover or focus, opens on keyboard activation, and Escape closes the tooltip', () => {
		const { container } = render(<ProcessTimeline steps={[{ kind: 'reasoning', id: 'keyboard-reasoning', text: 'private thoughts', iteration: 1 }]} />)
		const trigger = container.querySelector('.reasoning-trigger') as HTMLElement
		mockRect(trigger, 100)
		fireEvent.mouseEnter(trigger)
		fireEvent.focus(trigger)
		expect(trigger.getAttribute('aria-expanded')).toBe('false')
		expect(screen.queryByRole('tooltip')).toBeNull()
		fireEvent.keyDown(trigger, { key: 'Enter' })
		expect(trigger.getAttribute('aria-expanded')).toBe('true')
		expect(trigger.getAttribute('aria-haspopup')).toBeNull()
		expect(trigger.getAttribute('aria-describedby')).toBe('process-hover-details')
		expect(screen.getByRole('tooltip').textContent).toContain('private thoughts')
		fireEvent.keyDown(trigger, { key: 'Escape' })
		expect(screen.queryByRole('tooltip')).toBeNull()
		fireEvent.keyDown(trigger, { key: ' ' })
		expect(screen.getByRole('tooltip')).not.toBeNull()
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
