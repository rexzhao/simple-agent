import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../api'
import type { ActiveRun, ItemsPage, QueuedPrompt, Session, SessionImageAttachment, SessionItem } from '../types'
import { addUsageBreakdown, contextRequestCount, contextUsageBreakdown, usageBreakdownFromEvents, usageCostBreakdown, usageEventCount } from '../lib/cost'
import { buildConversationRows, conversationRowKey } from '../lib/conversationRows'
import type { ConversationRow } from '../lib/conversationRows'
import { blobAsDataURL, copyText, formatCost, formatTokenCount } from '../lib/format'
import { itemText, sessionName } from '../lib/session'
import { Composer } from './Composer'
import type { ComposerDraft, PastedImageAttachment, PastedTextAttachment } from './Composer'
import { MessageSkeleton } from './misc'
import { ProcessTimeline } from './ProcessTimeline'
import { BugIcon, CopyIcon, RetryIcon, SparkIcon, WarningIcon } from './icons'
import { VirtualConversationList } from './VirtualConversationList'
import type { VirtuosoHandle } from 'react-virtuoso'

export const Conversation = memo(function Conversation(props: {
  sessionID: string
  detail: Session | null
  page: ItemsPage | null
  activeRun: ActiveRun | null
  admissionPending?: boolean
  compacting: boolean
	draft: ComposerDraft
	onDraftChange: (content: string) => void
	onPastedTextAdd: (pastedText: PastedTextAttachment) => void
	onPastedTextRemove: (pastedTextID: number) => void
	onPastedImageAdd: (pastedImage: PastedImageAttachment) => void
	onPastedImageRemove: (pastedImageID: number) => void
	onDraftClear: () => void
  sessionNames: Record<string, string>
  turnError: { turnID: string; message: string } | null
  onDismissTurnError: () => void
  onLoadOlder: () => Promise<boolean>
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
  onCancelTool: (toolCallID: string) => void
  onContinue: () => void
  onRetryRefresh: () => void
  onDebug: () => void
  onToggleFullAccess: () => void
  onRemoveQueuedPrompt: (promptID: string) => void
  onSteerQueuedPrompt: (promptID: string, steer: boolean) => void
  onMoveQueuedPrompt: (promptID: string, direction: 'up' | 'down') => void
  onCompact: () => void
}) {
	const virtuosoRef = useRef<VirtuosoHandle | null>(null)
	const followMemoryRef = useRef(new Map<string, boolean>())
	type FollowIntent = 'initial' | 'following' | 'detached' | 'pending-send'
	const followIntentRef = useRef<FollowIntent>('initial')
	const userScrollRef = useRef(false)
	const loadingOlderRef = useRef(false)
	const [loadingOlder, setLoadingOlder] = useState(false)
	const safeDetail = props.detail && props.detail.id === props.sessionID ? props.detail : null

	// Virtuoso's Scroller is the sole .messages element. Use its imperative API
	// for explicit bottom requests instead of competing with its scroll model.
	const scrollToBottom = useCallback(() => {
		virtuosoRef.current?.scrollToIndex({ index: 'LAST', align: 'end', behavior: 'auto' })
	}, [])
	const handleVirtuosoRef = useCallback((sessionID: string, handle: VirtuosoHandle | null) => {
		if (sessionID !== props.sessionID) return
		virtuosoRef.current = handle
	}, [props.sessionID])
	// Virtuoso reports a new total height after variable rows are measured. A
	// bottom request here is event-driven: it is allowed only while the state
	// machine owns the viewport, and is never used to reclaim a detached or
	// restoring session.
	const handleTotalListHeightChanged = useCallback((sessionID: string) => {
		if (sessionID !== props.sessionID) return
		const intent = followIntentRef.current
		if (intent === 'initial' || intent === 'following' || intent === 'pending-send') {
			virtuosoRef.current?.scrollToIndex({ index: 'LAST', align: 'end', behavior: 'auto' })
		}
	}, [props.sessionID])
	// Keep intent separate from Virtuoso's measurement signal. In particular,
	// an atBottom=false notification during an explicit send must not cancel
	// following before the new row has committed and been measured.
	const handleAtBottomStateChange = useCallback((sessionID: string, atBottom: boolean, fromScroll = false) => {
		if (sessionID !== props.sessionID) return
		const intent = followIntentRef.current
		if (intent === 'pending-send') {
			if (atBottom) {
				followIntentRef.current = 'following'
				userScrollRef.current = false
				followMemoryRef.current.set(sessionID, true)
			}
			return
		}
		if (intent === 'initial') {
			// Initial measurement can report false before LAST is placed. It is
			// not user intent and must not turn off initial following.
			if (atBottom) {
				followIntentRef.current = 'following'
				userScrollRef.current = false
				followMemoryRef.current.set(sessionID, true)
			}
			return
		}
		if (intent === 'detached') {
			if (fromScroll && atBottom) {
				followIntentRef.current = 'following'
				userScrollRef.current = false
				followMemoryRef.current.set(sessionID, true)
			}
			return
		}
		if (atBottom) {
			followIntentRef.current = 'following'
			userScrollRef.current = false
			followMemoryRef.current.set(sessionID, true)
		} else if (fromScroll && userScrollRef.current) {
			followIntentRef.current = 'detached'
			followMemoryRef.current.set(sessionID, false)
		}
		// A false notification while following can be caused by a row's measured
		// height changing. Explicit user input is what detaches the viewport;
		// totalListHeightChanged keeps this state at the bottom without a timer.
	}, [props.sessionID])
	const markScrollInteraction = useCallback((sessionID: string) => {
		if (sessionID !== props.sessionID) return
		followIntentRef.current = 'detached'
		userScrollRef.current = true
		followMemoryRef.current.set(sessionID, false)
	}, [props.sessionID])
	const followOutput = useCallback((sessionID: string, isAtBottom: boolean) => {
		if (sessionID !== props.sessionID) return false
		const intent = followIntentRef.current
		if (intent === 'initial' || intent === 'pending-send' || intent === 'following') return 'auto'
		// A detached session can still legitimately receive a callback while
		// already at the bottom; honor Virtuoso's authoritative signal there.
		return isAtBottom ? 'auto' : false
	}, [props.sessionID])
	// Restore the session's follow intent before Virtuoso evaluates its first
	// layout. The scroll position itself comes from restoreStateFrom.
	useLayoutEffect(() => {
		userScrollRef.current = false
		followIntentRef.current = followMemoryRef.current.has(props.sessionID)
			? (followMemoryRef.current.get(props.sessionID) ? 'following' : 'detached')
			: 'initial'
	}, [props.sessionID])
	const copySessionID = async () => {
		if (!safeDetail) return
		try {
			await copyText(`${safeDetail.project_id} ${safeDetail.id}`)
		} catch { /* ignore copy errors */ }
	}
	// Sending is explicit intent to be at the bottom: re-engage following even
	// when the user had scrolled up to read history.
	const handleSend = useCallback(async (content: string, images: PastedImageAttachment[]): Promise<boolean> => {
		// Set this before awaiting the server response: startNewRun publishes the
		// active row before the promise resolves, and an early atBottom(false)
		// notification must not turn off explicit send-following in that commit.
		followIntentRef.current = 'pending-send'
		const sent = await props.onSend(content, images)
		if (sent) {
			scrollToBottom()
		} else {
			// There was no data commit to follow. Keep the explicit intent, but
			// let the next atBottom signal settle it normally.
			followIntentRef.current = 'following'
		}
		return sent
	}, [props.onSend, scrollToBottom])
	// Older history pages load only on an explicit click on the "Load earlier
	// messages" button. Scrolling to the top deliberately does not auto-load:
	// casual upward scrolls must not silently grow the history window.
	const conversationRows = useMemo(() => {
		const rows = buildConversationRows({
			sessionID: props.sessionID ?? '',
			items: props.page?.items ?? [],
			activeRun: props.activeRun,
			compacting: props.compacting,
			turnError: props.turnError,
			sessionStatus: safeDetail?.status,
		})
		// This is a real, stable final row rather than Virtuoso Footer padding.
		// Aligning LAST with it gives the UI one unambiguous bottom geometry.
		// An actually empty list stays empty so Virtuoso's EmptyPlaceholder can
		// represent the empty state instead of an invisible helper item.
		if (rows.length > 0) rows.push({ kind: 'bottom-spacer', key: conversationRowKey(props.sessionID, 'bottom-spacer') })
		return rows
	}, [props.activeRun, props.compacting, props.page?.items, props.sessionID, props.turnError, safeDetail?.status])
	const loadOlder = useCallback(async () => {
		if (loadingOlderRef.current || !props.page?.has_more_before) return
		loadingOlderRef.current = true
		setLoadingOlder(true)
		try {
			await props.onLoadOlder()
		} finally {
			loadingOlderRef.current = false
			setLoadingOlder(false)
		}
	}, [props.onLoadOlder, props.page])
  const headerStatus = props.compacting || props.activeRun?.status === 'running' || safeDetail?.status === 'running'
    ? 'running'
    : safeDetail?.status === 'interrupted' || safeDetail?.status === 'failed' ? 'interrupted' : 'idle'

	const renderRow = useCallback((row: ConversationRow) => renderConversationRow(row, {
		sessionID: props.sessionID ?? '',
		sessionNames: props.sessionNames,
		workspaceRoot: safeDetail?.created_cwd,
		canContinue: Boolean(safeDetail && (safeDetail.status === 'interrupted' || safeDetail.status === 'failed') && !safeDetail.running_run_id && !safeDetail.running_turn_id && safeDetail.interrupted_run_id && safeDetail.interrupted_turn_id && !props.activeRun),
		onCancelTool: props.onCancelTool,
		onContinue: props.onContinue,
		onRetryRefresh: props.onRetryRefresh,
		onDismissTurnError: props.onDismissTurnError,
	}), [props.activeRun, props.onCancelTool, props.onContinue, props.onDismissTurnError, props.onRetryRefresh, props.sessionID, props.sessionNames, safeDetail?.created_cwd, safeDetail?.interrupted_run_id, safeDetail?.interrupted_turn_id, safeDetail?.running_run_id, safeDetail?.running_turn_id, safeDetail?.status])

	const conversationEmpty = <div className="conversation-empty"><SparkIcon /><h3>Start a new task</h3><p>Describe a goal, a problem, or the code you want to change.</p></div>
	const listHeader = (
		<div className="messages-header-slot">
			{props.page?.has_more_before
				? <button className="load-older" disabled={loadingOlder} onClick={() => void loadOlder()}>{loadingOlder ? 'Loading earlier messages…' : 'Load earlier messages'}</button>
				: <span aria-hidden="true" />}
		</div>
	)
	const canContinue = Boolean(safeDetail && (safeDetail.status === 'interrupted' || safeDetail.status === 'failed') && !safeDetail.running_run_id && !safeDetail.running_turn_id && safeDetail.interrupted_run_id && safeDetail.interrupted_turn_id && !props.activeRun)
	const listFooter = canContinue && !props.turnError
		? <div className="conversation-retry"><button className="message-tool-button" onClick={props.onContinue} title="Continue interrupted run"><RetryIcon />Continue</button></div>
		: undefined

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
				className={`secondary-button debug-toggle${safeDetail.debug?.request_bodies ? ' on' : ''}`}
				onClick={props.onDebug}
				title="Open debug settings for this conversation"
				aria-pressed={safeDetail.debug?.request_bodies ?? false}
			>
				<BugIcon />Debug{safeDetail.debug?.request_bodies ? ' · ON' : ''}
			</button>
		  )}
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
		<VirtualConversationList
			sessionID={props.sessionID}
			rows={conversationRows}
			header={listHeader}
			footer={listFooter}
			emptyPlaceholder={!props.page ? <MessageSkeleton /> : conversationRows.length === 0 ? conversationEmpty : undefined}
			renderRow={renderRow}
			followOutput={followOutput}
			onInteraction={markScrollInteraction}
			onAtBottomStateChange={handleAtBottomStateChange}
			onTotalListHeightChanged={handleTotalListHeightChanged}
			onVirtuosoRef={handleVirtuosoRef}
		/>
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
		blocked={Boolean(props.admissionPending) || (!props.activeRun && (props.compacting || safeDetail?.status === 'running'))}
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
  previous.admissionPending === next.admissionPending &&
  previous.compacting === next.compacting &&
  previous.draft === next.draft &&
  previous.turnError === next.turnError &&
  previous.sessionNames === next.sessionNames &&
  previous.onCancelTool === next.onCancelTool &&
  previous.onContinue === next.onContinue &&
  previous.onRetryRefresh === next.onRetryRefresh &&
  previous.onDebug === next.onDebug)

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

