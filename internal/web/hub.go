package web

import (
	"encoding/json"
	"strconv"
	"sync"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/storage"
)

// DefaultWindow is how many trailing records a fresh connection receives
// before scroll-back kicks in.
const DefaultWindow = 200

// clientBufferSize bounds one SSE client's frame backlog. Overflow closes
// the connection; EventSource reconnects and replays. The agent is never
// blocked: fan-out is non-blocking by implementation constraint (a
// blocked hub would block the committer).
const clientBufferSize = 256

// Hub is the single consumer of the session's record and event streams
// and the fan-out point for every web client. It retains the flat record
// stream (its own subscription view), the digest chain with an index for
// cursor lookups, and the last status notices for /api/state.
type Hub struct {
	session *agent.Session
	wake    chan struct{}

	// run is the hub's single loop goroutine.
	done chan struct{}

	mu      sync.Mutex
	records []storage.Record
	chain   []string       // chain[k] = h_{k+1}; chainAt(k) below hides the indexing
	index   map[string]int // digest -> number of records folded (k)
	clients map[*client]struct{}
	genesis string

	// Status notices cached for /api/state (events are transient; the
	// agent keeps no queryable history).
	lastPressure string
	lastRetry    string
	lastNotice   string

	// events is the currently attached run event channel (nil between
	// runs). attachMu is not needed: only the loop goroutine touches it.
	events <-chan model.Event

	stopOnce sync.Once
}

type client struct {
	ch   chan []byte
	done chan struct{}
}

// NewHub starts the hub for the session.
func NewHub(session *agent.Session) *Hub {
	h := &Hub{
		session: session,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		index:   make(map[string]int),
		clients: make(map[*client]struct{}),
		genesis: chainGenesis(session.ID()),
	}
	go h.run()
	return h
}

// Stop terminates the hub loop and releases its subscription: a
// stopped hub must never sit as a dead subscriber whose full buffer
// would block the committer on the next commit.
func (h *Hub) Stop() {
	h.stopOnce.Do(func() { close(h.done) })
}

