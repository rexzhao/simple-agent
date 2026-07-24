import { useEffect, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../api'
import type { ActiveRun, ItemsPage, RunStep, Session, SessionImageAttachment, SessionItem, ToolActivity } from '../types'
import { blobAsDataURL, copyText, formatTime, formatTokenCount } from '../lib/format'
import { itemText, processKey, sessionName } from '../lib/session'
import { Composer } from './Composer'
import type { ComposerDraft, PastedImageAttachment, PastedTextAttachment } from './Composer'
import { MessageSkeleton } from './misc'
import { ProcessTimeline } from './ProcessTimeline'
import { CopyIcon, LogoIcon, SparkIcon } from './icons'

const autoScrollThresholdPX = 160

export function Conversation(props: {
  detail: Session | null
  page: ItemsPage | null
  activeRun: ActiveRun | null
	draft: ComposerDraft
	onDraftChange: (content: string) => void
	onPastedTextAdd: (pastedText: PastedTextAttachment) => void
	onPastedTextRemove: (pastedTextID: number) => void
	onPastedImageAdd: (pastedImage: PastedImageAttachment) => void
	onPastedImageRemove: (pastedImageID: number) => void
	onDraftClear: () => void
	otherSessionsRunning: boolean
	recentStepsByTurn: Record<string, RunStep[]>
  onLoadOlder: () => void
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
  onCompact: () => void
}) {
  const bottomRef = useRef<HTMLDivElement>(null)
	const messagesRef = useRef<HTMLElement>(null)
	const followOutputRef = useRef(true)
	useEffect(() => {
		followOutputRef.current = true
		bottomRef.current?.scrollIntoView({ behavior: 'auto' })
	}, [props.detail?.id])
  useEffect(() => {
		if (followOutputRef.current) bottomRef.current?.scrollIntoView({ behavior: 'auto' })
	}, [props.activeRun?.assistantText, props.activeRun?.steps, props.page?.newest_seq])
	const updateFollowOutput = () => {
		const messages = messagesRef.current
		if (!messages) return
		followOutputRef.current = messages.scrollHeight - messages.scrollTop - messages.clientHeight <= autoScrollThresholdPX
	}
	const visibleItems = props.activeRun?.turnID
		? (props.page?.items ?? []).filter((item) =>
			item.turn_id !== props.activeRun?.turnID || (props.activeRun?.restored && item.message?.role === 'user'))
		: (props.page?.items ?? [])

  return (
    <div className="conversation">
      <header className="conversation-header">
        <div className="conversation-heading">
          <h1>{props.detail ? sessionName(props.detail) : '加载中…'}</h1>
		  {props.detail && (
			<div className="conversation-meta">
			  <p>{props.detail.provider} / {props.detail.model_id}</p>
			  <ContextUsage context={props.detail.context} activeInputTokens={props.activeRun?.inputTokens} />
			</div>
		  )}
        </div>
        <div className="header-actions">
		  <span className={`status-pill ${props.activeRun || props.otherSessionsRunning ? 'running' : ''}`}><span />{props.activeRun ? '运行中' : props.otherSessionsRunning ? '其他会话运行中' : '就绪'}</span>
		  <button className="secondary-button" disabled={!props.detail || props.detail.status === 'running' || Boolean(props.activeRun)} onClick={props.onCompact}>压缩上下文</button>
        </div>
      </header>
      <section ref={messagesRef} className="messages" aria-live="polite" onScroll={updateFollowOutput}>
        {props.page?.has_more_before && <button className="load-older" onClick={props.onLoadOlder}>加载更早消息</button>}
        {!props.page && <MessageSkeleton />}
				{buildConversationEntries(visibleItems, props.detail?.id ?? '', props.recentStepsByTurn).map((entry) => entry.kind === 'message'
					? <Message key={entry.item.id} item={entry.item} sessionID={props.detail?.id ?? ''} />
					: <HistoricalProcess key={entry.id} entry={entry} />)}
        {props.activeRun && <ActiveRunView run={props.activeRun} />}
		{props.page && visibleItems.length === 0 && !props.activeRun && (
          <div className="conversation-empty"><SparkIcon /><h3>开始一个新任务</h3><p>描述目标、问题或需要修改的代码。</p></div>
        )}
        <div ref={bottomRef} />
      </section>
	  <Composer
		draft={props.draft}
		onContentChange={props.onDraftChange}
		onPastedTextAdd={props.onPastedTextAdd}
		onPastedTextRemove={props.onPastedTextRemove}
		onPastedImageAdd={props.onPastedImageAdd}
		onPastedImageRemove={props.onPastedImageRemove}
		onDraftClear={props.onDraftClear}
		running={Boolean(props.activeRun)}
		blocked={false}
		onSend={props.onSend}
		onCancel={props.onCancel}
	  />
    </div>
  )
}

function ContextUsage(props: { context: Session['context']; activeInputTokens?: number }) {
	const context = props.context
	const contextWindow = Number(context?.context_window ?? 0)
	if (contextWindow <= 0) return null

	const liveInputTokens = Number(props.activeInputTokens ?? 0)
	const recordedInputTokens = Number(context?.last_input_tokens ?? 0)
	const requestEstimate = Number(context?.last_request_tokens ?? 0)
	const usedTokens = liveInputTokens > 0 ? liveInputTokens : recordedInputTokens > 0 ? recordedInputTokens : requestEstimate
	const usageEstimated = liveInputTokens <= 0 && (recordedInputTokens <= 0 || context?.last_usage_source !== 'provider')
	const percent = usedTokens > 0 ? usedTokens / contextWindow * 100 : 0
	const warningThreshold = Number(context?.warning_threshold_percent ?? 80)
	const tone = percent >= 100 ? 'critical' : percent >= warningThreshold ? 'warning' : ''
	const progress = Math.min(100, Math.max(0, percent))
	const percentLabel = `${usageEstimated && usedTokens > 0 ? '约 ' : ''}${Math.round(percent)}%`
	const usageSource = usedTokens <= 0 ? '尚无使用数据' : usageEstimated ? '使用量为本地估算' : '使用量来自模型返回值'
	const windowSource = context?.context_window_source === 'configured' ? '窗口来自模型配置' : '窗口为默认估算值'
	const cacheDetails = [
		Number(context?.last_cached_tokens ?? 0) > 0 ? `缓存命中 ${Number(context?.last_cached_tokens).toLocaleString()}` : '',
		Number(context?.last_cache_write_tokens ?? 0) > 0 ? `缓存写入 ${Number(context?.last_cache_write_tokens).toLocaleString()}` : '',
		Number(context?.last_reasoning_tokens ?? 0) > 0 ? `推理 ${Number(context?.last_reasoning_tokens).toLocaleString()}` : '',
	].filter(Boolean).join('；')
	const title = `上下文：${usedTokens.toLocaleString()} / ${contextWindow.toLocaleString()} tokens（${percent.toFixed(1)}%）\n${usageSource}；${windowSource}${cacheDetails ? `\n${cacheDetails} tokens` : ''}`

	return (
		<div className={`context-usage ${tone}`} title={title}>
			<div className="context-usage-copy">
				<span>Context</span>
				<strong>{formatTokenCount(usedTokens)} / {formatTokenCount(contextWindow)}</strong>
				<small>{percentLabel}</small>
			</div>
			<div
				className="context-progress"
				role="progressbar"
				aria-label="上下文使用量"
				aria-valuemin={0}
				aria-valuemax={contextWindow}
				aria-valuenow={Math.min(usedTokens, contextWindow)}
			>
				<i style={{ width: `${progress}%` }} />
			</div>
		</div>
	)
}

function Message({ item, sessionID }: { item: SessionItem; sessionID: string }) {
  const role = item.message?.role
  const text = item.message?.content?.inline || item.message?.content?.preview || ''
  const images = item.message?.images ?? []
	const [copyStatus, setCopyStatus] = useState<'idle' | 'copied' | 'error'>('idle')
  if (!text && images.length === 0) return null
	const copyMessage = async () => {
		try {
			await copyText(text)
			setCopyStatus('copied')
			window.setTimeout(() => setCopyStatus('idle'), 1600)
		} catch {
			setCopyStatus('error')
			window.setTimeout(() => setCopyStatus('idle'), 2200)
		}
	}
  return (
    <article className={`message ${role === 'user' ? 'user' : 'assistant'}`}>
      <div className="message-avatar">{role === 'user' ? '你' : <LogoIcon />}</div>
      <div className="message-content">
        <div className="message-meta"><strong>{role === 'user' ? '你' : 'SAI'}</strong><time>{formatTime(item.created_at)}</time></div>
        {role === 'user' && text && <div className="message-text">{text}</div>}
        {role === 'user' && images.length > 0 && <StoredImageAttachments sessionID={sessionID} images={images} />}
        {role !== 'user' && text && <MarkdownMessage text={text} />}
		{role === 'assistant' && (
			<div className="message-tools" aria-label="消息操作">
				<button className="message-tool-button" onClick={() => void copyMessage()} title="复制完整输出">
					<CopyIcon />{copyStatus === 'copied' ? '已复制' : copyStatus === 'error' ? '复制失败' : '复制'}
				</button>
			</div>
		)}
      </div>
    </article>
  )
}

function StoredImageAttachments(props: { sessionID: string; images: SessionImageAttachment[] }) {
  return (
    <div className="message-image-grid" aria-label="已附加图片">
      {props.images.map((image) => <StoredImageAttachment key={image.hash} sessionID={props.sessionID} image={image} />)}
    </div>
  )
}

function StoredImageAttachment(props: { sessionID: string; image: SessionImageAttachment }) {
  const [dataURL, setDataURL] = useState('')
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    void api.sessionImage(props.sessionID, props.image.hash)
      .then(blobAsDataURL)
      .then((url) => {
        if (active) setDataURL(url)
      })
      .catch(() => {
        if (active) setFailed(true)
      })
    return () => { active = false }
  }, [props.image.hash, props.sessionID])

  if (failed) return <div className="message-image-unavailable">图片不可用</div>
  if (!dataURL) return <div className="message-image-loading">加载图片…</div>
  return <img className="message-image" src={dataURL} alt={`已附加图片（${props.image.media_type}）`} />
}