type ConversationRowRenderProps = {
	sessionID: string
	sessionNames?: Record<string, string>
	workspaceRoot?: string
	onCancelTool?: (toolCallID: string) => void
	canContinue: boolean
	onContinue: () => void
	onRetryRefresh: () => void
	onDismissTurnError: () => void
}

function renderConversationRow(row: ConversationRow, props: ConversationRowRenderProps) {
	switch (row.kind) {
		case 'message':
			return <Message key={row.key} item={row.item} sessionID={props.sessionID} assistantTail={row.assistantTail} assistantStreaming={row.assistantStreaming} />
		case 'compaction':
			return <CompactionRecord key={row.key} item={row.item} />
		case 'process':
			return <HistoricalProcess key={row.key} entry={row} sessionNames={props.sessionNames} workspaceRoot={props.workspaceRoot} />
		case 'active-process':
			return <ActiveProcessRow key={row.key} row={row} onCancelTool={props.onCancelTool} sessionNames={props.sessionNames} workspaceRoot={props.workspaceRoot} />
		case 'active-cursor':
			return <ActiveCursorRow key={row.key} run={row.run} />
		case 'active-compaction':
			return <CompactionStatus key={row.key} trigger={row.compaction.trigger} status={row.compaction.status} activeContextTokens={row.compaction.activeContextTokens} contextWindow={row.compaction.contextWindow} />
		case 'provider-retry':
			return <ProviderRetryStatus key={row.key} retry={row.retry} />
		case 'manual-compaction':
			return <CompactionStatus key={row.key} trigger="manual" status="running" />
		case 'turn-error':
			return <TurnErrorRow key={row.key} message={row.message} canContinue={props.canContinue} onContinue={props.onContinue} onDismiss={props.onDismissTurnError} />
		case 'refresh-error':
			return <RefreshErrorRow key={row.key} onRetryRefresh={props.onRetryRefresh} />
		case 'interrupted':
			return <InterruptedRow key={row.key} onDismiss={props.onDismissTurnError} />
		case 'bottom-spacer':
			return <div className="messages-bottom-padding" aria-hidden="true" />
	}
}

