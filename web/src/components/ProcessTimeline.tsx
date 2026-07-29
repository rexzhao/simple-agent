import { memo, useEffect, useRef, useState } from 'react'
import type { ReactNode, SyntheticEvent } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { ReasoningActivity, RunStep, ToolActivity } from '../types'
import { foldToolGroups } from '../lib/runSteps'
import type { FlatProcessStep } from '../lib/runSteps'
import { isSessionToolName, prettyJSONText, sessionToolPhrase, sessionToolTarget } from '../lib/sessionTools'
import { ChevronIcon, ToolIcon } from './icons'

const markdownComponents: Components = {
	a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

export const ProcessTimeline = memo(function ProcessTimeline({ steps, live = false, onCancelTool, sessionNames }: { steps: RunStep[]; live?: boolean; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string> }) {
	const nodes = foldToolGroups(steps)
	const lastNode = nodes[nodes.length - 1]
	const lastFlat = lastNode ? (lastNode.kind === 'tool-group' ? lastNode.flats[lastNode.flats.length - 1] : lastNode.flat) : undefined
	const lastStepID = lastFlat?.step.id
	return (
		<div className="process-timeline">
			{nodes.map((node, nodeIndex) => {
				if (node.kind === 'tool-group') {
					return <ToolGroupRow key={node.id} flats={node.flats} live={live && nodeIndex === nodes.length - 1} onCancelTool={onCancelTool} sessionNames={sessionNames} />
				}
				const { step, iteration, iterationStart } = node.flat
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
				if (step.kind === 'user') {
					// A mid-turn appended message renders like a regular user
					// message: light bubble on the right.
					return (
						<article className="message user step-message" key={step.id}>{marker}
							<div className="message-content">
								<div className="message-text">{step.text}</div>
							</div>
						</article>
					)
				}
				return <ToolRow key={step.id} tool={step} marker={marker} onCancelTool={onCancelTool} sessionNames={sessionNames} />
			})}
		</div>
	)
})

// ToolGroupRow renders a folded run of consecutive tool calls as one
// aggregated row. The row stays expanded while it is the live in-flight tail
// — even between tool batches — and collapses only once a run-breaking step
// seals it or the run settles. While live, it re-expands whenever a member
// tool fails; historical groups always start collapsed, failed or not — a
// finished session should not greet the reader with walls of tool output.
// Reasoning steps inside the run stay individually collapsed.
function ToolGroupRow({ flats, live, onCancelTool, sessionNames }: { flats: FlatProcessStep[]; live: boolean; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string> }) {
	const tools = flats.map((flat) => flat.step).filter((step): step is ToolActivity => step.kind === 'tool')
	const failed = tools.filter((tool) => tool.status === 'error').length
	const pending = tools.filter((tool) => tool.status === 'requested' || tool.status === 'running').length
	const active = live
	const hasError = failed > 0
	const [expanded, setExpanded] = useState(active || (live && hasError))
	useEffect(() => {
		if (live && hasError) setExpanded(true)
	}, [live, hasError])
	useEffect(() => {
		if (!active && !(live && hasError)) setExpanded(false)
	}, [active, live, hasError])

	const toggle = (event: SyntheticEvent<HTMLDetailsElement>) => {
		setExpanded(event.currentTarget.open)
	}

	// A collapsed group is marked with the latest iteration it contains, so
	// the marker tracks progress. An expanded group instead marks each round
	// on the row where the round actually begins: the first tool or
	// reasoning step of that round inside the group. Rounds that begin with
	// an output or user step outside the group keep their marker there and
	// are not repeated inside.
	let markerIteration: number | undefined
	for (const flat of flats) {
		if (flat.iterationStart) markerIteration = flat.iteration
	}
	const topMarker = expanded ? undefined : markerIteration
	const status = hasError ? 'error' : pending > 0 ? 'running' : 'finished'
	const badge = hasError ? `${failed} failed` : pending > 0 ? `${tools.length - pending}/${tools.length}` : 'Done'
	const summary = toolGroupSummary(tools)
	const targets = toolGroupTargets(tools)

	return (
		<details className={`tool-group ${status}`} open={expanded} onToggle={toggle}>
			<summary>
				{topMarker !== undefined && <i className="iteration-marker">{topMarker}</i>}
				<ChevronIcon expanded={expanded} />
				<ToolIcon />
				<span className="tool-group-summary" title={summary}>{summary}</span>
				{targets && <code title={targets}>{targets}</code>}
				<small>{badge}</small>
			</summary>
			{expanded && (
				<div className="tool-group-body">
					{flats.map(({ step, iteration, iterationStart }, index) => {
						const marker = iterationStart ? <i className="iteration-marker">{iteration}</i> : undefined
						if (step.kind === 'reasoning') {
							// Reasoning still streaming inside a live group renders
							// expanded and follows new lines, exactly like an
							// ungrouped streaming reasoning block.
							return <ReasoningStep key={step.id} step={step} marker={marker} streaming={live && index === flats.length - 1} />
						}
						if (step.kind === 'tool') return <ToolRow key={step.id} tool={step} marker={marker} onCancelTool={onCancelTool} sessionNames={sessionNames} />
						return null
					})}
				</div>
			)}
		</details>
	)
}

function toolGroupSummary(tools: ToolActivity[]): string {
	const counts = new Map<string, number>()
	for (const tool of tools) counts.set(tool.name, (counts.get(tool.name) ?? 0) + 1)
	return [...counts.entries()].map(([name, count]) => toolPhrase(name, count)).join(' · ')
}

function toolPhrase(name: string, count: number): string {
	if (isSessionToolName(name)) return sessionToolPhrase(name, count)
	const plural = count === 1 ? '' : 's'
	switch (name) {
		case 'read_file': return `Read ${count} file${plural}`
		case 'write_file': return `Wrote ${count} file${plural}`
		case 'edit_file': return `Edited ${count} file${plural}`
		case 'apply_patch': return `Applied ${count} patch${count === 1 ? '' : 'es'}`
		case 'shell': return `Ran ${count} command${plural}`
		case 'grep_files': return `Ran ${count} search${count === 1 ? '' : 'es'}`
		case 'glob_files': return `Ran ${count} glob${plural}`
		case 'list_files': return `Listed ${count} ${count === 1 ? 'directory' : 'directories'}`
		default: return count > 1 ? `${name} ×${count}` : name
	}
}

// toolGroupTargets names the files a run modified so that write operations
// stay visible even while the group is collapsed.
function toolGroupTargets(tools: ToolActivity[]): string {
	const targets: string[] = []
	for (const tool of tools) {
		for (const target of toolWriteTargets(tool)) {
			if (!targets.includes(target)) targets.push(target)
		}
	}
	return targets.join(', ')
}

function toolWriteTargets(tool: ToolActivity): string[] {
	const argumentsObject = parseToolArguments(tool.arguments)
	if (tool.name === 'apply_patch') {
		return stringField(argumentsObject, 'patch')
			.split('\n')
			.map((line) => /^\*\*\* (?:Add File|Update File|Delete File|Move to):\s*(.+)$/.exec(line)?.[1].trim() ?? '')
			.filter(Boolean)
	}
	if (tool.name === 'write_file' || tool.name === 'edit_file') {
		const target = toolTarget(tool.name, argumentsObject)
		return target ? [target] : []
	}
	return []
}

// Distance from the bottom within which a streaming reasoning block keeps
// following new lines; scrolling further up pauses the auto-scroll.
const reasoningFollowThresholdPX = 24

// ReasoningStep streams expanded, follows the latest line while the user stays
// at the bottom, and collapses by itself once the reasoning stream ends.
function ReasoningStep({ step, marker, streaming }: { step: ReasoningActivity; marker?: ReactNode; streaming: boolean }) {
	const [expanded, setExpanded] = useState(streaming)
	const preRef = useRef<HTMLPreElement>(null)
	const followRef = useRef(true)

	useEffect(() => {
		const pre = preRef.current
		if (pre && streaming && expanded && followRef.current) pre.scrollTop = pre.scrollHeight
	}, [step.text, streaming, expanded])

	useEffect(() => {
		if (!streaming) setExpanded(false)
	}, [streaming])

	const updateFollow = () => {
		const pre = preRef.current
		if (pre) followRef.current = pre.scrollHeight - pre.scrollTop - pre.clientHeight <= reasoningFollowThresholdPX
	}

	const toggle = (event: SyntheticEvent<HTMLDetailsElement>) => {
		const open = event.currentTarget.open
		if (open && streaming) followRef.current = true
		setExpanded(open)
	}

	return (
		<details className="reasoning-step" open={expanded} onToggle={toggle}>
			<summary>{marker}<ChevronIcon expanded={expanded} /><span>{step.label || 'Reasoning'}</span></summary>
			{expanded && <pre ref={preRef} onScroll={updateFollow}>{step.text}</pre>}
		</details>
	)
}

function ToolRow({ tool, marker, onCancelTool, sessionNames }: { tool: ToolActivity; marker?: ReactNode; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string> }) {
	const argumentsObject = parseToolArguments(tool.arguments)
	const isSessionTool = isSessionToolName(tool.name)
	const target = isSessionTool ? sessionToolTarget(tool.name, argumentsObject, sessionNames) : toolTarget(tool.name, argumentsObject)
	const command = tool.name === 'shell' ? stringField(argumentsObject, 'command') : ''
	const patch = tool.name === 'apply_patch' ? stringField(argumentsObject, 'patch') : ''
	const oldText = tool.name === 'edit_file' ? stringField(argumentsObject, 'old') : ''
	const newText = tool.name === 'edit_file' ? stringField(argumentsObject, 'new') : ''
	const showEditDiff = tool.name === 'edit_file' && Boolean(oldText)
	const showPatch = Boolean(patch)
	// Session tools have no file/command affordance of their own; the expanded
	// row instead shows the exact request arguments and the (JSON) result, so
	// both sides of the orchestration call stay inspectable.
	const showArguments = isSessionTool && Boolean(tool.arguments)
	const result = isSessionTool && tool.result ? prettyJSONText(tool.result) : tool.result
	const showResult = Boolean(result) && (tool.name !== 'edit_file' || tool.status === 'error')
	const showDetails = Boolean(command || showPatch || showEditDiff || showArguments || showResult)
	const cancelButton = tool.status === 'running' && onCancelTool
		? <button className="tool-cancel-button" onClick={() => onCancelTool(tool.id)} title="Cancel this tool call" aria-label="Cancel tool">×</button>
		: null
	const header = <><ToolIcon /><strong>{tool.name}</strong>{target && <code title={target}>{target}</code>}<small>{toolStatus(tool.status)}</small>{cancelButton}</>
	const details = (
		<div className="tool-details">
			{command && <div><span>Command</span><pre>{command}</pre></div>}
			{showPatch && <AppliedPatchDiff patch={patch} />}
			{showEditDiff && <EditFileDiff path={target} oldText={oldText} newText={newText} />}
			{showArguments && <div><span>Arguments</span><pre>{prettyJSONText(tool.arguments ?? '')}</pre></div>}
			{showResult && <div><span>{tool.name === 'edit_file' ? 'Error details' : 'Output'}</span><pre>{result}</pre></div>}
		</div>
	)
	if (!showDetails) {
		return <div className={`tool-row ${tool.status}`}><div className="tool-row-header">{marker}{header}</div></div>
	}
	return (
		<details className={`tool-row ${tool.status} expandable`}>
			<summary className="tool-row-header">{marker}{header}</summary>
			{details}
		</details>
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
	if (path) return path
	if (name === 'shell') {
		const command = stringField(argumentsObject, 'command').replace(/\s+/g, ' ').trim()
		return command.length > 100 ? `${command.slice(0, 100)}…` : command
	}
	return stringField(argumentsObject, 'pattern') || stringField(argumentsObject, 'query')
}

function toolStatus(status: ToolActivity['status']): string {
  return { requested: 'Pending', running: 'Running', finished: 'Done', error: 'Failed' }[status]
}
