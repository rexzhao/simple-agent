package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexzhao/simple-agent/internal/model"
)

func testSQLiteSession(t *testing.T) (*V2Store, SessionV2, string) {
	t.Helper()
	root := t.TempDir()
	store := NewV2Store(root)
	session, err := store.SaveMetadata(SessionV2{ID: "session-test", DisplayName: "Test session"})
	if err != nil {
		t.Fatalf("SaveMetadata() error = %v", err)
	}
	return store, session, filepath.Join(root, session.ID, "session.db")
}

func TestListStatesReadsOnlyCompactState(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	if _, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{
		Role:    model.MessageRoleUser,
		Content: "hello",
	})); err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}

	// Make the immutable event payload deliberately large and invalid JSON. A
	// state/list query must remain correct because it never reads events or the
	// complete item projection.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	// Keep this close to the production-sized history we care about without
	// requiring a slow loop of individual event appends. The payload is not
	// valid JSON on purpose: ListStates must not deserialize it at all.
	largePayload := strings.Repeat("x", 100*1024*1024)
	if _, err := db.Exec("UPDATE events SET payload = ?", largePayload); err != nil {
		t.Fatalf("inflate events error = %v", err)
	}
	// Remove the projection/event tables entirely. A compact list read must
	// still work; this is a structural assertion, not a timing threshold.
	if _, err := db.Exec("DROP TABLE events; DROP TABLE items;"); err != nil {
		t.Fatalf("drop history tables error = %v", err)
	}

	states, err := store.ListStates(V2ListOptions{All: true})
	if err != nil {
		t.Fatalf("ListStates() error = %v", err)
	}
	if len(states) != 1 || states[0].ID != session.ID {
		t.Fatalf("ListStates() = %#v, want one state %q", states, session.ID)
	}
	if len(states[0].Items) != 0 {
		t.Fatalf("ListStates() returned %d items, want compact state without history", len(states[0].Items))
	}
	if states[0].LastSeq != 1 {
		t.Fatalf("ListStates().LastSeq = %d, want 1", states[0].LastSeq)
	}
}

func TestSQLiteRunAndTurnLifecycleStoresOrdinalsAndRelations(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	first, err := store.CreateRun(session.ID, "run-1", "", []byte(`{"content":"one"}`), time.Now())
	if err != nil {
		t.Fatalf("CreateRun(first) error = %v", err)
	}
	if _, err := store.StartTurn(session.ID, first.ID, "turn-1", 0, time.Now()); err != nil {
		t.Fatalf("StartTurn(1) error = %v", err)
	}
	if _, err := store.SetTurnStatus(session.ID, first.ID, "turn-1", TurnStatusCommitted, time.Now()); err != nil {
		t.Fatalf("SetTurnStatus(1) error = %v", err)
	}
	if _, err := store.SetRunStatus(session.ID, first.ID, RunStatusCommitted, time.Now()); err != nil {
		t.Fatalf("SetRunStatus(1) error = %v", err)
	}
	second, err := store.CreateRun(session.ID, "run-2", first.ID, []byte(`{"content":"two"}`), time.Now())
	if err != nil {
		t.Fatalf("CreateRun(second) error = %v", err)
	}
	if second.PreviousRunID != first.ID {
		t.Fatalf("second previous_run_id = %q, want %q", second.PreviousRunID, first.ID)
	}
	if _, err := store.StartTurn(session.ID, second.ID, "turn-2", 0, time.Now()); err != nil {
		t.Fatalf("StartTurn(2) error = %v", err)
	}
	turns, err := store.ListTurns(session.ID, second.ID)
	if err != nil {
		t.Fatalf("ListTurns() error = %v", err)
	}
	if len(turns) != 1 || turns[0].Ordinal != 1 || turns[0].RunID != second.ID {
		t.Fatalf("turns = %#v, want one ordinal-1 turn for second run", turns)
	}
	state, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.RunningRunID != second.ID || state.RunningTurnID != "turn-2" || state.LatestRunID != second.ID || state.LastRunID != first.ID {
		t.Fatalf("run state = %#v, want second running and first last", state)
	}
}