// Wake reattaches the run event channel. The POST /api/send handler
// calls it right after Send returns: Send publishes s.events under the
// session lock, so the channel send/receive happens-before guarantees
// the hub sees the new channel, with zero polling latency (the event
// channel is small; late attachment could stall the run goroutine).
func (h *Hub) Wake() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Notice publishes an out-of-band status notice (e.g. an async compaction
// failure) to every client and caches it for /api/state.
func (h *Hub) Notice(text string) {
	h.mu.Lock()
	h.lastNotice = text
	h.broadcastStatusLocked()
	h.mu.Unlock()
}

func (h *Hub) run() {
	// h.done is closed by Stop only — a return here (session closed)
	// must not race a second close.
	sub := h.session.Subscribe()
	// Whatever ends the loop, the subscription is released: a dead
	// subscriber blocks the committer once its buffer fills.
	defer sub.Stop()
	// Seed the mirror and the chain from the atomic snapshot: no record
	// can be missed or duplicated between Initial and the channel.
	h.mu.Lock()
	for _, r := range sub.Initial() {
		h.extendLocked(r)
	}
	h.mu.Unlock()

	for {
		select {
		case rec := <-sub.Records():
			h.mu.Lock()
			h.extendLocked(rec)
			h.fanoutLocked(encodeRecordFrame(rec))
			h.mu.Unlock()
		case ev, ok := <-h.events:
			if !ok {
				// Run settled. The run's final commit precedes settle
				// precedes the channel close, so any trailing record is
				// already queued on the subscription.
				h.events = nil
				drainThenStatus(h, sub)
				continue
			}
			h.handleEvent(ev)
		case <-h.wake:
			// Attach by channel identity: spurious and duplicate wakes
			// are the norm (the previous run's channel may still be
			// open when the next Send is accepted), and two drainers
			// on one channel would silently split the events.
			if ch := h.session.Events(); ch != nil && ch != h.events {
				h.events = ch
				// The run may have settled before the wake arrived —
				// the attached channel can already be closed — and the
				// run's records are queued ahead of the status. A status
				// frame must never overtake pending records either way.
				drainThenStatus(h, sub)
			}
		case <-sub.Done():
			return
		case <-h.done:
			return
		}
	}
}

// drainThenStatus forwards every record already queued on the
// subscription, then broadcasts the current run state. A status frame
// must never overtake pending records — "idle" racing ahead of the
// final record reads as a lost answer. Runs on the hub loop goroutine.
func drainThenStatus(h *Hub, sub *agent.Subscription) {
	for {
		select {
		case rec := <-sub.Records():
			h.mu.Lock()
			h.extendLocked(rec)
			h.fanoutLocked(encodeRecordFrame(rec))
			h.mu.Unlock()
		default:
			h.mu.Lock()
			h.broadcastStatusLocked()
			h.mu.Unlock()
			return
		}
	}
}

// handleEvent fans one transient event out to the clients and refreshes
// the cached status notices. Runs on the hub loop goroutine.
func (h *Hub) handleEvent(ev model.Event) {
	switch e := ev.(type) {
	case model.ContextPressureEvent:
		h.mu.Lock()
		h.lastPressure = formatPressure(e)
		h.mu.Unlock()
	case model.ProviderRetryEvent:
		h.mu.Lock()
		h.lastRetry = formatRetry(e)
		h.mu.Unlock()
	}
	if frame, ok := encodeEventFrame(ev); ok {
		h.mu.Lock()
		h.fanoutLocked(encodeFrame(frame))
		h.mu.Unlock()
	}
}

func formatPressure(e model.ContextPressureEvent) string {
	return "context exceeds the estimated budget (" +
		strconv.Itoa(e.EstimatedTokens) + " ≥ " + strconv.Itoa(e.HardInputLimit) + " tokens); no compaction possible"
}

func formatRetry(e model.ProviderRetryEvent) string {
	return "retrying model request (attempt " + strconv.Itoa(e.Attempt) + "/" + strconv.Itoa(e.MaxAttempts) + ")"
}

// extendLocked appends one record to the mirror and extends the chain.
// Callers hold h.mu.
func (h *Hub) extendLocked(r storage.Record) {
	h.records = append(h.records, r)
	prev := h.chainHeadLocked()
	next := chainStep(prev, r)
	h.chain = append(h.chain, next)
	h.index[next] = len(h.records)
}

// chainHeadLocked returns the digest after all retained records.
func (h *Hub) chainHeadLocked() string {
	if len(h.chain) == 0 {
		return h.genesis
	}
	return h.chain[len(h.chain)-1]
}

// chainAtLocked returns the digest after the first k records.
func (h *Hub) chainAtLocked(k int) string {
	if k <= 0 {
		return h.genesis
	}
	if k > len(h.chain) {
		return h.chainHeadLocked()
	}
	return h.chain[k-1]
}

// fanoutLocked delivers a pre-encoded frame to every client without ever
// blocking: a full client buffer closes that connection (it reconnects
// and replays); the hub — and behind it the committer — always proceeds.
// Callers hold h.mu.
func (h *Hub) fanoutLocked(frame []byte) {
	for c := range h.clients {
		select {
		case c.ch <- frame:
		default:
			h.dropClientLocked(c)
		}
	}
}

func (h *Hub) dropClientLocked(c *client) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	close(c.done)
}

// broadcastStatusLocked sends a status frame reflecting the current run
// state. Callers hold h.mu.
func (h *Hub) broadcastStatusLocked() {
	running := h.session.Running()
	frame := encodeFrame(Frame{
		Kind:    "status",
		Running: &running,
		Digest:  h.chainHeadLocked(),
		Count:   len(h.records),
		Notice:  h.lastNotice,
	})
	h.fanoutLocked(frame)
}

