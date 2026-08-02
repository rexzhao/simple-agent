import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../api'
import type { ActiveRun, ItemsPage, QueuedPrompt, RunStep, Session, SessionImageAttachment, SessionItem, ToolActivity } from '../types'
import { addUsageBreakdown, contextRequestCount, contextUsageBreakdown, usageBreakdownFromEvents, usageCostBreakdown, usageEventCount } from '../lib/cost'
import { blobAsDataURL, copyText, formatCost, formatTokenCount } from '../lib/format'
import { itemText, processKey, sessionName, visibleSessionItems } from '../lib/session'
import { Composer } from './Composer'
import type { ComposerDraft, PastedImageAttachment, PastedTextAttachment } from './Composer'
import { MessageSkeleton } from './misc'
import { ProcessTimeline } from './ProcessTimeline'
import { CopyIcon, RetryIcon, SparkIcon, WarningIcon } from './icons'

// Distance from the bottom that still counts as "at the bottom". Kept tiny on
// purpose: any deliberate scroll away from the bottom disengages output
// following, and only scrolling back to (nearly) the bottom re-engages it.
const stickToBottomThresholdPX = 8

export const Conversation = memo(function Conversation(props: {
  sessionID: string
  detail: Session | null
  page: ItemsPage | null
  activeRun: ActiveRun | null
  compacting: boolean
	draft: ComposerDraft
	onDraftChange: (content: string) => void
	onPastedTextAdd: (pastedText: PastedTextAttachment) => void
	onPastedTextRemove: (pastedTextID: number) => void
	onPastedImageAdd: (pastedImage: PastedImageAttachment) => void
	onPastedImageRemove: (pastedImageID: number) => void
	onDraftClear: () => void
	recentStepsByTurn: Record<string, RunStep[]>
  sessionNames: Record<string, string>
  turnError: { turnID: string; message: string } | null
  onDismissTurnError: () => void
  onResend: (item: SessionItem) => Promise<boolean>
  onLoadOlder: () => Promise<boolean>
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
  onCancelTool: (toolCallID: string) => void
  onRetry: () => void
  onRetryRefresh: () => void
  onToggleFullAccess: () => void
  onRemoveQueuedPrompt: (promptID: string) => void
  onSteerQueuedPrompt: (promptID: string, steer: boolean) => void
  onMoveQueuedPrompt: (promptID: string, direction: 'up' | 'down') => void
  onCompact: () => void
}) {
	const messagesRef = useRef<HTMLElement>(null)
	const followOutputRef = useRef(true)
	const loadingOlderRef = useRef(false)
	const prependAnchorRef = useRef<{
		sessionID: string
		element: HTMLElement | null
		top: number
		oldestSeq: number
	} | null>(null)
	// Per-session scroll memory: where the user was (and whether they were
	// following output) when they left the session.
	const scrollMemoryRef = useRef(new Map<string, { following: boolean; seq: number | null; offsetPX: number }>())
	const sessionIDRef = useRef('')
	const saveMemoryFrameRef = useRef(0)
	const settleFrameRef = useRef(0)
	const settleActiveRef = useRef(false)
	const [loadingOlder, setLoadingOlder] = useState(false)
	sessionIDRef.current = props.sessionID
	const safeDetail = props.detail && props.detail.id === props.sessionID ? props.detail : null
	const [resendPending, setResendPending] = useState(false)

	// Scoped to the messages container: unlike scrollIntoView this can never
	// drag along other scrollable ancestors (window, sidebar).
	const scrollToBottom = useCallback(() => {
		const messages = messagesRef.current
		if (messages) messages.scrollTop = messages.scrollHeight
	}, [])
	// Scroll events are the single source of truth for follow intent. Landing
	// at the bottom engages following; leaving it — wheel, touch, keyboard,
	// scrollbar — disengages, so streaming output never yanks the viewport.
	const updateFollowOutput = useCallback(() => {
		const messages = messagesRef.current
		if (!messages) return
		followOutputRef.current = messages.scrollHeight - messages.scrollTop - messages.clientHeight <= stickToBottomThresholdPX
	}, [])
	// Keeps the per-session scroll memory fresh; rAF-throttled so busy scroll
	// gestures record at most one sample per frame.
	const scheduleScrollMemorySave = useCallback(() => {
		if (saveMemoryFrameRef.current) return
		saveMemoryFrameRef.current = requestAnimationFrame(() => {
			saveMemoryFrameRef.current = 0
			const messages = messagesRef.current
			const sessionID = sessionIDRef.current
			if (!messages || !sessionID) return
			const containerTop = messages.getBoundingClientRect().top
			const anchor = [...messages.querySelectorAll<HTMLElement>('.message[data-seq]')]
				.find((element) => element.getBoundingClientRect().bottom > containerTop) ?? null
			scrollMemoryRef.current.set(sessionID, {
				following: followOutputRef.current,
				seq: anchor ? Number(anchor.dataset.seq) : null,
				offsetPX: anchor ? anchor.getBoundingClientRect().top - containerTop : 0,
			})
		})
	}, [])
	const handleScroll = useCallback(() => {
		updateFollowOutput()
		scheduleScrollMemorySave()
	}, [scheduleScrollMemorySave, updateFollowOutput])
	// Any deliberate scroll gesture cancels an in-flight position restore.
	const cancelSettle = useCallback(() => {
		if (settleFrameRef.current) cancelAnimationFrame(settleFrameRef.current)
		settleFrameRef.current = 0
		settleActiveRef.current = false
	}, [])
	// On session switch, restore where the user left off: snapping to the
	// bottom when they were following output (or have never visited), otherwise
	// re-anchoring on the remembered message. content-visibility estimates make
	// scrollHeight a moving target right after the swap, so the restore pins
	// the target whenever the layout moves; without it the viewport lands
	// wherever the estimates put it — e.g. the top of the last page — and even
	// triggers a spurious older-page load. Position changes the restore did
	// not cause (user scrolls) are respected after one reclaim frame, and any
	// scroll gesture cancels the loop outright.
	useLayoutEffect(() => {
		const messages = messagesRef.current
		const sessionID = props.sessionID
		if (!messages || !sessionID) return
		const memory = scrollMemoryRef.current.get(sessionID)
		const anchorTarget = memory && !memory.following && memory.seq != null
			? { seq: memory.seq, offsetPX: memory.offsetPX }
			: null
		followOutputRef.current = !anchorTarget
		settleActiveRef.current = true
		let lastHeight = -1
		let stableFrames = 0
		let misses = 0
		let foundAnchor = false
		const startedAt = performance.now()
		const finish = (container: HTMLElement) => {
			settleActiveRef.current = false
			settleFrameRef.current = 0
			if (anchorTarget && !foundAnchor) {
				// The remembered message is gone (history rewritten): fall back
				// to the bottom rather than stranding the user mid-list.
				followOutputRef.current = true
				container.scrollTop = container.scrollHeight
			} else {
				updateFollowOutput()
			}
		}
		const step = () => {
			const container = messagesRef.current
			if (!container || !settleActiveRef.current) return
			if (performance.now() - startedAt > 2500) {
				finish(container)
				return
			}
			let distance: number
			if (anchorTarget) {
				const anchor = container.querySelector<HTMLElement>(`.message[data-seq="${anchorTarget.seq}"]`)
				if (anchor) {
					foundAnchor = true
					distance = anchor.getBoundingClientRect().top - container.getBoundingClientRect().top - anchorTarget.offsetPX
				} else {
					distance = Number.POSITIVE_INFINITY
				}
			} else {
				distance = container.scrollHeight - container.scrollTop - container.clientHeight
			}
			const height = container.scrollHeight
			const heightChanged = height !== lastHeight
			lastHeight = height
			if (Math.abs(distance) <= 2) {
				misses = 0
				stableFrames += 1
				if (stableFrames >= 3) {
					finish(container)
					return
				}
			} else if (!heightChanged) {
				// The position moved without a layout change — an external
				// scroll. Reclaim once in case it was estimate drift, then
				// respect it and stop.
				misses += 1
				if (misses >= 2) {
					finish(container)
					return
				}
				stableFrames = 0
				if (anchorTarget) container.scrollTop += distance
				else container.scrollTop = container.scrollHeight
			} else {
				// Layout still converging: keep the restore target pinned.
				misses = 0
				stableFrames = 0
				if (anchorTarget) container.scrollTop += distance
				else container.scrollTop = container.scrollHeight
			}
			settleFrameRef.current = requestAnimationFrame(step)
		}
		step()
		return cancelSettle
	}, [props.sessionID, cancelSettle, updateFollowOutput])
	// Follow the output stream, but only while the user stays at the bottom.
	// Runs before paint so streaming growth never shows an un-scrolled frame.
	// While a switch restore is settling, the restore loop owns scrolling.
	useLayoutEffect(() => {
		if (settleActiveRef.current) return
		if (followOutputRef.current) scrollToBottom()
	}, [props.activeRun, props.page?.newest_seq, props.turnError, scrollToBottom])
	// Resending starts a run from the trailing user message already in history.
	const handleResend = useCallback(async (item: SessionItem) => {
		if (resendPending) return
		setResendPending(true)
		try {
			await props.onResend(item)
		} finally {
			setResendPending(false)
		}
	}, [props.onResend, resendPending])
	const copySessionID = async () => {
		if (!safeDetail) return
		try {
			await copyText(`${safeDetail.project_id} ${safeDetail.id}`)
		} catch { /* ignore copy errors */ }
	}
	// Sending is explicit intent to be at the bottom: re-engage following even
	// when the user had scrolled up to read history.
	const handleSend = useCallback(async (content: string, images: PastedImageAttachment[]): Promise<boolean> => {
		const sent = await props.onSend(content, images)
		if (sent) {
			cancelSettle()
			followOutputRef.current = true
			scrollToBottom()
		}
		return sent
	}, [props.onSend, scrollToBottom, cancelSettle])
	const loadOlder = useCallback(async () => {
		const messages = messagesRef.current
		if (!messages || loadingOlderRef.current || !props.page?.has_more_before) return
		loadingOlderRef.current = true
		setLoadingOlder(true)
		const containerTop = messages.getBoundingClientRect().top
		const anchorElement = [...messages.querySelectorAll<HTMLElement>('.message')]
			.find((element) => element.getBoundingClientRect().bottom > containerTop) ?? null
		prependAnchorRef.current = {
			sessionID: props.sessionID ?? '',
			element: anchorElement,
			top: anchorElement?.getBoundingClientRect().top ?? containerTop,
			oldestSeq: props.page.oldest_seq,
		}
		try {
			if (!await props.onLoadOlder()) prependAnchorRef.current = null
		} finally {
			loadingOlderRef.current = false
			setLoadingOlder(false)
		}
	}, [props.sessionID, props.onLoadOlder, props.page])
	// Restore the viewport anchor after older items are prepended. Only a true
	// prepend (oldest_seq moved backwards) consumes the anchor: refresh merges
	// append at the tail and leave the geometry untouched. The compensation is
	// geometric, so it composes with the browser's own scroll anchoring: when
	// the browser already compensated, the measured delta is zero.
	useLayoutEffect(() => {
		const anchor = prependAnchorRef.current
		const messages = messagesRef.current
		if (!anchor || !messages) return
		if (anchor.sessionID !== (props.sessionID ?? '')) {
			prependAnchorRef.current = null
			return
		}
		const page = props.page
		if (!page || page.oldest_seq === anchor.oldestSeq) return
		prependAnchorRef.current = null
		if (anchor.element?.isConnected) messages.scrollTop += anchor.element.getBoundingClientRect().top - anchor.top
	}, [props.sessionID, props.page])
	// Older history pages load only on an explicit click on the "Load earlier
	// messages" button. Scrolling to the top deliberately does not auto-load:
	// casual upward scrolls must not silently grow the history window.
	useEffect(() => () => {
		if (saveMemoryFrameRef.current) cancelAnimationFrame(saveMemoryFrameRef.current)
	}, [])
	const visibleItems = useMemo(
		() => visibleSessionItems(props.page?.items ?? [], props.activeRun),
		[props.activeRun, props.page?.items],
	)
	const conversationEntries = useMemo(() => buildConversationEntries(visibleItems, props.sessionID ?? '', props.recentStepsByTurn), [props.sessionID, props.recentStepsByTurn, visibleItems])
	// A session that is idle with a user message at the tail almost always
  // means the turn died mid-flight: offer to resend it in place.
	const trailingUserItem = useMemo(() => {
		if (props.activeRun || safeDetail?.status === 'running') return null
		const last = visibleItems[visibleItems.length - 1]
		return last?.message?.role === 'user' ? last : null
	}, [props.activeRun, safeDetail?.status, visibleItems])

  const headerStatus = props.compacting || props.activeRun?.status === 'running' || safeDetail?.status === 'running'
    ? 'running'
    : safeDetail?.status === 'interrupted' ? 'interrupted' : 'idle'

  return (
    <div className="conversation">
      <header className="conversation-header">
        <div className="conversation-left-group">
          <div className="conversation-heading">
            <div className="conversation-title-row">
              <span className={`status-dot ${headerStatus !== 'idle' ? headerStatus : ''}`} title={`Session ${headerStatus}`} aria-label={`Session status: ${headerStatus}`} />
              <h1>{safeDetail ? sessionName(safeDetail) : 'Loading…'}</h1>
              {safeDetail && <button className="message-tool-button copy-id-button" onClick={() => void copySessionID()} title="Copy project and session ID" aria-label="Copy project and session ID"><CopyIcon /></button>}
            </div>
            {safeDetail && <p>{safeDetail.provider} / {safeDetail.model_id}{safeDetail.reasoning_level && ` · ${safeDetail.reasoning_level}`}</p>}
          </div>
          {safeDetail && (
            <ContextUsage context={safeDetail.context} activeInputTokens={props.activeRun?.inputTokens} activeCachedTokens={props.activeRun?.cachedTokens} activeCacheWriteTokens={props.activeRun?.cacheWriteTokens} compactedContextTokens={props.activeRun?.compaction?.status === 'completed' ? props.activeRun.compaction.activeContextTokens : undefined} />
          )}
          {safeDetail && <CostUsage pricing={safeDetail.pricing} context={safeDetail.context} activeUsageEvents={props.activeRun?.usageEvents} activeStatus={props.activeRun?.status} />}
        </div>
        <div className="header-actions">
		  {safeDetail && (
			<button
				className={`secondary-button full-access-toggle${safeDetail.full_access ? ' on' : ''}`}
				onClick={props.onToggleFullAccess}
				title={`Full access ${safeDetail.full_access ? 'ON' : 'OFF'}: file tools ${safeDetail.full_access ? 'may read and write outside the workspace' : 'are confined to the workspace'}. Toggling applies from the next turn.`}
				aria-pressed={safeDetail.full_access}
			>
				Full access{safeDetail.full_access ? ' · ON' : ''}
			</button>
		  )}
		  <button className="secondary-button" disabled={!safeDetail || safeDetail.status === 'running' || props.compacting || props.activeRun?.status === 'running'} onClick={props.onCompact}>{props.compacting ? 'Compacting…' : 'Compact context'}</button>
        </div>
      </header>
      <section ref={messagesRef} className="messages" aria-live="polite" onScroll={handleScroll} onWheel={cancelSettle} onTouchMove={cancelSettle} onKeyDown={cancelSettle} onPointerDown={cancelSettle}>
        {props.page?.has_more_before && <button className="load-older" disabled={loadingOlder} onClick={() => void loadOlder()}>{loadingOlder ? 'Loading earlier messages…' : 'Load earlier messages'}</button>}
        {!props.page && <MessageSkeleton />}
				{conversationEntries.map((entry) => entry.kind === 'message'
					? <Message key={entry.item.id} item={entry.item} sessionID={props.sessionID ?? ''} onResend={entry.item === trailingUserItem ? handleResend : undefined} resendPending={resendPending} />
					: <HistoricalProcess key={entry.id} entry={entry} sessionNames={props.sessionNames} workspaceRoot={safeDetail?.created_cwd} />)}
        {props.activeRun && <ActiveRunView run={props.activeRun} onCancelTool={props.onCancelTool} sessionNames={props.sessionNames} workspaceRoot={safeDetail?.created_cwd} />}
        {props.compacting && <CompactionStatus trigger="manual" status="running" />}
		{props.turnError && (
			<div className="turn-error" role="alert">
				<WarningIcon />
				<div className="turn-error-copy">
					<strong>Turn failed</strong>
					<p>{props.turnError.message}</p>
				</div>
				<button className="message-tool-button" onClick={props.onRetry} title="Retry last turn"><RetryIcon />Retry</button>
				<button className="turn-error-dismiss" onClick={props.onDismissTurnError} aria-label="Dismiss error" title="Dismiss">×</button>
			</div>
		)}
	{props.activeRun?.status === 'error_pending_refresh' && (
		<div className="turn-error" role="alert">
			<WarningIcon />
			<div className="turn-error-copy">
				<strong>Refresh needed</strong>
				<p>The session state may be outdated. Refresh to see the latest.</p>
			</div>
			<button className="message-tool-button" onClick={props.onRetryRefresh} title="Refresh session">Refresh</button>
		</div>
	)}
		{!props.turnError && safeDetail?.status === 'interrupted' && !props.activeRun && (
			<div className="turn-error" role="alert">
				<WarningIcon />
				<div className="turn-error-copy">
					<strong>Session interrupted</strong>
					<p>The last turn did not complete. You can retry it.</p>
				</div>
				<button className="turn-error-dismiss" onClick={props.onDismissTurnError} aria-label="Dismiss" title="Dismiss">×</button>
			</div>
		)}
		{props.page && visibleItems.length === 0 && !props.activeRun && (
          <div className="conversation-empty"><SparkIcon /><h3>Start a new task</h3><p>Describe a goal, a problem, or the code you want to change.</p></div>
        )}
		{safeDetail?.status === 'interrupted' && !props.activeRun && !props.turnError && (
          <div className="conversation-retry">
            <button className="message-tool-button" onClick={props.onRetry} title="Retry last turn"><RetryIcon />Retry last turn</button>
          </div>
        )}
      </section>
	  <QueuedPromptList
		prompts={props.activeRun?.queuedPrompts ?? []}
		onRemove={props.onRemoveQueuedPrompt}
		onSteer={props.onSteerQueuedPrompt}
		onMove={props.onMoveQueuedPrompt}
	  />
	  <Composer
		draft={props.draft}
		onContentChange={props.onDraftChange}
		onPastedTextAdd={props.onPastedTextAdd}
		onPastedTextRemove={props.onPastedTextRemove}
		onPastedImageAdd={props.onPastedImageAdd}
		onPastedImageRemove={props.onPastedImageRemove}
		onDraftClear={props.onDraftClear}
		running={props.activeRun?.status === 'running'}
		blocked={!props.activeRun && (props.compacting || safeDetail?.status === 'running')}
		onSend={handleSend}
		onCancel={props.onCancel}
	  />
    </div>
  )
}, (previous, next) =>
  previous.sessionID === next.sessionID &&
  previous.detail === next.detail &&
  previous.page === next.page &&
  previous.activeRun === next.activeRun &&
  previous.compacting === next.compacting &&
  previous.draft === next.draft &&
  previous.turnError === next.turnError &&
  previous.recentStepsByTurn === next.recentStepsByTurn &&
  previous.sessionNames === next.sessionNames &&
  previous.onCancelTool === next.onCancelTool &&
  previous.onRetry === next.onRetry &&
  previous.onRetryRefresh === next.onRetryRefresh)

