package web

import (
	"encoding/json"
	"strconv"
	"sync"

	"github.com/rexzhao/simple-agent/agent"
	"github.com/rexzhao/simple-agent/model"
	"github.com/rexzhao/simple-agent/storage"
)

// DefaultWindow is how many trailing records a fresh connection receives
// before scroll-back kicks in.
const DefaultWindow = 200

// clientBufferSize bounds one SSE client frame backlog. Overflow closes
// the connection; EventSource reconnects and replays.
const clientBufferSize = 256

// Hub is the SSE bridge between one session's stream and the web
// clients. It subscribes ONCE to the session's single ordered stream
// (milestone A step 3): RecordCommitted → record frames, Transient →
// event frames, Status → status frames. The ordering is the stream's
// seq order — the hub needs no drain patches, no Wake, no per-run
// channel attach. The digest chain is maintained by the hub (the
// stream carries records, not digests).
type Hub struct {
	session *agent.Session
	stream  *agent.StreamSubscription

	// done is closed by Stop.
	done chan struct{}

	mu      sync.Mutex
	records []storage.Record
	chain   []string       // chain[k] = h_{k+1}; chainAt(k) below hides the indexing
	index   map[string]int // digest -> number of records folded (k)
	clients map[*client]struct{}
	genesis string

	// The running/compacting MIRROR: what the hub has processed from
	// the stream, not the live session state. Every status frame reads
	// this (H2): the hub's digest/count trail the session by definition,
	// so reading live running/compacting alongside mirror digests would
	// emit frames that contradict the stream's order — e.g. an idle
	// Notice frame racing ahead of the still-unprocessed final records.
	// Initialized from the subscription snapshot (NewHub/resubscribe),
	// updated ONLY by stream Status events.
	running    bool
	compacting bool
	// processedSeq is the seq of the last SessionEvent the hub handled
	// (the snapshot's StartSeq before any). Out-of-band frames carry it
	// so the fan-out's seq filter treats them consistently with stream
	// frames: a client whose snapshot is newer (startSeq >= processedSeq)
	// skips them.
	processedSeq uint64

	// Status notices cached for /api/state (events are transient; the
	// agent keeps no queryable history).
	lastPressure string
	lastRetry    string
	lastNotice   string

	// onClients, when set, is called (outside h.mu) on every subscriber
	// count transition — the manager's eviction clock restarts on both
	// attach and detach, so idle time is measured from the moment the
	// last activity ended, not from some stale request.
	onClients func(count int)

	stopOnce sync.Once
}

type client struct {
	ch   chan []byte
	done chan struct{}
	// startSeq is the stream seq of the client's connect-time snapshot
	// (real-time at registration). Stream-originated Transient/Status
	// frames with seq <= startSeq are already reflected in that
	// snapshot and are skipped for this client (H3: otherwise a client
	// connecting while the hub trails the session would see the same
	// deltas twice — once folded into the snapshot's Streaming content,
	// once replayed from the hub's backlog). Record frames are exempt:
	// the snapshot's records come from the hub's mirror, so record
	// frames still carry records the snapshot lacks.
	startSeq uint64
}

// NewHub starts the hub for the session. The mirror is seeded
// SYNCHRONOUSLY from the stream's snapshot, before any client can
// register. The stream subscription is the hub's single event source.
func NewHub(session *agent.Session) *Hub {
	h := &Hub{
		session: session,
		done:    make(chan struct{}),
		index:   make(map[string]int),
		clients: make(map[*client]struct{}),
	}
	stream := session.Stream()
	snap := stream.Initial()
	h.mu.Lock()
	// The chain's genesis follows the segment (§14.4): a hub opened on
	// a compacted session folds from the compaction-seeded genesis, the
	// same one the pre-restart hub switched to at truncation time. It
	// must be set BEFORE the seeding folds (chainHeadLocked reads it
	// for the very first step).
	h.genesis = segmentGenesis(session.ID(), snap.Records)
	for _, r := range snap.Records {
		h.extendLocked(r)
	}
	// The mirror starts at the subscription snapshot: running/compacting
	// from its status flags, processedSeq at its StartSeq.
	h.running = snap.StatusRunning
	h.compacting = snap.StatusCompacting
	h.processedSeq = snap.StartSeq
	h.mu.Unlock()
	h.stream = stream
	go h.run()
	return h
}

// Stop terminates the hub loop and releases its stream subscription.
func (h *Hub) Stop() {
	h.stopOnce.Do(func() { close(h.done) })
}