// register connects one SSE client. The returned snapshot frame and the
// client's live registration are produced under one lock section, so no
// record is missed or duplicated between them. from is the client's
// digest (empty on first connect): a hit resumes after it, a miss (or
// no from) replays the trailing window.
func (h *Hub) register(from string, window int) (*client, []byte, error) {
	if window <= 0 {
		window = DefaultWindow
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	total := len(h.records)
	start := total - window
	if start < 0 {
		start = 0
	}
	resumed := false
	if from != "" {
		if k, ok := h.index[from]; ok && k <= total {
			start = k
			resumed = true
		}
	}
	windowed := make([]storage.Record, 0, total-start)
	windowed = append(windowed, h.records[start:total]...)
	encoded, err := json.Marshal(windowed)
	if err != nil {
		return nil, nil, err
	}

	snapshot := h.session.Snapshot()
	running := h.session.Running()
	f := Frame{
		Kind:    "snapshot",
		Anchor:  h.chainAtLocked(start),
		Resumed: resumed,
		Total:   total,
		More:    start > 0,
		Records: encoded,
		Running: &running,
		Digest:  h.chainHeadLocked(),
	}
	if snapshot.Streaming != nil {
		f.Streaming = &StreamingWire{
			Item:      snapshot.Streaming.ItemID,
			Content:   snapshot.Streaming.Message.Content,
			Reasoning: snapshot.Streaming.Message.ReasoningContent,
		}
	}
	if snapshot.Running != nil {
		f.Tool = &ToolWire{
			Item: snapshot.Running.ItemID,
			Name: snapshot.Running.ToolCall.Name,
			Args: snapshot.Running.ToolCall.Arguments,
		}
	}

	c := &client{ch: make(chan []byte, clientBufferSize), done: make(chan struct{})}
	h.clients[c] = struct{}{}
	return c, encodeFrame(f), nil
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	h.dropClientLocked(c)
	h.mu.Unlock()
}

// history serves scroll-back: the limit records before the digest
// anchor, with the anchor that precedes them, so the client can verify
// fold(anchor, records) == its window's start digest. A stale or unknown
// anchor reports stale=true (the client must resynchronize — serving a
// different chain here would fake drift).
func (h *Hub) history(before string, limit int) (records []storage.Record, anchor string, more bool, stale bool) {
	if limit <= 0 || limit > DefaultWindow {
		limit = DefaultWindow
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	k, ok := h.index[before]
	if !ok || before == h.genesis {
		// before == genesis: the client already holds everything.
		return nil, "", false, before != h.genesis
	}
	start := k - limit
	if start < 0 {
		start = 0
	}
	records = append([]storage.Record(nil), h.records[start:k]...)
	return records, h.chainAtLocked(start), start > 0, false
}

// state is the /api/state summary.
type state struct {
	Session    string         `json:"session"`
	Running    bool           `json:"running"`
	Compacting bool           `json:"compacting"`
	Count      int            `json:"count"`
	Digest     string         `json:"digest"`
	Streaming  *StreamingWire `json:"streaming,omitempty"`
	Pressure   string         `json:"pressure,omitempty"`
	Retry      string         `json:"retry,omitempty"`
	Notice     string         `json:"notice,omitempty"`
}

func (h *Hub) state() state {
	snapshot := h.session.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := state{
		Session:    h.session.ID(),
		Running:    h.session.Running(),
		Compacting: snapshot.Compacting != nil,
		Count:      len(h.records),
		Digest:     h.chainHeadLocked(),
		Pressure:   h.lastPressure,
		Retry:      h.lastRetry,
		Notice:     h.lastNotice,
	}
	// The wire DTO, not the internal StreamingItem (untagged keys, the
	// whole Message shape) — the same trap the snapshot frame fixed.
	if snapshot.Streaming != nil {
		s.Streaming = &StreamingWire{
			Item:      snapshot.Streaming.ItemID,
			Content:   snapshot.Streaming.Message.Content,
			Reasoning: snapshot.Streaming.Message.ReasoningContent,
		}
	}
	return s
}