function ContextUsage(props: { context: Session['context']; activeInputTokens?: number; activeCachedTokens?: number; activeCacheWriteTokens?: number; compactedContextTokens?: number }) {
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
	const compactedContextTokens = Number(props.compactedContextTokens ?? 0)
	const usedTokens = compactedContextTokens > 0 ? compactedContextTokens : livePromptTokens > 0 ? livePromptTokens : recordedPromptTokens > 0 ? recordedPromptTokens : requestEstimate
	const usageEstimated = compactedContextTokens > 0 || (livePromptTokens <= 0 && (recordedPromptTokens <= 0 || context?.last_usage_source !== 'provider'))
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

function CostUsage(props: { pricing?: Session['pricing']; context: Session['context']; activeUsageEvents?: ActiveRun['usageEvents']; activeStatus?: ActiveRun['status'] }) {
	const storedUsage = contextUsageBreakdown(props.context)
	const activeUsage = usageBreakdownFromEvents(props.activeUsageEvents, props.pricing)
	// While a run is live its usage has not been persisted yet. Once the run
	// enters reconciliation, the refreshed session may already include it, so
	// do not add the in-memory usage a second time.
	const totalUsage = props.activeStatus === 'running' ? addUsageBreakdown(storedUsage, activeUsage) : storedUsage
	const totalCost = usageCostBreakdown(totalUsage, props.pricing) ?? (props.pricing ? 0 : undefined)
	const runCost = usageCostBreakdown(activeUsage, props.pricing)
	const totalRequests = contextRequestCount(props.context) + (props.activeStatus === 'running' ? usageEventCount(props.activeUsageEvents) : 0)
	const usage = totalUsage?.total
	if (!usage && totalRequests <= 0 && !props.pricing) return null
	const currency = props.pricing?.currency ?? 'USD'
	const details = [
		`Cache hit ${usage?.cachedTokens.toLocaleString() ?? 0}`,
		`Cache miss ${Math.max(0, usage?.inputTokens ?? 0).toLocaleString()}`,
		`Cache write ${Math.max(0, usage?.cacheWriteTokens ?? 0).toLocaleString()}`,
		`Output ${usage?.outputTokens.toLocaleString() ?? 0}`,
	].join('; ')
	return (
		<div className="cost-usage" title={`${details}\nAPI requests ${totalRequests.toLocaleString()}\nPrices are per 1M tokens`}>
			<div className="session-stat"><span>Cost</span><strong>{totalCost !== undefined ? formatCost(totalCost, currency) : '—'}</strong><small>{runCost !== undefined ? `Run ${formatCost(runCost, currency)}` : 'Session'}</small></div>
			<div className="session-stat"><span>API requests</span><strong>{totalRequests.toLocaleString()}</strong></div>
			<div className="session-stat"><span>Tokens</span><strong>{formatTokenCount(usage?.totalTokens ?? 0)}</strong></div>
		</div>
	)
}

const Message = memo(function Message({ item, sessionID, onResend, resendPending }: { item: SessionItem; sessionID: string; onResend?: (item: SessionItem) => void; resendPending?: boolean }) {
  // A compaction record is the durable divider marking where earlier
  // messages were summarized; it renders as one muted line, not a bubble.
  if (item.kind === 'compaction') return <CompactionRecord item={item} />
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
    <article className={`message ${role === 'user' ? 'user' : 'assistant'}`} data-seq={item.seq}>
      <div className="message-content">
        {role === 'user' && text && <div className="message-text">{text}</div>}
        {role === 'user' && images.length > 0 && <StoredImageAttachments sessionID={sessionID} images={images} />}
		{role === 'user' && onResend && (
			<div className="message-tools" aria-label="Message actions">
				<button className="message-tool-button" disabled={resendPending} onClick={() => onResend(item)} title="Send this message again">
					<RetryIcon />{resendPending ? 'Sending…' : 'Resend'}
				</button>
			</div>
		)}
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
})

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

function QueuedPromptList({ prompts, onRemove, onSteer, onMove }: {
  prompts: QueuedPrompt[]
  onRemove: (promptID: string) => void
  onSteer: (promptID: string, steer: boolean) => void
  onMove: (promptID: string, direction: 'up' | 'down') => void
}) {
  if (prompts.length === 0) return null
  // The server keeps steer prompts ahead of plain queued prompts, so the list
  // is two contiguous groups. Reorder buttons clamp at the group boundary: a
  // plain queued message can never move above a steer.
  const firstOfGroup = (index: number) =>
    prompts.findIndex((entry) => Boolean(entry.steer) === Boolean(prompts[index].steer)) === index
  const lastOfGroup = (index: number) =>
    index === prompts.length - 1 || Boolean(prompts[index + 1].steer) !== Boolean(prompts[index].steer)
  return (
    <div className="queued-prompt-list" aria-label="Queued messages">
      {prompts.map((prompt, index) => (
        <div className={`queued-prompt-row${prompt.steer ? ' steer' : ''}`} key={prompt.id}>
          <span className={`queued-prompt-badge${prompt.steer ? ' steer' : ''}`}>{prompt.steer ? 'Steer' : 'Queued'}</span>
          <span className="queued-prompt-text" title={prompt.content}>{prompt.content}</span>
          <button
            type="button"
            className="queued-prompt-move"
            disabled={firstOfGroup(index)}
            onClick={() => onMove(prompt.id, 'up')}
            aria-label="Move queued message up"
            title="Move up"
          >↑</button>
          <button
            type="button"
            className="queued-prompt-move"
            disabled={lastOfGroup(index)}
            onClick={() => onMove(prompt.id, 'down')}
            aria-label="Move queued message down"
            title="Move down"
          >↓</button>
          <button
            type="button"
            className={`queued-prompt-steer${prompt.steer ? ' active' : ''}`}
            onClick={() => onSteer(prompt.id, !prompt.steer)}
            aria-label={prompt.steer ? 'Demote to queued message' : 'Promote to steer message'}
            title={prompt.steer ? 'Demote to regular queue' : 'Steer: deliver first, ahead of queued messages'}
          >{prompt.steer ? 'Queued' : 'Steer'}</button>
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

function ActiveRunView({ run, onCancelTool, sessionNames, workspaceRoot }: { run: ActiveRun; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
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
      {run.compaction && <CompactionStatus trigger={run.compaction.trigger} status={run.compaction.status} activeContextTokens={run.compaction.activeContextTokens} contextWindow={run.compaction.contextWindow} />}
      <ActiveRunBody run={run} onCancelTool={onCancelTool} sessionNames={sessionNames} workspaceRoot={workspaceRoot} />
      {run.providerRetry && <ProviderRetryStatus key={run.providerRetry.attempt} retry={run.providerRetry} />}
    </>
  )
}

// ProviderRetryStatus counts down the backoff delay, then switches to a
// "reconnecting" label while the retry attempt itself is in flight. The
// component is keyed by attempt, so a new provider.retrying event restarts
// the countdown from the new delay.
function ProviderRetryStatus({ retry }: { retry: NonNullable<ActiveRun['providerRetry']> }) {
  const [remainingMS, setRemainingMS] = useState(() => Math.max(0, retry.delayMS))
  useEffect(() => {
    if (retry.delayMS <= 0) return
    const startedAt = Date.now()
    const timer = window.setInterval(() => {
      setRemainingMS(Math.max(0, retry.delayMS - (Date.now() - startedAt)))
    }, 250)
    return () => window.clearInterval(timer)
  }, [retry.delayMS])
  const waiting = remainingMS > 0
  return (
    <div className="provider-retry-status" role="status">
      <span />
      {waiting
        ? <>Server temporarily failed. Retrying request {retry.attempt} of {retry.maxAttempts} in {Math.ceil(remainingMS / 1000).toLocaleString()}s…</>
        : <>Server temporarily failed. Reconnecting (attempt {retry.attempt} of {retry.maxAttempts})…</>}
    </div>
  )
}

function CompactionStatus({ trigger, status, activeContextTokens, contextWindow }: {
  trigger: 'auto' | 'manual'
  status: 'running' | 'completed'
  activeContextTokens?: number
  contextWindow?: number
}) {
  const detail = status === 'running'
    ? `${trigger === 'auto' ? 'Automatic' : 'Manual'} compaction is running…`
    : `Context compacted${activeContextTokens ? ` to approximately ${formatTokenCount(activeContextTokens)}${contextWindow ? ` / ${formatTokenCount(contextWindow)}` : ''}` : ''}. Continuing the turn…`
  return <div className={`compaction-status ${status}`} role="status"><span />{detail}</div>
}

// ActiveRunBody renders the in-flight turn. Mid-turn appended user messages
// interrupt the assistant process: the steps gathered before an appended user
// message form one assistant segment, the appended message renders as its own
// user bubble, and the remaining steps continue in a following assistant
// segment. The streaming text and token note live in the trailing segment.
function ActiveRunBody({ run, onCancelTool, sessionNames, workspaceRoot }: { run: ActiveRun; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
  const segments = useMemo(() => buildActiveRunSegments(run.steps), [run.steps])
  const displaySegments = segments.length > 0 ? segments : [{ kind: 'steps' as const, steps: [] }]

  // Streaming text soft-seals the live tail: once the model starts writing,
  // the trailing tool group collapses instead of staying open until the
  // output step flush at the next tool call (or until the run settles).
  const textStreaming = Boolean(run.assistantText)
  // The cursor tracks the turn, not the text: it stays visible for the whole
  // run — while the model writes, while tools run between iterations, and
  // before the first usage update — and only stops once the run leaves the
  // running state (settled, failed, cancelled, or reconciling).
  const running = run.status === 'running'
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
      {displaySegments.map((segment, index) => {
        const isLast = index === displaySegments.length - 1
        if (segment.kind === 'user') return <article className="message user transient" key={segment.step.id}><div className="message-content"><div className="message-text">{segment.step.text}</div></div></article>
        return <article className="message assistant transient" key={`steps-${index}`}><div className="message-content">
          {isLast && <div className="message-meta"><span className="streaming-label"><i />Generating</span></div>}
          <ProcessTimeline steps={segment.steps} live={isLast && run.status === 'running' && !textStreaming} onCancelTool={onCancelTool} sessionNames={sessionNames} workspaceRoot={workspaceRoot} />
          {isLast && run.assistantText && <MarkdownMessage text={run.assistantText} streaming cursor={running} />}
          {isLast && running && !run.assistantText && <div className="message-text assistant-stream"><span className="cursor" /></div>}
          {isLast && tokenNote}
        </div></article>
      })}
    </>
  )
}

function buildActiveRunSegments(steps: RunStep[]) {
  const segments: Array<{ kind: 'steps'; steps: RunStep[] } | { kind: 'user'; step: Extract<RunStep, { kind: 'user' }> }> = []
  let current: RunStep[] = []
  for (const step of steps) {
    if (step.kind === 'user') {
      if (current.length > 0) segments.push({ kind: 'steps', steps: current })
      current = []
      segments.push({ kind: 'user', step })
    } else {
      current.push(step)
    }
  }
  if (current.length > 0) segments.push({ kind: 'steps', steps: current })
  return segments
}

// CompactionRecord renders the durable compaction marker as a subtle
// one-line divider in the message flow.
function CompactionRecord({ item }: { item: SessionItem }) {
  const text = itemText(item) || 'Context compacted'
  return (
    <div className="compaction-record" role="note" title="Earlier messages were summarized to fit the context window">
      <span className="compaction-record-rule" aria-hidden="true" />
      <span className="compaction-record-text">{text}</span>
      <span className="compaction-record-rule" aria-hidden="true" />
    </div>
  )
}

const markdownComponents: Components = {
  a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

function MarkdownMessage({ text, streaming = false, cursor = false }: { text: string; streaming?: boolean; cursor?: boolean }) {
  return (
    <div className={`message-text markdown-body ${streaming ? 'assistant-stream' : ''}`}>
      <Markdown remarkPlugins={[remarkGfm]} components={markdownComponents} skipHtml>{text}</Markdown>
      {cursor && <span className="cursor" />}
    </div>
  )
}

type ConversationEntry =
	| { kind: 'message'; item: SessionItem }
	| { kind: 'process'; id: string; createdAt: string; lastSeq: number; steps: RunStep[] }

function buildConversationEntries(items: SessionItem[], sessionID: string, recentStepsByTurn: Record<string, RunStep[]>): ConversationEntry[] {
	const entries: ConversationEntry[] = []
	let steps: RunStep[] = []
	let processCreatedAt = ''
	let processTurnID = ''
	let processLastSeq = 0
	let agentIteration = 0
	const emittedRecentTurns = new Set<string>()

	const flushProcess = (turnID = processTurnID) => {
		const recentKey = turnID ? processKey(sessionID, turnID) : ''
		const recentSteps = recentKey && !emittedRecentTurns.has(recentKey) ? recentStepsByTurn[recentKey] : undefined
		const displayedSteps = recentSteps?.length ? recentSteps : steps
		if (displayedSteps.length > 0) {
			// The id must stay unique per group: a mid-turn user message or a
			// compaction record splits one turn into several process groups.
			// The id must stay unique per group: a mid-turn user message or a
			// compaction record splits one turn into several process groups.
			entries.push({ kind: 'process', id: `process-${sessionID}-${turnID || displayedSteps[0].id}-${processLastSeq}`, createdAt: processCreatedAt, lastSeq: processLastSeq, steps: displayedSteps })
		}
		if (recentKey && recentSteps?.length) emittedRecentTurns.add(recentKey)
		steps = []
		processCreatedAt = ''
		processTurnID = ''
		processLastSeq = 0
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
			processLastSeq = item.seq
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
			processLastSeq = item.seq
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

function HistoricalProcess({ entry, sessionNames, workspaceRoot }: { entry: Extract<ConversationEntry, { kind: 'process' }>; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
	return (
		<article className="message assistant process-message" data-seq={entry.lastSeq}>
			<div className="message-content">
				<ProcessTimeline steps={entry.steps} sessionNames={sessionNames} workspaceRoot={workspaceRoot} />
			</div>
		</article>
	)
}