function ActiveRunView({ run }: { run: ActiveRun }) {
  return (
    <>
      {(run.userText || (run.userImages?.length ?? 0) > 0) && (
        <article className="message user transient">
          <div className="message-avatar">你</div>
          <div className="message-content">
            <div className="message-meta"><strong>你</strong><span>刚刚</span></div>
            {run.userText && <div className="message-text">{run.userText}</div>}
            {(run.userImages?.length ?? 0) > 0 && (
              <div className="message-image-grid" aria-label="已附加图片">
                {run.userImages?.map((image, index) => <img className="message-image" src={image.data_url} alt={`待发送图片 #${index + 1}`} key={`${image.data_url}-${index}`} />)}
              </div>
            )}
          </div>
        </article>
      )}
      <article className="message assistant transient">
        <div className="message-avatar"><LogoIcon /></div>
        <div className="message-content">
          <div className="message-meta"><strong>SAI</strong><span className="streaming-label"><i />生成中</span></div>
					{run.steps.length > 0 && <ProcessTimeline steps={run.steps} />}
          {run.assistantText ? <MarkdownMessage text={run.assistantText} streaming /> : <div className="message-text assistant-stream"><span className="cursor" /></div>}
			{run.totalTokens !== undefined && (
				<div className="token-note">
					本轮 {run.totalTokens.toLocaleString()} tokens
					{Boolean(run.cachedTokens) && ` · 缓存命中 ${run.cachedTokens?.toLocaleString()}`}
					{Boolean(run.cacheWriteTokens) && ` · 缓存写入 ${run.cacheWriteTokens?.toLocaleString()}`}
					{Boolean(run.reasoningTokens) && ` · 推理 ${run.reasoningTokens?.toLocaleString()}`}
				</div>
			)}
        </div>
      </article>
    </>
  )
}