const Message = memo(function Message({ item, sessionID, assistantTail = '', assistantStreaming = false }: { item: SessionItem; sessionID: string; assistantTail?: string; assistantStreaming?: boolean }) {
	const [copyStatus, setCopyStatus] = useState<'idle' | 'copied' | 'error'>('idle')
	const copyResetTimerRef = useRef<number | null>(null)
	useEffect(() => () => {
		if (copyResetTimerRef.current !== null) window.clearTimeout(copyResetTimerRef.current)
	}, [])
  // A compaction record is the durable divider marking where earlier
  // messages were summarized; it renders as one muted line, not a bubble.
  if (item.kind === 'compaction') return <CompactionRecord item={item} />
  const role = item.message?.role
  const committedText = item.message?.content?.inline || item.message?.content?.preview || ''
  // buildConversationRows attaches a tail only through the backend-provided
  // item id. Rendering it in this durable row keeps one assistant bubble.
  const text = role === 'assistant' ? committedText + assistantTail : committedText
  const images = item.message?.images ?? []
  const showCursor = role === 'assistant' && assistantStreaming
  if (!text && images.length === 0 && !showCursor) return null
	const copyMessage = async () => {
		const resetCopyStatus = (delay: number) => {
			if (copyResetTimerRef.current !== null) window.clearTimeout(copyResetTimerRef.current)
			copyResetTimerRef.current = window.setTimeout(() => {
				copyResetTimerRef.current = null
				setCopyStatus('idle')
			}, delay)
		}
		try {
			await copyText(text)
			setCopyStatus('copied')
			resetCopyStatus(1600)
		} catch {
			setCopyStatus('error')
			resetCopyStatus(2200)
		}
	}
  return (
    <article className={`message ${role === 'user' ? 'user' : 'assistant'}`} data-seq={item.seq}>
      <div className="message-content">
        {role === 'user' && text && <div className="message-text">{text}</div>}
        {role === 'user' && images.length > 0 && <StoredImageAttachments sessionID={sessionID} images={images} />}
		{role !== 'user' && (text || showCursor) && <MarkdownMessage text={text} streaming={showCursor} cursor={showCursor} />}
		{role === 'assistant' && (text || images.length > 0) && (
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

type ActiveProcessRowModel = Extract<ConversationRow, { kind: 'active-process' }>

function ActiveProcessRow({ row, onCancelTool, sessionNames, workspaceRoot }: { row: ActiveProcessRowModel; onCancelTool?: (toolCallID: string) => void; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
	const run = row.run
	// Streaming text soft-seals the live tail: once the model starts writing,
	// the trailing tool group collapses instead of staying open until the
	// output step flushes at the next tool call (or until the run settles).
	const textStreaming = Boolean(run.assistantText) && !row.assistantTailAttached
	const running = run.status === 'running'
	const tokenNote = row.isLast && run.totalTokens !== undefined && (
		<div className="token-note">
			This turn: {run.totalTokens.toLocaleString()} tokens
			{Boolean(run.cachedTokens) && ` · Cache hit ${run.cachedTokens?.toLocaleString()}`}
			{Boolean(run.cacheWriteTokens) && ` · Cache write ${run.cacheWriteTokens?.toLocaleString()}`}
			{Boolean(run.reasoningTokens) && ` · Reasoning ${run.reasoningTokens?.toLocaleString()}`}
		</div>
	)
	return (
		<article className="message assistant transient">
			<div className="message-content">
				<ProcessTimeline steps={row.steps} live={row.isLast && running && !textStreaming} onCancelTool={onCancelTool} sessionNames={sessionNames} workspaceRoot={workspaceRoot} />
				{row.isLast && !row.assistantTailAttached && run.assistantText && <MarkdownMessage text={run.assistantText} streaming cursor={running} />}
				{row.isLast && !row.assistantTailAttached && running && !run.assistantText && <div className="message-text assistant-stream"><span className="cursor" aria-hidden="true" /></div>}
				{tokenNote}
			</div>
		</article>
	)
}

function ActiveCursorRow({ run }: { run: ActiveRun }) {
	const running = run.status === 'running'
	return (
		<article className="message assistant transient active-cursor">
			<div className="message-content">
				{run.assistantText
					? <MarkdownMessage text={run.assistantText} streaming cursor={running} />
					: running && <div className="message-text assistant-stream"><span className="cursor" aria-hidden="true" /></div>}
			</div>
		</article>
	)
}

// ProviderRetryStatus counts down the backoff delay, then switches to a
// "reconnecting" label while the retry attempt itself is in flight.
function ProviderRetryStatus({ retry }: { retry: NonNullable<ActiveRun['providerRetry']> }) {
  const [remainingMS, setRemainingMS] = useState(() => Math.max(0, retry.delayMS))
  useEffect(() => {
    setRemainingMS(Math.max(0, retry.delayMS))
    if (retry.delayMS <= 0) return
    const startedAt = Date.now()
    const timer = window.setInterval(() => {
      setRemainingMS(Math.max(0, retry.delayMS - (Date.now() - startedAt)))
    }, 250)
    return () => window.clearInterval(timer)
  }, [retry.attempt, retry.delayMS])
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

function TurnErrorRow({ message, canContinue, onContinue, onDismiss }: { message: string; canContinue: boolean; onContinue: () => void; onDismiss: () => void }) {
	return (
		<div className="turn-error" role="alert">
			<WarningIcon />
			<div className="turn-error-copy">
				<strong>Turn failed</strong>
				<p>{message}</p>
			</div>
			{canContinue && <button className="message-tool-button" onClick={onContinue} title="Continue interrupted run"><RetryIcon />Continue</button>}
			<button className="turn-error-dismiss" onClick={onDismiss} aria-label="Dismiss error" title="Dismiss">×</button>
		</div>
	)
}

function RefreshErrorRow({ onRetryRefresh }: { onRetryRefresh: () => void }) {
	return (
		<div className="turn-error" role="alert">
			<WarningIcon />
			<div className="turn-error-copy">
				<strong>Refresh needed</strong>
				<p>The session state may be outdated. Refresh to see the latest.</p>
			</div>
			<button className="message-tool-button" onClick={onRetryRefresh} title="Refresh session">Refresh</button>
		</div>
	)
}

function InterruptedRow({ onDismiss }: { onDismiss: () => void }) {
	return (
		<div className="turn-error" role="alert">
			<WarningIcon />
			<div className="turn-error-copy">
				<strong>Session interrupted</strong>
				<p>The last turn did not complete. You can retry it.</p>
			</div>
			<button className="turn-error-dismiss" onClick={onDismiss} aria-label="Dismiss" title="Dismiss">×</button>
		</div>
	)
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
      {cursor && <span className="cursor" aria-hidden="true" />}
    </div>
  )
}

function HistoricalProcess({ entry, sessionNames, workspaceRoot }: { entry: Extract<ConversationRow, { kind: 'process' }>; sessionNames?: Record<string, string>; workspaceRoot?: string }) {
	return (
		<article className="message assistant process-message" data-seq={entry.lastSeq}>
			<div className="message-content">
				<ProcessTimeline steps={entry.steps} sessionNames={sessionNames} workspaceRoot={workspaceRoot} />
			</div>
		</article>
	)
}
