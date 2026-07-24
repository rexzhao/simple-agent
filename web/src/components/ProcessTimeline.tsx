import type { RunStep, ToolActivity } from '../types'
import { groupProcessSteps } from '../lib/runSteps'
import { ToolIcon } from './icons'

export function ProcessTimeline({ steps }: { steps: RunStep[] }) {
	const iterations = groupProcessSteps(steps)
	return (
		<div className="process-iterations">
			{iterations.map((iteration) => (
				<section className="process-iteration" key={iteration.number}>
					<div className="process-iteration-title">第 {iteration.number} 轮</div>
					<div className="process-timeline">
						{iteration.steps.map((step) => step.kind === 'reasoning'
							? <div className="reasoning-step" key={step.id}><span>{step.label || '思考过程'}</span><pre>{step.text}</pre></div>
							: step.kind === 'output'
								? <div className="model-output-step" key={step.id}><span>Agent 中间输出</span><pre>{step.text}</pre></div>
								: <ToolRow key={step.id} tool={step} />)}
					</div>
				</section>
			))}
		</div>
	)
}

function ToolRow({ tool }: { tool: ToolActivity }) {
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
	const header = <><ToolIcon /><strong>{toolDisplayName(tool.name)}</strong>{target && <code title={target}>{target}</code>}<small>{toolStatus(tool.status)}</small></>
	const details = (
		<div className="tool-details">
			{command && <div><span>命令</span><pre>{command}</pre></div>}
			{showPatch && <AppliedPatchDiff patch={patch} />}
			{showEditDiff && <EditFileDiff path={target} oldText={oldText} newText={newText} />}
			{showResult && <div><span>{tool.name === 'edit_file' ? '错误详情' : '输出'}</span><pre>{tool.result}</pre></div>}
		</div>
	)
	if (!showDetails) {
		return <div className={`tool-row ${tool.status}`}><div className="tool-row-header">{header}</div></div>
	}
	return (
		<details className={`tool-row ${tool.status} expandable`}>
			<summary className="tool-row-header">{header}</summary>
			{details}
		</details>
  )
}

function AppliedPatchDiff({ patch }: { patch: string }) {
	const lines = patch.split('\n')
	return (
		<div className="tool-apply-patch">
			<span>补丁</span>
			<pre aria-label="apply_patch 内容">{lines.map((line, index) => <span className={patchDiffLineClass(line)} key={`${index}-${line}`}>{`${line}${index < lines.length - 1 ? '\n' : ''}`}</span>)}</pre>
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
			<span>变更</span>
			<pre aria-label={`编辑 ${props.path} 的差异`}><span className="diff-meta">{`--- ${props.path}\n+++ ${props.path}\n@@\n`}</span>{lines.map((line, index) => <span className={`diff-${line.kind}`} key={`${line.kind}-${index}`}>{`${diffPrefix(line.kind)}${line.text}${index < lines.length - 1 ? '\n' : ''}`}</span>)}</pre>
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
		return [path, pattern && `模式: ${pattern}`].filter(Boolean).join(' · ')
	}
	if (name === 'grep_files') {
		const query = stringField(argumentsObject, 'query')
		const mode = argumentsObject.regex === true ? '正则' : '文本'
		return [path, query && `${mode}: ${query}`].filter(Boolean).join(' · ')
	}
	if (path) return path
	if (name === 'shell') {
		const command = stringField(argumentsObject, 'command').replace(/\s+/g, ' ').trim()
		return command.length > 100 ? `${command.slice(0, 100)}…` : command
	}
	return stringField(argumentsObject, 'pattern') || stringField(argumentsObject, 'query')
}

function toolDisplayName(name: string): string {
	return {
		read_file: '读取文件',
		write_file: '写入文件',
		edit_file: '编辑文件',
		list_files: '列出文件',
		glob_files: '查找文件',
		grep_files: '搜索文件',
		apply_patch: '应用补丁',
		shell: 'Shell',
	}[name] || name
}

function toolStatus(status: ToolActivity['status']): string {
  return { requested: '等待', running: '执行中', finished: '完成', error: '失败' }[status]
}