// Notice publishes an out-of-band status notice (e.g. an async compaction
// failure) to every client and caches it for /api/state. The frame's
// running/compacting come from the hub mirror (H2) and carry the hub's
// processed seq — clients whose snapshot is newer skip it (H3).
func (h *Hub) Notice(text string) {
	h.mu.Lock()
	h.lastNotice = text
	h.broadcastStatusLocked()
	h.mu.Unlock()
}

// ClearNotices drops the cached transient notices and pushes a status
// frame. The rule is uniform: notices describe a PAST event; the
// user's next accepted action supersedes them.
func (h *Hub) ClearNotices() {
	h.mu.Lock()
	h.lastNotice = ""
	h.lastPressure = ""
	h.lastRetry = ""
	h.broadcastStatusLocked()
	h.mu.Unlock()
}

// run is the hub's single loop goroutine. It consumes SessionEvent
// from the stream in seq order — the ordering guarantee is structural
// (the stream assigns seq under the session lock in the same critical
// section as the state change). Lost means the hub's subscription was
// evicted (buffer overflow); the hub re-subscribes and reissues a
// window snapshot to its SSE clients.
func (h *Hub) run() {
	for {
		select {
		case ev := <-h.stream.Events():
			h.handleStreamEvent(ev)
		case <-h.stream.Lost():
			h.resubscribe()
		case <-h.done:
			h.stream.Stop()
			return
		}
	}
}

// handleStreamEvent maps one SessionEvent onto SSE frames and the
// hub's mirror. Runs on the hub loop goroutine.
func (h *Hub) handleStreamEvent(ev agent.SessionEvent) {
	switch e := ev.(type) {
	case agent.RecordCommitted:
		h.mu.Lock()
		if e.Record.Type == storage.TypeContextCompacted && len(h.records) > 0 {
			// Segment switch (§14.4): the session truncated its record
			// stream to [session.created, compaction]; the mirror
			// follows, the chain re-genesis from the compaction's
			// ItemID, and every connected client gets a fresh snapshot
			// (resumed=false — its old anchor belongs to the
			// truncated-away chain). No record frame for this commit:
			// folding it onto a client's old chain would be drift.
			var head []storage.Record
			if len(h.records) > 0 && h.records[0].Type == storage.TypeSessionCreated {
				head = append(head, h.records[0])
			}
			segment := append(head, e.Record)
			h.records = nil
			h.chain = h.chain[:0]
			for k := range h.index {
				delete(h.index, k)
			}
			// The SAME derivation a fresh Open runs (§14.4): the
			// truncation's segment shape and the reopened segment are
			// identical by construction, so the genesis — and with it
			// the whole chain — is restart-stable for every shape.
			h.genesis = segmentGenesis(h.session.ID(), segment)
			for _, r := range segment {
				h.extendLocked(r)
			}
			h.processedSeq = e.Seq
			h.fanoutLocked(h.snapshotFrameLocked(h.session.SnapshotStreamState()), 0, false)
			h.mu.Unlock()
			return
		}
		h.extendLocked(e.Record)
		h.processedSeq = e.Seq
		// Record frames bypass the seq filter: a client's snapshot
		// carries the hub MIRROR's records, so record frames still
		// deliver records the snapshot lacks (H3's exemption).
		h.fanoutLocked(encodeRecordFrame(e.Record), e.Seq, false)
		h.mu.Unlock()
	case agent.Transient:
		h.handleEvent(e.Event, e.Seq)
	case agent.Status:
		h.mu.Lock()
		// The mirror IS the event's fields: update it, then broadcast
		// from it — every status frame now reads state the hub has
		// actually processed, in stream order (H2).
		h.running = e.Running
		h.compacting = e.Compacting
		h.processedSeq = e.Seq
		h.broadcastStatusLocked()
		h.mu.Unlock()
	}
}

// snapshotFrameLocked builds a full snapshot frame from the mirror
// plus one atomic state snapshot: the trailing window of records, the
// segment anchor, and the transient slots. Shared by the eviction
// reissue and the compaction segment switch. Callers hold h.mu.
func (h *Hub) snapshotFrameLocked(state agent.StreamSnapshot) []byte {
	total := len(h.records)
	start := total - DefaultWindow
	if start < 0 {
		start = 0
	}
	windowed := append([]storage.Record(nil), h.records[start:total]...)
	encoded, _ := json.Marshal(windowed)
	running := state.StatusRunning
	compacting := state.StatusCompacting
	f := Frame{
		Kind:       "snapshot",
		Anchor:     h.chainAtLocked(start),
		Resumed:    false,
		Total:      total,
		More:       start > 0,
		Records:    encoded,
		Running:    &running,
		Compacting: &compacting,
		Digest:     h.chainHeadLocked(),
	}
	if state.Streaming != nil {
		f.Streaming = &StreamingWire{
			Item:      state.Streaming.ItemID,
			Content:   state.Streaming.Message.Content,
			Reasoning: state.Streaming.Message.ReasoningContent,
		}
	}
	if state.Running != nil {
		f.Tool = &ToolWire{
			Item:             state.Running.ItemID,
			CallID:           state.Running.ToolCall.ID,
			Name:             state.Running.ToolCall.Name,
			Args:             state.Running.ToolCall.Arguments,
			AwaitingApproval: state.Running.AwaitingApproval,
		}
	}
	for _, entry := range state.Steered {
		f.Steered = append(f.Steered, SteeredWire{Item: entry.ItemID, Text: entry.Text})
	}
	return encodeFrame(f)
}