func TestSQLiteRunFailureAndCancellationSettleTurnStateTogether(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	failed, err := store.CreateRun(session.ID, "run-failed", "", []byte(`{"content":"one"}`), time.Now())
	if err != nil {
		t.Fatalf("CreateRun(failed) error = %v", err)
	}
	if _, err := store.StartTurn(session.ID, failed.ID, "turn-failed", 0, time.Now()); err != nil {
		t.Fatalf("StartTurn(failed) error = %v", err)
	}
	state, err := store.SetRunStatus(session.ID, failed.ID, RunStatusFailed, time.Now())
	if err != nil {
		t.Fatalf("SetRunStatus(failed) error = %v", err)
	}
	if state.RunningRunID != "" || state.RunningTurnID != "" || state.InterruptedRunID != failed.ID || state.InterruptedTurnID != "turn-failed" || state.LastRunStatus != RunStatusFailed {
		t.Fatalf("failed state = %#v, want settled failed run and resumable turn", state)
	}
	runs, err := store.ListRuns(session.ID)
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	turns, err := store.ListTurns(session.ID, failed.ID)
	if err != nil {
		t.Fatalf("ListTurns(failed) error = %v", err)
	}
	if len(runs) != 1 || runs[0].Status != RunStatusFailed || len(turns) != 1 || turns[0].Status != TurnStatusFailed {
		t.Fatalf("failed durable rows = runs %#v turns %#v, want both settled failed", runs, turns)
	}

	cancelled, err := store.CreateRun(session.ID, "run-cancelled", failed.ID, []byte(`{"content":"two"}`), time.Now())
	if err != nil {
		t.Fatalf("CreateRun(cancelled) error = %v", err)
	}
	state, err = store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after new run) error = %v", err)
	}
	if state.InterruptedRunID != "" || state.InterruptedTurnID != "" || state.LastRunStatus != RunStatusRunning {
		t.Fatalf("state after new run = %#v, want old Continue marker consumed", state)
	}
	if _, err := store.StartTurn(session.ID, cancelled.ID, "turn-cancelled", 0, time.Now()); err != nil {
		t.Fatalf("StartTurn(cancelled) error = %v", err)
	}
	state, err = store.SetRunStatus(session.ID, cancelled.ID, RunStatusCancelled, time.Now())
	if err != nil {
		t.Fatalf("SetRunStatus(cancelled) error = %v", err)
	}
	if state.LastRunStatus != RunStatusCancelled || state.InterruptedRunID != "" || state.InterruptedTurnID != "" {
		t.Fatalf("cancelled state = %#v, want no Continue marker", state)
	}
	turns, err = store.ListTurns(session.ID, cancelled.ID)
	if err != nil {
		t.Fatalf("ListTurns(cancelled) error = %v", err)
	}
	if len(turns) != 1 || turns[0].Status != TurnStatusInterrupted {
		t.Fatalf("cancelled turn = %#v, want interrupted terminal turn", turns)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(events) error = %v", err)
	}
	defer db.Close()
	var eventCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM events WHERE type IN (?, ?, ?)", RecordTypeTurnInterrupted, RecordTypeRunSettled, RecordTypeRunStarted).Scan(&eventCount); err != nil {
		t.Fatalf("event count query error = %v", err)
	}
	if eventCount < 6 {
		t.Fatalf("lifecycle event count = %d, want run/turn events for both runs", eventCount)
	}
}

