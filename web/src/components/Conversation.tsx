import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../api'
import type { ActiveRun, ItemsPage, QueuedPrompt, RunStep, Session, SessionImageAttachment, SessionItem, ToolActivity } from '../types'
import { blobAsDataURL, copyText, formatTokenCount } from '../lib/format'
import { itemText, processKey, sessionName } from '../lib/session'
import { Composer } from './Composer'
import type { ComposerDraft, PastedImageAttachment, PastedTextAttachment } from './Composer'
import { MessageSkeleton } from './misc'
import { ProcessTimeline } from './ProcessTimeline'
import { CopyIcon, SparkIcon } from './icons'

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
  onLoadOlder: () => Promise<boolean>
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
  onRemoveQueuedPrompt: (promptID: string) => void
  onCompact: () => void
}) {
  const bottomRef = useRef<HTMLDivElement>(null)
	const messagesRef = useRef<HTMLElement>(null)
	const loadOlderRef = useRef<HTMLButtonElement>(null)
	const followOutputRef = useRef(true)
	const loadingOlderRef = useRef(false)
	const prependAnchorRef = useRef<{
		sessionID: string
		scrollHeight: number
		scrollTop: number
		oldestSeq: number
		itemCount: number
	} | null>(null)
	const [loadingOlder, setLoadingOlder] = useState(false)
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
	const loadOlder = useCallback(async () => {
		const messages = messagesRef.current
		if (!messages || loadingOlderRef.current || !props.page?.has_more_before) return
		loadingOlderRef.current = true
		setLoadingOlder(true)
		prependAnchorRef.current = {
			sessionID: props.detail?.id ?? '',
			scrollHeight: messages.scrollHeight,
			scrollTop: messages.scrollTop,
			oldestSeq: props.page.oldest_seq,
			itemCount: props.page.items.length,
		}
		try {
			if (!await props.onLoadOlder()) prependAnchorRef.current = null
		} finally {
			loadingOlderRef.current = false
			setLoadingOlder(false)
		}
	}, [props.detail?.id, props.onLoadOlder, props.page])
	useLayoutEffect(() => {
		const anchor = prependAnchorRef.current
		const messages = messagesRef.current
		if (!anchor || !messages) return
		if (anchor.sessionID !== (props.detail?.id ?? '')) {
			prependAnchorRef.current = null
			return
		}
		if (anchor.oldestSeq === props.page?.oldest_seq && anchor.itemCount === (props.page?.items.length ?? 0) && props.page?.has_more_before) return
		messages.scrollTop = anchor.scrollTop + messages.scrollHeight - anchor.scrollHeight
		prependAnchorRef.current = null
	}, [props.detail?.id, props.page?.has_more_before, props.page?.items.length, props.page?.oldest_seq])
	useEffect(() => {
		const messages = messagesRef.current
		const button = loadOlderRef.current
		if (!messages || !button || loadingOlder || !props.page?.has_more_before || typeof IntersectionObserver === 'undefined') return
		const observer = new IntersectionObserver((entries) => {
			if (entries.some((entry) => entry.isIntersecting)) void loadOlder()
		}, { root: messages, threshold: 0.01 })
		observer.observe(button)
		return () => observer.disconnect()
	}, [loadOlder, loadingOlder, props.page?.has_more_before])
	const visibleItems = props.activeRun?.turnID
		? (props.page?.items ?? []).filter((item) =>
			item.turn_id !== props.activeRun?.turnID || (props.activeRun?.restored && item.message?.role === 'user'))
		: (props.page?.items ?? [])

  return (
    <div className="conversation">
      <header className="conversation-header">
        <div className="conversation-heading">
          <h1>{props.detail ? sessionName(props.detail) : 'Loading…'}</h1>
		  {props.detail && (
			<div className="conversation-meta">
			  <p>{props.detail.provider} / {props.detail.model_id}</p>
			  <ContextUsage context={props.detail.context} activeInputTokens={props.activeRun?.inputTokens} activeCachedTokens={props.activeRun?.cachedTokens} activeCacheWriteTokens={props.activeRun?.cacheWriteTokens} />
			</div>
		  )}
        </div>
        <div className="header-actions">
		  <span className={`status-pill ${props.activeRun || props.otherSessionsRunning ? 'running' : ''}`}><span />{props.activeRun ? 'Running' : props.otherSessionsRunning ? 'Another session running' : 'Ready'}</span>
		  <button className="secondary-button" disabled={!props.detail || props.detail.status === 'running' || Boolean(props.activeRun)} onClick={props.onCompact}>Compact context</button>
        </div>
      </header>
      <section ref={messagesRef} className="messages" aria-live="polite" onScroll={updateFollowOutput}>
        {props.page?.has_more_before && <button ref={loadOlderRef} className="load-older" disabled={loadingOlder} onClick={() => void loadOlder()}>{loadingOlder ? 'Loading earlier messages…' : 'Load earlier messages'}</button>}
        {!props.page && <MessageSkeleton />}
				{buildConversationEntries(visibleItems, props.detail?.id ?? '', props.recentStepsByTurn).map((entry) => entry.kind === 'message'
					? <Message key={entry.item.id} item={entry.item} sessionID={props.detail?.id ?? ''} />
					: <HistoricalProcess key={entry.id} entry={entry} />)}
        {props.activeRun && <ActiveRunView run={props.activeRun} />}
		{props.page && visibleItems.length === 0 && !props.activeRun && (
          <div className="conversation-empty"><SparkIcon /><h3>Start a new task</h3><p>Describe a goal, a problem, or the code you want to change.</p></div>
        )}
        <div ref={bottomRef} />
      </section>
	  <QueuedPromptList prompts={props.activeRun?.queuedPrompts ?? []} onRemove={props.onRemoveQueuedPrompt} />
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

function ContextUsage(props: { context: Session['context']; activeInputTokens?: number; activeCachedTokens?: number; activeCacheWriteTokens?: number }) {
	const context = props.context
	const contextWindow = Number(context?.context_window ?? 0)
	if (contextWindow <= 0) return null

	// Usage buckets are disjoint: input tokens exclude cache reads and writes.
	// The meter tracks the full prompt, so add the cache buckets back.
	const liveInputTokens = Number(props.activeInputTokens ?? 0)
	const livePromptTokens = liveInputTokens > 0
		? liveInputTokens + Number(props.activeCachedTokens ?? 0) + Number(props.activeCacheWriteTokens ?? 0)
		: 0
	const recordedInputTokens = Number(context?.last_input_tokens ?? 0)
	const recordedPromptTokens = recordedInputTokens > 0
		? recordedInputTokens + Number(context?.last_cached_tokens ?? 0) + Number(context?.last_cache_write_tokens ?? 0)
		: 0
	const requestEstimate = Number(context?.last_request_tokens ?? 0)
	const usedTokens = livePromptTokens > 0 ? livePromptTokens : recordedPromptTokens > 0 ? recordedPromptTokens : requestEstimate
	const usageEstimated = livePromptTokens <= 0 && (recordedPromptTokens <= 0 || context?.last_usage_source !== 'provider')
	const percent = usedTokens > 0 ? usedTokens / contextWindow * 100 : 0
	const warningThreshold = Number(context?.warning_threshold_percent ?? 80)
	const tone = percent >= 100 ? 'critical' : percent >= warningThreshold ? 'warning' : ''
	const progress = Math.min(100, Math.max(0, percent))
	const percentLabel = `${usageEstimated && usedTokens > 0 ? '~' : ''}${Math.round(percent)}%`
	const usageSource = usedTokens <= 0 ? 'No usage data yet' : usageEstimated ? 'Usage estimated locally' : 'Usage reported by the model'
	const windowSource = context?.context_window_source === 'configured' ? 'Window from model config' : 'Window is a default estimate'
	const cacheDetails = [
		Number(context?.last_cached_tokens ?? 0) > 0 ? `Cache hit ${Number(context?.last_cached_tokens).toLocaleString()}` : '',
		Number(context?.last_cache_write_tokens ?? 0) > 0 ? `Cache write ${Number(context?.last_cache_write_tokens).toLocaleString()}` : '',
		Number(context?.last_reasoning_tokens ?? 0) > 0 ? `Reasoning ${Number(context?.last_reasoning_tokens).toLocaleString()}` : '',
	].filter(Boolean).join('; ')
	const title = `Context: ${usedTokens.toLocaleString()} / ${contextWindow.toLocaleString()} tokens (${percent.toFixed(1)}%)\n${usageSource}; ${windowSource}${cacheDetails ? `\n${cacheDetails} tokens` : ''}`

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
				aria-label="Context usage"
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
      <div className="message-content">
        {role === 'user' && text && <div className="message-text">{text}</div>}
        {role === 'user' && images.length > 0 && <StoredImageAttachments sessionID={sessionID} images={images} />}
        {role !== 'user' && text && <MarkdownMessage text={text} />}
		{role === 'assistant' && (
			<div className="message-tools" aria-label="Message actions">
				<button className="message-tool-button" onClick={() => void copyMessage()} title="Copy full output">
					<CopyIcon />{copyStatus === 'copied' ? 'Copied' : copyStatus === 'error' ? 'Copy failed' : 'Copy'}
				</button>
			</div>
		)}
      </div>
    </article>
  )
}

function StoredImageAttachments(props: { sessionID: string; images: SessionImageAttachment[] }) {
  return (
    <div className="message-image-grid" aria-label="Attached images">
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

  if (failed) return <div className="message-image-unavailable">Image unavailable</div>
  if (!dataURL) return <div className="message-image-loading">Loading image…</div>
  return <img className="message-image" src={dataURL} alt={`Attached image (${props.image.media_type})`} />
}

function QueuedPromptList({ prompts, onRemove }: { prompts: QueuedPrompt[]; onRemove: (promptID: string) => void }) {
  if (prompts.length === 0) return null
  return (
    <div className="queued-prompt-list" aria-label="Queued messages">
      {prompts.map((prompt) => (
        <div className="queued-prompt-row" key={prompt.id}>
          <span className="queued-prompt-badge">Queued</span>
          <span className="queued-prompt-text" title={prompt.content}>{prompt.content}</span>
          <button
            type="button"
            className="queued-prompt-remove"
            onClick={() => onRemove(prompt.id)}
            aria-label="Remove queued message"
            title="Remove"
          >×</button>
        </div>
      ))}
    </div>
  )
}

function ActiveRunView({ run }: { run: ActiveRun }) {
  return (
    <>
      {(run.userText || (run.userImages?.length ?? 0) > 0) && (
        <article className="message user transient">
          <div className="message-content">
            {run.userText && <div className="message-text">{run.userText}</div>}
            {(run.userImages?.length ?? 0) > 0 && (
              <div className="message-image-grid" aria-label="Attached images">
                {run.userImages?.map((image, index) => <img className="message-image" src={image.data_url} alt={`Image to send #${index + 1}`} key={`${image.data_url}-${index}`} />)}
              </div>
            )}
          </div>
        </article>
      )}
      <ActiveRunBody run={run} />
    </>
  )
}

// ActiveRunBody renders the in-flight turn. Mid-turn appended user messages
// interrupt the assistant process: the steps gathered before an appended user
// message form one assistant segment, the appended message renders as its own
// user bubble, and the remaining steps continue in a following assistant
// segment. The streaming text and token note live in the trailing segment.
function ActiveRunBody({ run }: { run: ActiveRun }) {
  const segments: Array<{ kind: 'steps'; steps: RunStep[] } | { kind: 'user'; step: Extract<RunStep, { kind: 'user' }> }> = []
  let current: RunStep[] = []
  for (const step of run.steps) {
    if (step.kind === 'user') {
      if (current.length > 0) segments.push({ kind: 'steps', steps: current })
      current = []
      segments.push({ kind: 'user', step })
    } else {
      current.push(step)
    }
  }
  if (current.length > 0) segments.push({ kind: 'steps', steps: current })

  const trailing = run.assistantText || run.totalTokens !== undefined || segments.length === 0
  const tokenNote = run.totalTokens !== undefined && (
    <div className="token-note">
      This turn: {run.totalTokens.toLocaleString()} tokens
      {Boolean(run.cachedTokens) && ` · Cache hit ${run.cachedTokens?.toLocaleString()}`}
      {Boolean(run.cacheWriteTokens) && ` · Cache write ${run.cacheWriteTokens?.toLocaleString()}`}
      {Boolean(run.reasoningTokens) && ` · Reasoning ${run.reasoningTokens?.toLocaleString()}`}
    </div>
  )

  return (
    <>
      {segments.map((segment, index) => {
        const isLast = index === segments.length - 1
        if (segment.kind === 'user') {
          return (
            <article className="message user transient" key={segment.step.id}>
              <div className="message-content">
                <div className="message-text">{segment.step.text}</div>
              </div>
            </article>
          )
        }
        return (
          <article className="message assistant transient" key={`steps-${index}`}>
            <div className="message-content">
              {isLast && <div className="message-meta"><span className="streaming-label"><i />Generating</span></div>}
              <ProcessTimeline steps={segment.steps} live={isLast && run.status === 'running'} />
              {isLast && trailing && (run.assistantText ? <MarkdownMessage text={run.assistantText} streaming /> : <div className="message-text assistant-stream"><span className="cursor" /></div>)}
              {isLast && tokenNote}
            </div>
          </article>
        )
      })}
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
			const itemTurnID = item.turn_id || ''
			// A user item sharing the in-progress process turn id is a mid-turn
			// appended message: it interrupts the process. Flush the steps gathered
			// so far into their own process entry, render the user message as a
			// regular bubble, then keep accumulating the rest of the same turn into
			// a fresh process entry.
			if (processTurnID && itemTurnID && itemTurnID === processTurnID) {
				flushProcess(processTurnID)
				processTurnID = itemTurnID
				entries.push({ kind: 'message', item })
				continue
			}
			flushProcess(processTurnID)
			processTurnID = itemTurnID
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
	return (
		<article className="message assistant process-message">
			<div className="message-content">
				<ProcessTimeline steps={entry.steps} />
			</div>
		</article>
	)
}