// resubscribe recovers from an evicted stream subscription: stop the
// old one, start a fresh one, rebuild the mirror from the snapshot,
// and reissue a window snapshot to every connected SSE client.
func (h *Hub) resubscribe() {
	h.stream.Stop()
	stream := h.session.Stream()
	snap := stream.Initial()

	h.mu.Lock()
	// Rebuild the mirror from the snapshot (the hub may have missed
	// records while evicted). The genesis is re-derived from the
	// segment (a compaction while evicted switched it) BEFORE the
	// seeding folds.
	h.genesis = segmentGenesis(h.session.ID(), snap.Records)
	h.records = nil
	h.chain = h.chain[:0]
	for k := range h.index {
		delete(h.index, k)
	}
	for _, r := range snap.Records {
		h.extendLocked(r)
	}
	// The status mirror and processed position restart at the fresh
	// snapshot (H2): everything up to its StartSeq is reflected in it.
	h.running = snap.StatusRunning
	h.compacting = snap.StatusCompacting
	h.processedSeq = snap.StartSeq
	// Window reissue: a fresh snapshot frame to every connected client.
	// Each client sees resumed=false + the new anchor and does a clean
	// rebuild (the client's existing code handles this path — the same
	// as a missed from= on reconnect).
	h.fanoutLocked(h.snapshotFrameLocked(snap), 0, false)
	// A status frame alongside carries the cached notice (the snapshot
	// frame does not); its running/compacting read the mirror just
	// initialized from the same snapshot, so the two frames agree.
	h.broadcastStatusLocked()
	h.mu.Unlock()
	h.stream = stream

}