const markdownComponents: Components = {
  a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

function MarkdownMessage({ text, streaming = false }: { text: string; streaming?: boolean }) {
  return (
    <div className={`message-text markdown-body ${streaming ? 'assistant-stream' : ''}`}>
      <Markdown remarkPlugins={[remarkGfm]} components={markdownComponents} skipHtml>{text}</Markdown>
    </div>
  )
}

type ConversationEntry =
	| { kind: 'message'; item: SessionItem }
	| { kind: 'process'; id: string; createdAt: string; steps: RunStep[] }

function buildConversationEntries(items: SessionItem[], sessionID: string, recentStepsByTurn: Record<string, RunStep[]>): ConversationEntry[] {
	const entries: ConversationEntry[] = []
	let steps: RunStep[] = []
	let processCreatedAt = ''
	let processTurnID = ''
	let agentIteration = 0
	const emittedRecentTurns = new Set<string>()

	const flushProcess = (turnID = processTurnID) => {
		const recentKey = turnID ? processKey(sessionID, turnID) : ''
		const recentSteps = recentKey && !emittedRecentTurns.has(recentKey) ? recentStepsByTurn[recentKey] : undefined
		const displayedSteps = recentSteps?.length ? recentSteps : steps
		if (displayedSteps.length > 0) {
			entries.push({ kind: 'process', id: `process-${sessionID}-${turnID || displayedSteps[0].id}`, createdAt: processCreatedAt, steps: displayedSteps })
		}
		if (recentKey && recentSteps?.length) emittedRecentTurns.add(recentKey)
		steps = []
		processCreatedAt = ''
		processTurnID = ''
	}

	for (const item of items) {
		const role = item.message?.role
		const text = itemText(item)
		if (role === 'user') {
			flushProcess(processTurnID)
			processTurnID = item.turn_id || ''
			agentIteration = 0
			entries.push({ kind: 'message', item })
			continue
		}
		if (role === 'assistant' && (item.message?.tool_calls?.length ?? 0) > 0) {
			agentIteration = item.agent_iteration || agentIteration + 1
			if (!processCreatedAt) processCreatedAt = item.created_at
			if (!processTurnID) processTurnID = item.turn_id || ''
			if (text) steps.push({ kind: 'output', id: `${item.id}-output`, text, iteration: agentIteration })
			for (const toolCall of item.message?.tool_calls ?? []) {
				steps.push({
					kind: 'tool',
					id: toolCall.id,
					name: toolCall.name,
					iteration: agentIteration,
					arguments: toolCall.arguments,
					status: 'requested',
				})
			}
			continue
		}
		if (role === 'tool') {
			agentIteration = item.agent_iteration || agentIteration || 1
			if (!processCreatedAt) processCreatedAt = item.created_at
			if (!processTurnID) processTurnID = item.turn_id || ''
			const toolCallID = item.message?.tool_call_id || item.id
			const index = steps.findIndex((step) => step.kind === 'tool' && step.id === toolCallID)
			const status: ToolActivity['status'] = item.message?.is_error || item.status === 'error' ? 'error' : item.status === 'pending' ? 'requested' : 'finished'
			if (index >= 0) {
				const tool = steps[index] as ToolActivity
				steps[index] = { ...tool, result: text, status }
			} else {
				steps.push({ kind: 'tool', id: toolCallID, name: 'tool', iteration: agentIteration, result: text, status })
			}
			continue
		}
		if (!processCreatedAt) processCreatedAt = item.created_at
		flushProcess(item.turn_id || processTurnID)
		if (text) entries.push({ kind: 'message', item })
	}
	flushProcess(processTurnID)
	return entries
}

function HistoricalProcess({ entry }: { entry: Extract<ConversationEntry, { kind: 'process' }> }) {
	const reasoningCount = entry.steps.filter((step) => step.kind === 'reasoning').length
	const outputCount = entry.steps.filter((step) => step.kind === 'output').length
	const toolCount = entry.steps.filter((step) => step.kind === 'tool').length
	const iterationCount = new Set(entry.steps.map((step) => step.iteration)).size
	const summary = [`${iterationCount} 轮`, reasoningCount > 0 ? `${reasoningCount} 段思考` : '', outputCount > 0 ? `${outputCount} 段中间输出` : '', toolCount > 0 ? `${toolCount} 次工具调用` : ''].filter(Boolean).join(' · ')
	return (
		<article className="message assistant process-message">
			<div className="message-avatar"><LogoIcon /></div>
			<div className="message-content">
				<details className="process-card">
					<summary><span>执行过程</span><small>{summary}</small><time>{formatTime(entry.createdAt)}</time></summary>
					<ProcessTimeline steps={entry.steps} />
				</details>
			</div>
		</article>
	)
}
