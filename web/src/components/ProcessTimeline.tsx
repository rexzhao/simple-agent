import type { ReactNode } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { RunStep, ToolActivity } from '../types'
import { groupProcessSteps } from '../lib/runSteps'
import { ToolIcon } from './icons'

const markdownComponents: Components = {
	a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

export function ProcessTimeline({ steps }: { steps: RunStep[] }) {
	const iterations = groupProcessSteps(steps)
	return (
		<div className="process-iterations">
			{iterations.map((iteration) => (
				<section className="process-iteration" key={iteration.number}>
					<div className="process-timeline">
						{iteration.steps.map((step, stepIndex) => {
							const first = stepIndex === 0
							const marker = first ? <i className="iteration-marker">{iteration.number}</i> : null
							if (step.kind === 'reasoning') {
								return <div className="reasoning-step" key={step.id}>{marker}<span>{step.label || 'Reasoning'}</span><pre>{step.text}</pre></div>
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
								// message: avatar and name on the right, plain text.
								return (
									<article className="message user step-message" key={step.id}>{marker}
										<div className="message-avatar">You</div>
										<div className="message-content">
											<div className="message-meta"><strong>You</strong></div>
											<div className="message-text">{step.text}</div>
										</div>
									</article>
								)
							}
							return <ToolRow key={step.id} tool={step} marker={marker} />
						})}
					</div>
				</section>
			))}
		</div>
	)
}

function ToolRow({ tool, marker }: { tool: ToolActivity; marker?: ReactNode }) {
	const argumentsObject = parseToolArguments(tool.arguments)
	const target = toolTarget(tool.name, argumentsObject)
	const command = tool.name === 'shell' ? stringField(argumentsObject, 'command') : ''
	const patch = tool.name === 'apply_patch' ? stringField(argumentsObject, 'patch') : ''
	const oldText = tool.name === 'edit_file' ? stringField(argumentsObject, 'old') : ''
	const newText = tool.name === 'edit_file' ? stringField(argumentsObject, 'new') : ''
	const showEditDiff = tool.name === 'edit_file' && Boolean(oldText)
	const showPatch = Boolean(patch)
	const showResult = Boolean(tool.result) && (tool.name !== 'edit_file' || tool.status === 'error')
	const showDetails = Boolean(command || showPatch || showEditDiff || showResult)
	const header = <><ToolIcon /><strong>{tool.name}</strong>{target && <code title={target}>{target}</code>}<small>{toolStatus(tool.status)}</small></>
	const details = (
		<div className="tool-details">
			{command && <div><span>Command</span><pre>{command}</pre></div>}
			{showPatch && <AppliedPatchDiff patch={patch} />}
			{showEditDiff && <EditFileDiff path={target} oldText={oldText} newText={newText} />}
			{showResult && <div><span>{tool.name === 'edit_file' ? 'Error details' : 'Output'}</span><pre>{tool.result}</pre></div>}
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
		const mode = argumentsObject.regex === true ? 'regex' : 'text'
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