func TestSQLiteEventAndStateSequenceCommitAtomically(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	first := SessionItemFromMessage("item-1", model.Message{Role: model.MessageRoleUser, Content: "one"})
	if _, err := store.AppendItem(session.ID, first); err != nil {
		t.Fatalf("first AppendItem() error = %v", err)
	}
	state, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.LastSeq != 1 {
		t.Fatalf("LastSeq after first append = %d, want 1", state.LastSeq)
	}

	// The duplicate primary key fails after the transaction has inserted its
	// begin marker. Both that marker and any state update must be rolled back.
	duplicate := SessionItemFromMessage(first.ID, model.Message{Role: model.MessageRoleUser, Content: "duplicate"})
	if _, err := store.AppendItemsAndReplaceActiveHistory(session.ID, []SessionItem{duplicate}, []string{first.ID}); err == nil {
		t.Fatal("duplicate append error = nil, want rollback")
	}
	state, err = store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() after rollback error = %v", err)
	}
	if state.LastSeq != 1 {
		t.Fatalf("LastSeq after rollback = %d, want 1", state.LastSeq)
	}
	execution, err := store.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("LoadExecutionState() after rollback error = %v", err)
	}
	if len(execution.Items) != 1 || execution.Items[0].ID != first.ID {
		t.Fatalf("items after rollback = %#v, want original item only", execution.Items)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	var eventCount, itemCount, lastSeq int
	if err := db.QueryRow("SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatalf("event count error = %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM items").Scan(&itemCount); err != nil {
		t.Fatalf("item count error = %v", err)
	}
	if err := db.QueryRow("SELECT last_seq FROM state WHERE singleton = 1").Scan(&lastSeq); err != nil {
		t.Fatalf("state sequence error = %v", err)
	}
	if eventCount != 1 || itemCount != 1 || lastSeq != 1 {
		t.Fatalf("database after rollback: events=%d items=%d last_seq=%d, want 1/1/1", eventCount, itemCount, lastSeq)
	}
}