// handleEvent fans one transient event out to the clients and refreshes
// the cached status notices. Runs on the hub loop goroutine.
func (h *Hub) handleEvent(ev model.Event, seq uint64) {
	switch e := ev.(type) {
	case model.ContextPressureEvent:
		h.mu.Lock()
		h.lastPressure = formatPressure(e)
		h.processedSeq = seq
		h.mu.Unlock()
	case model.ProviderRetryEvent:
		h.mu.Lock()
		h.lastRetry = formatRetry(e)
		h.processedSeq = seq
		h.mu.Unlock()
	case model.CompactionFailedEvent:
		h.mu.Lock()
		h.lastNotice = "compaction failed (soft pressure; run continues): " + e.Reason
		h.processedSeq = seq
		h.broadcastStatusLocked()
		h.mu.Unlock()
	case model.CompactionCompletedEvent:
		h.mu.Lock()
		h.lastNotice = ""
		h.processedSeq = seq
		h.broadcastStatusLocked()
		h.mu.Unlock()
	default:
		h.mu.Lock()
		h.processedSeq = seq
		h.mu.Unlock()
	}
	if frame, ok := encodeEventFrame(ev); ok {
		h.mu.Lock()
		h.fanoutLocked(encodeFrame(frame), seq, true)
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
// and replays); the hub — and behind it the stream — always proceeds.
// seq is the stream position the frame reflects; skipKnown filters
// stream-originated Transient/Status frames for clients whose snapshot
// already includes that position (seq <= client.startSeq — H3: the
// snapshot's transient slots are real-time, so replaying those events
// would duplicate deltas). Record frames pass skipKnown=false: the
// snapshot's records come from the hub mirror, which may lag the
// client's snapshot — record frames are how it catches up. Callers
// hold h.mu.
func (h *Hub) fanoutLocked(frame []byte, seq uint64, skipKnown bool) {
	for c := range h.clients {
		if skipKnown && seq <= c.startSeq {
			continue // already reflected in this client's snapshot
		}
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
	// A notice is NEW information for every client — the snapshot
	// frame carries no Notice field, so the seq filter must NOT skip
	// notice-bearing frames for clients whose snapshot is at or past
	// the hub's processed position. The positional fields are omitted
	// on purpose: at notice time the hub's mirror may trail a fresh
	// client's snapshot, and re-sending stale running/compacting/
	// digest would regress it (its next positional truth is the next
	// stream status frame, which the filter already orders). Both
	// clients treat absent positional fields as "no change".
	if h.lastNotice != "" {
		h.fanoutLocked(encodeFrame(Frame{Kind: "status", Notice: h.lastNotice}), 0, false)
		return
	}
	// Positional frame: reads the MIRROR (H2) — running/compacting as
	// the hub has processed them, alongside the mirror's digest/count —
	// and carries the hub's processed seq, so clients whose snapshot is
	// newer skip it (H3) instead of being regressed.
	running := h.running
	compacting := h.compacting
	frame := encodeFrame(Frame{
		Kind:       "status",
		Running:    &running,
		Compacting: &compacting,
		Digest:     h.chainHeadLocked(),
		Count:      len(h.records),
	})
	h.fanoutLocked(frame, h.processedSeq, true)
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
	// NOT deferred: the client-count observer must run outside h.mu
	// (it takes the manager's lock).
	c, f, err := h.registerLocked(from, window)
	count := len(h.clients)
	onClients := h.onClients
	h.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if onClients != nil {
		onClients(count)
	}
	return c, f, nil
}

// registerLocked is register's body; the caller holds h.mu. Split so
// tests can register at a controlled stream position (the hub loop
// frozen mid-backlog) — the (P, S] window the seq filter exists for.
func (h *Hub) registerLocked(from string, window int) (*client, []byte, error) {
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

	// Snapshot-frame state comes from ONE atomic snapshot — the light
	// variant without the record copy (records are the hub mirror's,
	// above): Snapshot()+Running()+Compacting() would be three separate
	// lock acquisitions racing each other mid-transition. The snapshot
	// is REAL-TIME by design: everything with seq <= its StartSeq is
	// reflected in it, and the fan-out's seq filter keeps replayed
	// transients from duplicating it (H3).
	snapshot := h.session.SnapshotStreamState()
	running := snapshot.StatusRunning
	compacting := snapshot.StatusCompacting
	f := Frame{
		Kind:       "snapshot",
		Anchor:     h.chainAtLocked(start),
		Resumed:    resumed,
		Total:      total,
		More:       start > 0,
		Records:    encoded,
		Running:    &running,
		Compacting: &compacting,
		Digest:     h.chainHeadLocked(),
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
			Item:             snapshot.Running.ItemID,
			CallID:           snapshot.Running.ToolCall.ID,
			Name:             snapshot.Running.ToolCall.Name,
			Args:             snapshot.Running.ToolCall.Arguments,
			AwaitingApproval: snapshot.Running.AwaitingApproval,
		}
	}
	for _, entry := range snapshot.Steered {
		f.Steered = append(f.Steered, SteeredWire{Item: entry.ItemID, Text: entry.Text})
	}

	c := &client{
		ch:       make(chan []byte, clientBufferSize),
		done:     make(chan struct{}),
		startSeq: snapshot.StartSeq,
	}
	h.clients[c] = struct{}{}
	return c, encodeFrame(f), nil
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	h.dropClientLocked(c)
	count := len(h.clients)
	onClients := h.onClients
	h.mu.Unlock()
	if onClients != nil {
		onClients(count)
	}
}

// Subscribers reports the current subscriber count.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// SetClientObserver installs the subscriber-count transition callback.
func (h *Hub) SetClientObserver(fn func(count int)) {
	h.mu.Lock()
	h.onClients = fn
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
	Steered    []SteeredWire  `json:"steered,omitempty"`
	Pressure   string         `json:"pressure,omitempty"`
	Retry      string         `json:"retry,omitempty"`
	Notice     string         `json:"notice,omitempty"`
}

func (h *Hub) state() state {
	// One atomic snapshot for the transient slots — the same race
	// argument as register, and the light variant (no record copy:
	// /api/state is polled). running/compacting read the hub MIRROR
	// (H2), keeping /api/state consistent with the frames the same
	// clients see on the stream.
	snapshot := h.session.SnapshotStreamState()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := state{
		Session:    h.session.ID(),
		Running:    h.running,
		Compacting: h.compacting,
		Count:      len(h.records),
		Digest:     h.chainHeadLocked(),
		Pressure:   h.lastPressure,
		Retry:      h.lastRetry,
		Notice:     h.lastNotice,
	}
	if snapshot.Streaming != nil {
		s.Streaming = &StreamingWire{
			Item:      snapshot.Streaming.ItemID,
			Content:   snapshot.Streaming.Message.Content,
			Reasoning: snapshot.Streaming.Message.ReasoningContent,
		}
	}
	for _, entry := range snapshot.Steered {
		s.Steered = append(s.Steered, SteeredWire{Item: entry.ItemID, Text: entry.Text})
	}
	return s
}
