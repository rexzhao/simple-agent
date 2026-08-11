import { createPortal } from 'react-dom'
import { createContext, memo, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent, ReactNode } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { ReasoningActivity, RunStep, ToolActivity } from '../types'
import { formatDuration, unicodeCodePointLength } from '../lib/format'
import { flattenProcessSteps } from '../lib/runSteps'
import { isPathOutsideWorkspace } from '../lib/paths'
import { isSessionToolName, prettyJSONText, sessionToolTarget } from '../lib/sessionTools'
import { ToolIcon } from './icons'

const markdownComponents: Components = {
	a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

export const PROCESS_HOVER_HIDE_DELAY_MS = 180
export const PROCESS_HOVER_REPLACE_DELAY_MS = 140

const processHoverPopoverID = 'process-hover-details'

type ProcessHoverEntry = {
	id: string
	kind: 'reasoning' | 'tool'
	trigger: HTMLElement
	content: ReactNode
	label: string
}

type ProcessHoverContextValue = {
	activeID: string | null
	show: (entry: ProcessHoverEntry) => void
	update: (entry: ProcessHoverEntry) => void
	leaveTrigger: (trigger: HTMLElement) => void
	enterPopover: () => void
	leavePopover: () => void
	close: () => void
}

const ProcessHoverContext = createContext<ProcessHoverContextValue | null>(null)

/**
 * Owns the only hover timers for a conversation. Keeping this above all
 * timelines is important: a virtualized row can disappear while its popup is
 * open, and moving between two rows must not create competing row-local
 * timers.
 */
export function ProcessHoverProvider({ children, scopeKey }: { children: ReactNode; scopeKey?: string }) {
	const [active, setActive] = useState<ProcessHoverEntry | null>(null)
	const activeRef = useRef<ProcessHoverEntry | null>(null)
	const pendingRef = useRef<ProcessHoverEntry | null>(null)
	const hoveredTriggerRef = useRef<HTMLElement | null>(null)
	const popoverHoveredRef = useRef(false)
	const timerRef = useRef<number | null>(null)

	const clearTimer = useCallback(() => {
		if (timerRef.current !== null) window.clearTimeout(timerRef.current)
		timerRef.current = null
	}, [])
	const setActiveEntry = useCallback((entry: ProcessHoverEntry | null) => {
		activeRef.current = entry
		setActive(entry)
	}, [])
	const scheduleHide = useCallback(() => {
		clearTimer()
		timerRef.current = window.setTimeout(() => {
			timerRef.current = null
			if (popoverHoveredRef.current || hoveredTriggerRef.current) return
			pendingRef.current = null
			setActiveEntry(null)
		}, PROCESS_HOVER_HIDE_DELAY_MS)
	}, [clearTimer, setActiveEntry])

	const show = useCallback((entry: ProcessHoverEntry) => {
		clearTimer()
		hoveredTriggerRef.current = entry.trigger
		const current = activeRef.current
		if (!current || current.id === entry.id) {
			pendingRef.current = null
			setActiveEntry(entry)
			return
		}

		// Keep the old popup alive while crossing to another trigger. This gives
		// the pointer a chance to enter it and cancel the replacement.
		pendingRef.current = entry
		timerRef.current = window.setTimeout(() => {
			timerRef.current = null
			if (pendingRef.current !== entry) return
			pendingRef.current = null
			if (hoveredTriggerRef.current === entry.trigger && !popoverHoveredRef.current) setActiveEntry(entry)
			else if (!hoveredTriggerRef.current && !popoverHoveredRef.current) setActiveEntry(null)
		}, PROCESS_HOVER_REPLACE_DELAY_MS)
	}, [clearTimer, setActiveEntry])

	const update = useCallback((entry: ProcessHoverEntry) => {
		if (activeRef.current?.id === entry.id && activeRef.current.trigger === entry.trigger) setActiveEntry(entry)
	}, [setActiveEntry])
	const leaveTrigger = useCallback((trigger: HTMLElement) => {
		if (hoveredTriggerRef.current !== trigger) return
		hoveredTriggerRef.current = null
		if (pendingRef.current?.trigger === trigger) {
			pendingRef.current = null
			clearTimer()
		}
		scheduleHide()
	}, [clearTimer, scheduleHide])
	const enterPopover = useCallback(() => {
		popoverHoveredRef.current = true
		pendingRef.current = null
		clearTimer()
	}, [clearTimer])
	const leavePopover = useCallback(() => {
		popoverHoveredRef.current = false
		scheduleHide()
	}, [scheduleHide])
	const close = useCallback(() => {
		clearTimer()
		pendingRef.current = null
		hoveredTriggerRef.current = null
		popoverHoveredRef.current = false
		setActiveEntry(null)
	}, [clearTimer, setActiveEntry])

	useEffect(() => {
		// scopeKey changes are session changes. Do not let a portal from the old
		// session survive while virtualized content for the new one is mounting.
		if (scopeKey !== undefined) close()
	}, [close, scopeKey])
	useEffect(() => () => {
		clearTimer()
		activeRef.current = null
		pendingRef.current = null
	}, [clearTimer])

	const context = useMemo(() => ({ activeID: active?.id ?? null, show, update, leaveTrigger, enterPopover, leavePopover, close }), [active?.id, close, enterPopover, leavePopover, leaveTrigger, show, update])
	return (
		<ProcessHoverContext.Provider value={context}>
			{children}
			{active && <ProcessHoverPopover entry={active} onEnter={enterPopover} onLeave={leavePopover} onClose={close} />}
		</ProcessHoverContext.Provider>
	)
}

export const ProcessTimeline = memo(function ProcessTimeline(props: { steps: RunStep[]; live?: boolean; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
	const existingHoverContext = useContext(ProcessHoverContext)
	if (existingHoverContext) return <ProcessTimelineContent {...props} />
	// Standalone timelines (including component tests) still get the same
	// singleton behavior. Conversation supplies one provider around its list.
	return <ProcessHoverProvider><ProcessTimelineContent {...props} /></ProcessHoverProvider>
})

function ProcessTimelineContent({ steps, live = false, onCancelTool, sessionNames, workspaceRoot }: { steps: RunStep[]; live?: boolean; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
	const nodes = flattenProcessSteps(steps)
	const lastFlat = nodes[nodes.length - 1]
	const lastStepID = lastFlat?.step.id
	return (
		<div className="process-timeline">
			{nodes.map(({ step, iteration, iterationStart }) => {
				const marker = iterationStart ? <i className="iteration-marker">{iteration}</i> : null
				if (step.kind === 'reasoning') {
					return <ReasoningStep key={step.id} step={step} marker={marker} streaming={live && step.id === lastStepID} />
				}
				if (step.kind === 'output') {
					// Mid-turn assistant output renders like the final assistant
					// message: plain markdown body, no process card chrome.
					return (
						<div className="step-message assistant" key={step.id}>{marker}
							<div className="message-text markdown-body"><Markdown remarkPlugins={[remarkGfm]} components={markdownComponents} skipHtml>{step.text}</Markdown></div>
						</div>
					)
				}
				return <ToolRow key={step.id} tool={step} marker={marker} onCancelTool={onCancelTool} sessionNames={sessionNames} workspaceRoot={workspaceRoot} />
			})}
		</div>
	)
}

function ProcessHoverPopover({ entry, onEnter, onLeave, onClose }: { entry: ProcessHoverEntry; onEnter: () => void; onLeave: () => void; onClose: () => void }) {
	const popoverRef = useRef<HTMLDivElement>(null)
	const [position, setPosition] = useState(() => calculatePopoverPosition(entry.trigger.getBoundingClientRect()))

	const reposition = useCallback(() => {
		if (!entry.trigger.isConnected) {
			onClose()
			return
		}
		const next = calculatePopoverPosition(entry.trigger.getBoundingClientRect(), popoverRef.current?.getBoundingClientRect())
		setPosition((previous) => previous.top === next.top && previous.left === next.left && previous.maxHeight === next.maxHeight && previous.placement === next.placement ? previous : next)
	}, [entry.trigger, onClose])

	useLayoutEffect(() => {
		reposition()
	}, [entry, reposition])
	useEffect(() => {
		window.addEventListener('resize', reposition)
		window.addEventListener('scroll', reposition, true)
		return () => {
			window.removeEventListener('resize', reposition)
			window.removeEventListener('scroll', reposition, true)
		}
	}, [reposition])

	const popup = (
		<div
			ref={popoverRef}
			id={processHoverPopoverID}
			className="process-hover-popover"
			role="tooltip"
			aria-label={entry.label}
			data-placement={position.placement}
			style={{ top: `${position.top}px`, left: `${position.left}px`, maxHeight: `${position.maxHeight}px` }}
			onMouseEnter={onEnter}
			onMouseLeave={onLeave}
		>
			<div className="process-hover-popover-heading">{entry.kind === 'reasoning' ? 'Reasoning details' : `${entry.label} details`}</div>
			{entry.content}
		</div>
	)
	return typeof document === 'undefined' ? null : createPortal(popup, document.body)
}

type ViewportRect = Pick<DOMRect, 'top' | 'bottom' | 'left' | 'right' | 'width' | 'height'>
type PopoverPosition = { top: number; left: number; maxHeight: number; placement: 'above' | 'below' }

export function calculatePopoverPosition(trigger: ViewportRect, popup?: ViewportRect): PopoverPosition {
	const viewportWidth = typeof window === 'undefined' ? 1024 : Math.max(1, window.innerWidth || document.documentElement.clientWidth || 1024)
	const viewportHeight = typeof window === 'undefined' ? 768 : Math.max(1, window.innerHeight || document.documentElement.clientHeight || 768)
	const margin = 12
	const gap = 8
	const fallbackWidth = Math.max(1, Math.min(680, viewportWidth - margin * 2))
	const fallbackHeight = Math.max(1, Math.min(520, viewportHeight - margin * 2))
	const popupWidth = popup?.width ? Math.max(1, Math.min(popup.width, viewportWidth - margin * 2)) : fallbackWidth
	const triggerWidth = trigger.width || Math.max(0, trigger.right - trigger.left)
	const triggerHeight = trigger.height || Math.max(0, trigger.bottom - trigger.top)
	const center = trigger.top + triggerHeight / 2
	const placement = center < viewportHeight / 2 ? 'below' : 'above'
	const sideSpace = placement === 'below'
		? viewportHeight - margin - trigger.bottom - gap
		: trigger.top - gap - margin
	const maxHeight = Math.max(1, Math.min(fallbackHeight, sideSpace))
	const popupHeight = popup?.height ? Math.min(popup.height, maxHeight) : maxHeight
	// Do not clamp across the trigger. The selected side owns both the top
	// coordinate and the available height; overflow inside the popup is safer
	// than overlapping the row that opened it.
	const top = placement === 'below' ? trigger.bottom + gap : trigger.top - gap - popupHeight
	const desiredLeft = trigger.left + (triggerWidth - popupWidth) / 2
	const left = clamp(desiredLeft, margin, Math.max(margin, viewportWidth - popupWidth - margin))
	return { top, left, maxHeight, placement }
}

function clamp(value: number, minimum: number, maximum: number): number {
	return Math.min(maximum, Math.max(minimum, Number.isFinite(value) ? value : minimum))
}

// ReasoningStep exposes only the compact summary in the list. The full text is
// rendered by the shared body portal, so no inline details can be clipped by
// the virtual scroller.
function ReasoningStep({ step, marker, streaming }: { step: ReasoningActivity; marker?: ReactNode; streaming: boolean }) {
	const [nowMS, setNowMS] = useState(() => Date.now())
	const hover = useContext(ProcessHoverContext)
	const triggerRef = useRef<HTMLDivElement>(null)
	const id = `reasoning-${step.id}`
	const durationMS = reasoningDurationMS(step.reasoningTiming, streaming, nowMS)
	const content = useMemo(() => <div className="process-hover-reasoning"><pre>{step.text || 'No reasoning text was recorded.'}</pre></div>, [step.text])

	useEffect(() => {
		const startedAt = parseTimestamp(step.reasoningTiming?.startedAt)
		const endedAt = parseTimestamp(step.reasoningTiming?.endedAt)
		if (!streaming || startedAt === undefined || endedAt !== undefined) return undefined
		const timer = window.setInterval(() => setNowMS(Date.now()), 250)
		return () => window.clearInterval(timer)
	}, [step.reasoningTiming?.endedAt, step.reasoningTiming?.startedAt, streaming])

	useEffect(() => {
		if (triggerRef.current) hover?.update({ id, kind: 'reasoning', trigger: triggerRef.current, content, label: 'Reasoning' })
	}, [content, hover, id])
	const show = () => {
		if (triggerRef.current) hover?.show({ id, kind: 'reasoning', trigger: triggerRef.current, content, label: 'Reasoning' })
	}
	const leave = () => {
		if (triggerRef.current) hover?.leaveTrigger(triggerRef.current)
	}
	const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
		if (event.key === 'Escape') {
			event.preventDefault()
			hover?.close()
		} else if (event.key === 'Enter' || event.key === ' ') {
			event.preventDefault()
			show()
		}
	}

	return (
		<div className="reasoning-step">
			<div
				ref={triggerRef}
				className="reasoning-trigger"
				role="button"
				tabIndex={0}
				aria-expanded={hover?.activeID === id}
				aria-describedby={hover?.activeID === id ? processHoverPopoverID : undefined}
				onMouseEnter={show}
				onMouseLeave={leave}
				onFocus={show}
				onBlur={leave}
				onKeyDown={handleKeyDown}
			>
				{streaming && <ActivityStatusDot className="reasoning-status-dot running" label="Reasoning status: Thinking" />}
				{marker}
				<span className="reasoning-summary-status">{streaming ? 'Thinking' : 'Thinking complete'}</span>
				<span className="reasoning-summary-meta">· {unicodeCodePointLength(step.text).toLocaleString()} chars · {durationMS === undefined ? '—' : formatDuration(durationMS)}</span>
			</div>
		</div>
	)
}

function parseTimestamp(value: string | undefined): number | undefined {
	if (!value) return undefined
	const parsed = Date.parse(value)
	return Number.isFinite(parsed) ? parsed : undefined
}

function reasoningDurationMS(timing: ReasoningActivity['reasoningTiming'], streaming: boolean, nowMS: number): number | undefined {
	const startedAt = parseTimestamp(timing?.startedAt)
	if (startedAt === undefined) return undefined
	const endedAt = parseTimestamp(timing?.endedAt)
	const finishAt = endedAt ?? (streaming ? nowMS : undefined)
	if (finishAt === undefined || finishAt < startedAt) return undefined
	return finishAt - startedAt
}

function ActivityStatusDot({ className, label }: { className: string; label: string }) {
	return <span className={`process-status-dot ${className}`} role="img" aria-label={label} />
}

function ToolRow({ tool, marker, onCancelTool, sessionNames, workspaceRoot }: { tool: ToolActivity; marker?: ReactNode; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
	const hover = useContext(ProcessHoverContext)
	const triggerRef = useRef<HTMLDivElement>(null)
	const argumentsObject = parseToolArguments(tool.arguments)
	const isSessionTool = isSessionToolName(tool.name)
	const target = isSessionTool ? sessionToolTarget(tool.name, argumentsObject, sessionNames) : toolTarget(tool.name, argumentsObject)
	const command = tool.name === 'shell' ? stringField(argumentsObject, 'command') : ''
	const patch = tool.name === 'apply_patch' ? stringField(argumentsObject, 'patch') : ''
	const oldText = tool.name === 'edit_file' ? stringField(argumentsObject, 'old') : ''
	const newText = tool.name === 'edit_file' ? stringField(argumentsObject, 'new') : ''
	const showEditDiff = tool.name === 'edit_file' && Boolean(oldText)
	const showPatch = Boolean(patch)
	// Session tools have no file/command affordance of their own; their hover
	// body shows the exact request arguments and the (JSON) result, so both
	// sides of the orchestration call stay inspectable.
	// Specialized views remain concise for shell/patch/edit tools. Other tools
	// expose their exact arguments instead of producing an empty popup.
	const showArguments = Boolean(tool.arguments) && !command && !showPatch && !showEditDiff
	const result = isSessionTool && tool.result ? prettyJSONText(tool.result) : tool.result
	const showResult = Boolean(result)
	// Full access sessions may address files outside the workspace; those
	// targets are flagged so out-of-workspace reads and writes stay visible.
	const outsideTargets = workspaceRoot ? toolOutsideTargets(tool.name, argumentsObject, workspaceRoot) : []
	const outside = outsideTargets.length > 0
	const details = useMemo(() => (
		<div className="tool-details">
			{command && <div><span>Command</span><pre>{command}</pre></div>}
			{showPatch && <AppliedPatchDiff patch={patch} />}
			{showEditDiff && <EditFileDiff path={target} oldText={oldText} newText={newText} />}
			{showArguments && <div><span>Arguments</span><pre>{prettyJSONText(tool.arguments ?? '')}</pre></div>}
			{showResult && <div><span>{tool.name === 'edit_file' ? 'Error details' : 'Output'}</span><pre>{result}</pre></div>}
			{!command && !showPatch && !showEditDiff && !showArguments && !showResult && <div className="tool-details-empty">No additional details were recorded.</div>}
		</div>
	), [command, newText, oldText, patch, result, showArguments, showEditDiff, showPatch, showResult, target, tool.arguments, tool.name])
	const id = `tool-${tool.id}`
	const show = () => {
		if (triggerRef.current) hover?.show({ id, kind: 'tool', trigger: triggerRef.current, content: details, label: tool.name })
	}
	const leave = () => {
		if (triggerRef.current) hover?.leaveTrigger(triggerRef.current)
	}
	const cancelButton = tool.status === 'running' && onCancelTool
		? <button className="tool-cancel-button" onClick={() => onCancelTool(tool.id)} onFocus={show} onBlur={leave} title="Cancel this tool call" aria-label="Cancel tool">×</button>
		: null
	const header = <><ActivityStatusDot className={`tool-status-dot ${tool.status}`} label={`Tool status: ${toolStatus(tool.status)}`} />{marker}<ToolIcon /><strong>{tool.name}</strong>{target && <code title={target} className={outside ? 'outside-workspace' : undefined}>{target}</code>}{outside && <span className="outside-workspace-flag" title={`Outside workspace: ${outsideTargets.join(', ')}`}>!</span>}</>
	useEffect(() => {
		if (triggerRef.current) hover?.update({ id, kind: 'tool', trigger: triggerRef.current, content: details, label: tool.name })
	}, [details, hover, id, tool.name])
	const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
		if (event.key === 'Escape') {
			event.preventDefault()
			hover?.close()
		} else if (event.key === 'Enter' || event.key === ' ') {
			event.preventDefault()
			show()
		}
	}
	return (
		<div className={`tool-row ${tool.status}`}>
			<div
				className="tool-row-main"
				onMouseEnter={show}
				onMouseLeave={leave}
			>
				<div
					ref={triggerRef}
					className="tool-row-header"
					role="button"
					tabIndex={0}
					aria-expanded={hover?.activeID === id}
					aria-describedby={hover?.activeID === id ? processHoverPopoverID : undefined}
					onMouseEnter={show}
					onFocus={show}
					onBlur={leave}
					onKeyDown={handleKeyDown}
				>
					{header}
				</div>
				{cancelButton}
			</div>
		</div>
  )
}

function AppliedPatchDiff({ patch }: { patch: string }) {
	const lines = patch.split('\n')
	return (
		<div className="tool-apply-patch">
			<span>Patch</span>
			<pre aria-label="apply_patch content">{lines.map((line, index) => <span className={patchDiffLineClass(line)} key={`${index}-${line}`}>{`${line}${index < lines.length - 1 ? '\n' : ''}`}</span>)}</pre>
		</div>
	)
}

function patchDiffLineClass(line: string): string {
	if (line.startsWith('*** ') || line.startsWith('--- ') || line.startsWith('+++ ') || line.startsWith('@@')) return 'diff-meta'
	if (line.startsWith('+')) return 'diff-added'
	if (line.startsWith('-')) return 'diff-removed'
	return 'diff-context'
}

function EditFileDiff(props: { path: string; oldText: string; newText: string }) {
	const lines = editFileDiffLines(props.oldText, props.newText)
	return (
		<div className="tool-edit-diff">
			<span>Changes</span>
			<pre aria-label={`Diff of ${props.path}`}><span className="diff-meta">{`--- ${props.path}\n+++ ${props.path}\n@@\n`}</span>{lines.map((line, index) => <span className={`diff-${line.kind}`} key={`${line.kind}-${index}`}>{`${diffPrefix(line.kind)}${line.text}${index < lines.length - 1 ? '\n' : ''}`}</span>)}</pre>
		</div>
	)
}

type EditDiffLine = { kind: 'context' | 'removed' | 'added'; text: string }

function editFileDiffLines(oldText: string, newText: string): EditDiffLine[] {
	const oldLines = splitEditDiffLines(oldText)
	const newLines = splitEditDiffLines(newText)
	let prefixLength = 0
	for (; prefixLength < oldLines.length && prefixLength < newLines.length && oldLines[prefixLength] === newLines[prefixLength]; prefixLength++) {
		// Shared prefix is displayed as diff context.
	}
	let suffixLength = 0
	for (; suffixLength < oldLines.length - prefixLength && suffixLength < newLines.length - prefixLength && oldLines[oldLines.length - suffixLength - 1] === newLines[newLines.length - suffixLength - 1]; suffixLength++) {
		// Shared suffix is displayed as diff context.
	}
	return [
		...oldLines.slice(0, prefixLength).map((text) => ({ kind: 'context' as const, text })),
		...oldLines.slice(prefixLength, oldLines.length - suffixLength).map((text) => ({ kind: 'removed' as const, text })),
		...newLines.slice(prefixLength, newLines.length - suffixLength).map((text) => ({ kind: 'added' as const, text })),
		...oldLines.slice(oldLines.length - suffixLength).map((text) => ({ kind: 'context' as const, text })),
	]
}

function splitEditDiffLines(text: string): string[] {
	return text === '' ? [] : text.split('\n')
}

function diffPrefix(kind: EditDiffLine['kind']): string {
	return kind === 'removed' ? '-' : kind === 'added' ? '+' : ' '
}

// toolOutsideTargets lists the file paths a tool call addresses outside the
// workspace. Only path-taking file tools are classified; shell commands and
// session tools are not parsed for paths.
function toolOutsideTargets(name: string, argumentsObject: Record<string, unknown>, workspaceRoot: string): string[] {
	switch (name) {
		case 'read_file':
		case 'write_file':
		case 'edit_file':
		case 'list_files':
		case 'glob_files':
		case 'grep_files': {
			const path = stringField(argumentsObject, 'path')
			return path && isPathOutsideWorkspace(workspaceRoot, path) ? [path] : []
		}
		case 'apply_patch':
			return stringField(argumentsObject, 'patch')
				.split('\n')
				.map((line) => /^\*\*\* (?:Add File|Update File|Delete File|Move to):\s*(.+)$/.exec(line)?.[1].trim() ?? '')
				.filter((path) => path && isPathOutsideWorkspace(workspaceRoot, path))
		default:
			return []
	}
}

function parseToolArguments(value?: string): Record<string, unknown> {
	if (!value) return {}
	try {
		const parsed: unknown = JSON.parse(value)
		return parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed as Record<string, unknown> : {}
	} catch {
		return {}
	}
}

function stringField(value: Record<string, unknown>, key: string): string {
	return typeof value[key] === 'string' ? value[key] : ''
}

function numberField(value: Record<string, unknown>, key: string): number | undefined {
	return typeof value[key] === 'number' ? value[key] : undefined
}

function toolTarget(name: string, argumentsObject: Record<string, unknown>): string {
	const path = stringField(argumentsObject, 'path')
	if (name === 'list_files') return path || '.'
	if (name === 'glob_files') {
		const pattern = stringField(argumentsObject, 'pattern')
		return [path, pattern && `pattern: ${pattern}`].filter(Boolean).join(' · ')
	}
	if (name === 'grep_files') {
		const query = stringField(argumentsObject, 'query')
		const mode = argumentsObject.literal === true ? 'literal' : 'regex'
		return [path, query && `${mode}: ${query}`].filter(Boolean).join(' · ')
	}
	if (name === 'read_file') {
		const startLine = numberField(argumentsObject, 'start_line')
		const lineCount = numberField(argumentsObject, 'line_count')
		if (startLine === undefined) return path
		return lineCount === undefined ? `${path}:${startLine}` : `${path}:${startLine}+${lineCount}`
	}
	if (path) return path
	if (name === 'shell') {
		const command = stringField(argumentsObject, 'command').replace(/\s+/g, ' ').trim()
		return command.length > 100 ? `${command.slice(0, 100)}…` : command
	}
	return stringField(argumentsObject, 'pattern') || stringField(argumentsObject, 'query')
}

function toolStatus(status: ToolActivity['status']): string {
  return { requested: 'Requested', running: 'Running', finished: 'Done', error: 'Failed' }[status]
}