func TestReadHistoryPageSQLCursorsAndTurnAlignment(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	turns := []string{"turn-1", "turn-1", "turn-2", "turn-2", "turn-3"}
	for i, turnID := range turns {
		item := SessionItemFromMessage("item-"+string(rune('1'+i)), model.Message{
			Role:    model.MessageRoleUser,
			Content: "message-" + string(rune('1'+i)),
		})
		item.TurnID = turnID
		if _, err := store.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", i, err)
		}
	}

	latest, err := store.ReadHistoryPage(session.ID, HistoryPageOptions{Limit: 2, AlignTurn: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage(latest) error = %v", err)
	}
	if got := historyIDs(latest.Items); len(got) != 3 || got[0] != "item-3" || got[1] != "item-4" || got[2] != "item-5" {
		t.Fatalf("aligned latest ids = %#v, want item-3,item-4,item-5", got)
	}
	if latest.OldestSeq != 3 || latest.NewestSeq != 5 || !latest.HasMoreBefore || latest.HasMoreAfter {
		t.Fatalf("latest page cursors = %#v, want oldest=3 newest=5 more-before only", latest)
	}

	before, err := store.ReadHistoryPage(session.ID, HistoryPageOptions{BeforeSeq: latest.OldestSeq, Limit: 1, AlignTurn: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage(before) error = %v", err)
	}
	if got := historyIDs(before.Items); len(got) != 2 || got[0] != "item-1" || got[1] != "item-2" {
		t.Fatalf("aligned before ids = %#v, want item-1,item-2", got)
	}
	if before.HasMoreBefore || !before.HasMoreAfter {
		t.Fatalf("before page flags = %#v, want after only", before)
	}

	after, err := store.ReadHistoryPage(session.ID, HistoryPageOptions{AfterSeq: before.NewestSeq, Limit: 2, AlignTurn: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage(after) error = %v", err)
	}
	if got := historyIDs(after.Items); len(got) != 2 || got[0] != "item-3" || got[1] != "item-4" {
		t.Fatalf("after ids = %#v, want item-3,item-4", got)
	}
	if !after.HasMoreBefore || !after.HasMoreAfter {
		t.Fatalf("after page flags = %#v, want both directions", after)
	}
}

func TestReadHistoryPageVisibleFilterAndBounds(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	items := []struct {
		id         string
		visibility string
	}{
		{"visible-1", ItemVisibilityVisible},
		{"hidden-2", ItemVisibilityHidden},
		{"visible-3", ItemVisibilityVisible},
		{"hidden-4", ItemVisibilityHidden},
		{"visible-5", ItemVisibilityVisible},
	}
	for _, input := range items {
		item := SessionItemFromMessage(input.id, model.Message{Role: model.MessageRoleUser, Content: input.id})
		item.Visibility = input.visibility
		if _, err := store.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%s) error = %v", input.id, err)
		}
	}
	latest, err := store.ReadHistoryPage(session.ID, HistoryPageOptions{Limit: 2, VisibleOnly: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage(latest visible) error = %v", err)
	}
	if got := historyIDs(latest.Items); len(got) != 2 || got[0] != "visible-3" || got[1] != "visible-5" {
		t.Fatalf("latest visible ids = %#v, want visible-3,visible-5", got)
	}
	if latest.OldestSeq != 3 || latest.NewestSeq != 5 || !latest.HasMoreBefore || latest.HasMoreAfter {
		t.Fatalf("latest visible bounds = %#v, want seq 3..5 and before only", latest)
	}

	before, err := store.ReadHistoryPage(session.ID, HistoryPageOptions{BeforeSeq: latest.NewestSeq, Limit: 1, VisibleOnly: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage(before visible) error = %v", err)
	}
	if got := historyIDs(before.Items); len(got) != 1 || got[0] != "visible-3" || !before.HasMoreBefore || !before.HasMoreAfter {
		t.Fatalf("before visible page = %#v, want visible-3 with both directions", before)
	}

	after, err := store.ReadHistoryPage(session.ID, HistoryPageOptions{AfterSeq: 1, Limit: 1, VisibleOnly: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage(after visible) error = %v", err)
	}
	if got := historyIDs(after.Items); len(got) != 1 || got[0] != "visible-3" || !after.HasMoreBefore || !after.HasMoreAfter {
		t.Fatalf("after visible page = %#v, want visible-3 with both directions", after)
	}
}

func TestMarkRunningTurnsInterruptedPreservesInterruptedTurn(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	if _, err := store.MarkTurnRunning(session.ID, "turn-stale"); err != nil {
		t.Fatalf("MarkTurnRunning() error = %v", err)
	}
	marked, err := store.MarkRunningTurnsInterrupted()
	if err != nil {
		t.Fatalf("MarkRunningTurnsInterrupted() error = %v", err)
	}
	if len(marked) != 1 || marked[0].InterruptedTurnID != "turn-stale" {
		t.Fatalf("marked = %#v, want interrupted stale turn", marked)
	}
	state, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.RunningTurnID != "" || !state.RunningStartedAt.IsZero() {
		t.Fatalf("running state after recovery = %#v, want cleared", state)
	}
	if state.InterruptedTurnID != "turn-stale" || state.InterruptedAt.IsZero() {
		t.Fatalf("interrupted state after recovery = %#v, want preserved", state)
	}
	if _, err := store.MarkTurnRunning(session.ID, "turn-new"); err != nil {
		t.Fatalf("new MarkTurnRunning() error = %v", err)
	}
	state, err = store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() after new run error = %v", err)
	}
	if state.RunningTurnID != "turn-new" || state.InterruptedTurnID != "turn-stale" {
		t.Fatalf("state after new run = %#v, want new running plus stale interrupted", state)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(events) error = %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT seq, type FROM events ORDER BY seq")
	if err != nil {
		t.Fatalf("lifecycle event query error = %v", err)
	}
	defer rows.Close()
	wantTypes := []string{RecordTypeTurnRunning, RecordTypeTurnInterrupted, RecordTypeTurnRunning}
	for i, wantType := range wantTypes {
		var seq int64
		var eventType string
		if !rows.Next() {
			t.Fatalf("lifecycle event %d missing, want %q", i, wantType)
		}
		if err := rows.Scan(&seq, &eventType); err != nil {
			t.Fatalf("lifecycle event %d scan error = %v", i, err)
		}
		if seq != int64(i+1) || eventType != wantType {
			t.Fatalf("lifecycle event %d = seq %d type %q, want seq %d type %q", i, seq, eventType, i+1, wantType)
		}
	}
	if rows.Next() {
		t.Fatal("lifecycle event log has an unexpected extra event")
	}
}

func historyIDs(items []SessionItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func TestSQLiteSessionDBLayout(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	root := store.root
	if _, err := os.Stat(filepath.Join(root, session.ID, "session.db")); err != nil {
		t.Fatalf("session.db stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, session.ID, "blobs")); err != nil {
		t.Fatalf("blobs stat error = %v", err)
	}
}

func TestSQLiteStoreUsesRollbackJournalAndReadSetupDoesNotCreateSchema(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		db.Close()
		t.Fatalf("journal_mode error = %v", err)
	}
	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		db.Close()
		t.Fatalf("synchronous error = %v", err)
	}
	db.Close()
	if strings.ToLower(journalMode) != "delete" || synchronous != 2 {
		t.Fatalf("database setup = journal=%q synchronous=%d, want delete/FULL", journalMode, synchronous)
	}
	readDB, err := store.openSessionDB(session.ID, false)
	if err != nil {
		t.Fatalf("openSessionDB(read) error = %v", err)
	}
	var foreignKeys, busyTimeout, readSynchronous int
	if err := readDB.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		readDB.Close()
		t.Fatalf("foreign_keys error = %v", err)
	}
	if err := readDB.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		readDB.Close()
		t.Fatalf("busy_timeout error = %v", err)
	}
	if err := readDB.QueryRow("PRAGMA synchronous").Scan(&readSynchronous); err != nil {
		readDB.Close()
		t.Fatalf("read synchronous error = %v", err)
	}
	readDB.Close()
	if foreignKeys != 1 || busyTimeout != 5000 || readSynchronous != 2 {
		t.Fatalf("read connection setup = foreign_keys=%d busy_timeout=%d synchronous=%d, want 1/5000/2", foreignKeys, busyTimeout, readSynchronous)
	}

	// A read open must not repair a database by creating the rest of the
	// schema. Keep the state table so LoadState can still do its one SELECT.
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove session database error = %v", err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(manual) error = %v", err)
	}
	stateJSON, err := json.Marshal(session)
	if err != nil {
		db.Close()
		t.Fatalf("json.Marshal(session) error = %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE state (singleton INTEGER PRIMARY KEY, session_id TEXT NOT NULL, state_json BLOB NOT NULL, last_seq INTEGER NOT NULL, metadata_version INTEGER NOT NULL DEFAULT 0)`); err != nil {
		db.Close()
		t.Fatalf("create manual state table error = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO state(singleton, session_id, state_json, last_seq) VALUES(1, ?, ?, 0)`, session.ID, stateJSON); err != nil {
		db.Close()
		t.Fatalf("insert manual state error = %v", err)
	}
	db.Close()
	loaded, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(manual state) error = %v", err)
	}
	if loaded.ID != session.ID {
		t.Fatalf("LoadState(manual state) = %#v", loaded)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(check) error = %v", err)
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('events', 'items')`)
	if err != nil {
		db.Close()
		t.Fatalf("schema check error = %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			db.Close()
			t.Fatalf("schema check scan error = %v", err)
		}
		db.Close()
		t.Fatalf("LoadState created unexpected table %q", name)
	}
	db.Close()
}

func TestSQLiteConcurrentWritersAllocateUniqueSequences(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	const writerCount = 12
	start := make(chan struct{})
	errs := make(chan error, writerCount)
	var wg sync.WaitGroup
	for i := 0; i < writerCount; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for attempt := 0; attempt < 20; attempt++ {
				_, err := store.AppendItem(session.ID, SessionItemFromMessage(
					"concurrent-"+string(rune('a'+i)),
					model.Message{Role: model.MessageRoleUser, Content: "concurrent"},
				))
				if err == nil {
					return
				}
				if !strings.Contains(err.Error(), "stale cached session state") {
					errs <- err
					return
				}
			}
			errs <- errors.New("writer did not make progress after stale-state retries")
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append error = %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT seq FROM items ORDER BY seq")
	if err != nil {
		t.Fatalf("item seq query error = %v", err)
	}
	defer rows.Close()
	seqs := make([]int64, 0, writerCount)
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("item seq scan error = %v", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("item seq rows error = %v", err)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if len(seqs) != writerCount {
		t.Fatalf("persisted item count = %d, want %d", len(seqs), writerCount)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("item seq[%d] = %d, want %d", i, seq, i+1)
		}
	}
}

func TestSQLiteMetadataMutationCannotLoseLifecycleUpdate(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	stale, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	stale.DisplayName = "renamed"
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := store.MarkTurnRunning(session.ID, "turn-1")
		results <- err
	}()
	go func() {
		<-start
		_, err := store.SaveMetadata(stale)
		results <- err
	}()
	close(start)
	var firstErr error
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// One operation may legitimately lose the optimistic race, but the
	// durable winner must not erase the lifecycle mutation.
	final, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(final) error = %v", err)
	}
	if final.RunningTurnID != "turn-1" {
		t.Fatalf("final RunningTurnID = %q, want turn-1 (first error=%v)", final.RunningTurnID, firstErr)
	}
	if final.DisplayName != "renamed" && firstErr == nil {
		t.Fatalf("final DisplayName = %q, want metadata winner when both operations succeeded", final.DisplayName)
	}
}

func TestSQLiteStaleHydratedAppendCannotClobberMetadata(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	stale, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(stale) error = %v", err)
	}
	fresh, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(fresh) error = %v", err)
	}
	fresh.DisplayName = "newer name"
	if _, err := store.SaveMetadata(fresh); err != nil {
		t.Fatalf("SaveMetadata(fresh) error = %v", err)
	}
	_, err = store.AppendItemsAndReplaceActiveHistoryFromState(session.ID, stale, []SessionItem{
		SessionItemFromMessage("must-not-append", model.Message{Role: model.MessageRoleUser, Content: "stale"}),
	}, []string{"must-not-append"})
	if err == nil || !strings.Contains(err.Error(), "stale cached session state") {
		t.Fatalf("stale append error = %v, want metadata-version conflict", err)
	}
	final, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(final) error = %v", err)
	}
	if final.DisplayName != "newer name" || final.LastSeq != 0 {
		t.Fatalf("final compact state = %#v, want newer metadata and unchanged seq", final)
	}
	execution, err := store.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("LoadExecutionState(final) error = %v", err)
	}
	if len(execution.Items) != 0 {
		t.Fatalf("stale append persisted %d items, want none", len(execution.Items))
	}
}

func TestSQLiteProjectionAndStateCorruptionAreObservable(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	if _, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{
		Role:    model.MessageRoleUser,
		Content: "persisted",
	})); err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec("UPDATE items SET payload = ?", "not-json"); err != nil {
		db.Close()
		t.Fatalf("corrupt item projection error = %v", err)
	}
	db.Close()
	if _, err := store.LoadExecutionState(session.ID); !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("LoadExecutionState() error = %v, want ErrCorruptedSession", err)
	}
	if _, err := store.LoadState(session.ID); err != nil {
		t.Fatalf("LoadState() after item corruption error = %v, want compact state to remain readable", err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(state) error = %v", err)
	}
	if _, err := db.Exec("UPDATE state SET state_json = ?", "not-json"); err != nil {
		db.Close()
		t.Fatalf("corrupt state error = %v", err)
	}
	db.Close()
	if _, err := store.LoadState(session.ID); !errors.Is(err, ErrCorruptedSession) {
		t.Fatalf("LoadState() error = %v, want ErrCorruptedSession", err)
	}
}

func TestSQLiteItemUpdatePreservesImmutableProjectionFields(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	original, err := store.AppendItem(session.ID, SessionItem{
		ID:         "tool-1",
		TurnID:     "turn-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityVisible,
		Audience:   ItemAudienceModel,
		Status:     ItemStatusPending,
		Message:    &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1"},
	})
	if err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	updated, err := store.UpdateItem(session.ID, SessionItem{
		ID:      original.ID,
		Status:  ItemStatusCompleted,
		Message: &model.Message{Role: model.MessageRoleTool, ToolCallID: "call-1", Content: "done"},
	})
	if err != nil {
		t.Fatalf("UpdateItem() error = %v", err)
	}
	if updated.Seq != original.Seq || updated.TurnID != original.TurnID || updated.Kind != original.Kind || updated.Visibility != original.Visibility || updated.Audience != original.Audience {
		t.Fatalf("updated immutable fields = %#v, want original sequence and routing metadata", updated)
	}
	if updated.Status != ItemStatusCompleted || updated.Message == nil || updated.Message.Content != "done" {
		t.Fatalf("updated item = %#v, want completed tool result", updated)
	}
	loaded, err := store.LoadExecutionState(session.ID)
	if err != nil {
		t.Fatalf("LoadExecutionState() error = %v", err)
	}
	if len(loaded.Items) != 1 || loaded.Items[0].Status != ItemStatusCompleted || loaded.LastSeq != original.Seq+1 {
		t.Fatalf("loaded updated state = %#v, want one updated item and next seq", loaded)
	}
}

func TestSQLiteCompactionAndLargeContentRemainMaterializable(t *testing.T) {
	store, session, _ := testSQLiteSession(t)
	content := strings.Repeat("large content ", 500) + "SECRET"
	item, err := store.AppendItem(session.ID, SessionItemFromMessage("item-1", model.Message{
		Role:    model.MessageRoleUser,
		Content: content,
	}))
	if err != nil {
		t.Fatalf("AppendItem() error = %v", err)
	}
	if item.Message == nil || item.Message.Content != "" || item.Content == nil || item.Content.Blob == nil {
		t.Fatalf("large item = %#v, want blob-backed content", item)
	}
	if _, err := store.ReplaceActiveHistory(session.ID, []string{item.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	summary := SessionItem{
		ID:         "summary-1",
		Kind:       ItemKindMessage,
		Visibility: ItemVisibilityHidden,
		Audience:   ItemAudienceModel,
		Message:    &model.Message{Role: model.MessageRoleDeveloper, Content: "summary"},
	}
	checkpoint := CompactionCheckpoint{
		ID:                    "compact-1",
		SummaryItemID:         summary.ID,
		PreviousActiveHistory: []string{item.ID},
		ReplacementHistory:    []string{summary.ID},
	}
	next, err := store.AppendCompactionCheckpoint(session.ID, summary, checkpoint)
	if err != nil {
		t.Fatalf("AppendCompactionCheckpoint() error = %v", err)
	}
	if len(next.Items) != 2 || len(next.Compactions) != 1 || next.ActiveHistory[0] != summary.ID {
		t.Fatalf("compacted state = %#v, want item plus checkpoint and replacement history", next)
	}
	materialized, err := store.MaterializeActiveHistory(SessionV2{ID: session.ID, Items: next.Items, ActiveHistory: []string{item.ID}})
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v", err)
	}
	if len(materialized) != 1 || materialized[0].Content != content {
		t.Fatalf("materialized large content length=%d, want %d", len(materialized[0].Content), len(content))
	}
}

func TestMaterializeActiveHistoryReadsOnlyActiveItems(t *testing.T) {
	store, session, dbPath := testSQLiteSession(t)
	inactive, err := store.AppendItem(session.ID, SessionItemFromMessage("inactive", model.Message{
		Role:    model.MessageRoleUser,
		Content: "not active",
	}))
	if err != nil {
		t.Fatalf("AppendItem(inactive) error = %v", err)
	}
	active, err := store.AppendItem(session.ID, SessionItemFromMessage("active", model.Message{
		Role:    model.MessageRoleUser,
		Content: "active",
	}))
	if err != nil {
		t.Fatalf("AppendItem(active) error = %v", err)
	}
	if _, err := store.ReplaceActiveHistory(session.ID, []string{active.ID}); err != nil {
		t.Fatalf("ReplaceActiveHistory() error = %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec("UPDATE items SET payload = ? WHERE id = ?", "not-json", inactive.ID); err != nil {
		db.Close()
		t.Fatalf("corrupt inactive item error = %v", err)
	}
	db.Close()
	compact, err := store.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	messages, err := store.MaterializeActiveHistory(compact)
	if err != nil {
		t.Fatalf("MaterializeActiveHistory() error = %v, want inactive corruption ignored", err)
	}
	if len(messages) != 1 || messages[0].Content != "active" {
		t.Fatalf("materialized active history = %#v, want active item only", messages)
	}
}
