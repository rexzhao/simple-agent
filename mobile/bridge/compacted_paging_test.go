package bridge_test

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/wire"
	"simple-agent/mobile/bridge"
)

// 压缩过的长会话：手机一路往上翻，必须能看到**整段对话**，每条恰好一次。
//
// 真机回归（"还没到会话开头就说已经是最早的内容了"）。磁盘上的形态：
//
//	archive-0001.jsonl  压缩前的整份日志（旧问题 1..N）
//	session.jsonl       seq S 是压缩快照（kept = 旧问题 N-1..N），之后是新问题
//
// 以前有两段手机永远看不到：
//   - 快照里的 kept 消息：backfill 只发内容事件、不发快照，归档又按 id 把它们排除了；
//   - 归档：Remote.LoadOlder 读的是镜像的本地日志（镜像没有日志），永远返回空页。
func TestScrollToTopReachesWholeCompactedSession(t *testing.T) {
	const (
		oldRounds  = 30
		keptRounds = 2
		newRounds  = 150 // 300 条事件，超过 attach 的 200 条尾巴：必须走 backfill
	)
	dataDir := t.TempDir()
	sessionID := "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
	dir := filepath.Join(dataDir, "sessions", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgID := func(kind string, n int) string { return fmt.Sprintf("%s%028d", kind, n) }
	textMsg := func(id string, role message.Role, text string) message.Message {
		return message.Message{MessageID: id, Role: role, Parts: []message.Part{{Content: message.TextPart{Text: text}}}}
	}
	encode := func(seq uint64, kind string, data any) string {
		t.Helper()
		event, err := store.NewEvent(kind, data)
		if err != nil {
			t.Fatal(err)
		}
		event.Version = store.Version
		event.Seq = seq
		event.At = at.Add(time.Duration(seq) * time.Second)
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		return string(line) + "\n"
	}
	round := func(lines *strings.Builder, seq *uint64, roundNo int, prefix string) {
		user := textMsg(msgID("aaaa", roundNo), message.RoleUser, fmt.Sprintf("%s问题-%03d", prefix, roundNo))
		agent := textMsg(msgID("bbbb", roundNo), message.RoleAgent, fmt.Sprintf("%s回答-%03d", prefix, roundNo))
		lines.WriteString(encode(*seq, store.TypeMessageInput, driver.MessageEventData{
			Round: roundNo, Turn: 1, Role: "user", Message: user, Status: conversation.StatusComplete,
		}))
		*seq++
		lines.WriteString(encode(*seq, store.TypeMessageOutput, driver.MessageEventData{
			Round: roundNo, Turn: 1, Role: "agent", Message: agent, Status: conversation.StatusComplete,
		}))
		*seq++
	}

	// 归档：压缩前的整份日志
	var archive strings.Builder
	var seq uint64
	archive.WriteString(encode(seq, store.TypeSessionCreated, map[string]any{}))
	seq++
	for n := 1; n <= oldRounds; n++ {
		round(&archive, &seq, n, "旧")
	}
	// 当前日志：快照（保留最后 keptRounds 轮）+ 新内容
	var kept []driver.KeptEntry
	for n := oldRounds - keptRounds + 1; n <= oldRounds; n++ {
		kept = append(kept,
			driver.KeptEntry{Message: textMsg(msgID("aaaa", n), message.RoleUser, fmt.Sprintf("旧问题-%03d", n)), Status: conversation.StatusComplete},
			driver.KeptEntry{Message: textMsg(msgID("bbbb", n), message.RoleAgent, fmt.Sprintf("旧回答-%03d", n)), Status: conversation.StatusComplete},
		)
	}
	var current strings.Builder
	// archived_as 必须带上：store 打开时会把"快照没记录过的归档"当孤儿删掉
	snapshot := map[string]any{}
	raw, err := json.Marshal(driver.CompactSnapshotData{
		Summary: "摘要", SummaryID: msgID("cccc", 0), Kept: kept,
		Selection: providers.Selection{ProviderID: "fake", Model: "m"}, KeptFromSeq: seq,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot["archived_as"] = 1
	current.WriteString(encode(seq, store.TypeCompactSnapshot, snapshot))
	seq++
	for n := 1; n <= newRounds; n++ {
		round(&current, &seq, 1000+n, "新")
	}
	if err := os.WriteFile(filepath.Join(dir, "archive-0001.jsonl"), []byte(archive.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(current.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	local := host.NewLocal(host.Config{
		Runtime:       &fakeRuntime{},
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     dataDir,
		MaxToolRounds: 4,
		ContextWindow: 200_000,
		Catalog:       store.NewCatalog(dataDir),
		Post:          func(fn func()) { fn() },
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Session(sessionID) == nil {
		t.Fatal("没有恢复出会话")
	}

	poster := &serialPoster{}
	viewerConn, execConn := wire.PipePair()
	go func() { _ = host.Serve(execConn, local, poster.post) }()
	remote, err := host.NewRemote(viewerConn, poster.post)
	if err != nil {
		t.Fatal(err)
	}
	client := bridge.NewClient(&listener{}, "zh", t.TempDir())
	t.Cleanup(client.Close)
	bridge.AttachForTest(client, remote)

	// 阻塞调用（要等执行端回包）放后台跑，这边持续 pump 投递回包
	call := func(fn func() string) string {
		t.Helper()
		done := make(chan string, 1)
		go func() { done <- fn() }()
		var out string
		waitFor(t, poster, func() bool {
			select {
			case out = <-done:
				return true
			default:
				return false
			}
		})
		return out
	}
	waitFor(t, poster, func() bool {
		return strings.Contains(client.TimelineJSON(sessionID), fmt.Sprintf("新问题-%03d", 1000+newRounds))
	})

	// 1) 时间线：从末页一路往前翻到 has_more=false
	// 收集每个条目/归档消息的 JSON：按"多少条含有这个标记"计数
	// （markdown 条目的 text 与 blocks 里都有原文，直接数子串会翻倍）
	var timelineItems, archiveEntries []string
	cursor := int64(0)
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("翻页超过 50 次还没到头：游标没有推进")
		}
		body := call(func() string { return client.TimelinePageJSON(sessionID, cursor, 60) })
		var page pageView
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			raw, _ := json.Marshal(item)
			timelineItems = append(timelineItems, string(raw))
		}
		if !page.HasMore {
			break
		}
		if len(page.Items) == 0 {
			continue // retry：同一游标再来
		}
		cursor = page.SeqFrom
	}

	// 2) 时间线到头之后翻归档
	archiveCursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("归档翻页没有结束")
		}
		body := call(func() string { return client.LoadOlder(sessionID, archiveCursor) })
		var page bridge.ArchiveView
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		if page.Error != "" {
			t.Fatalf("加载归档失败：%s", page.Error)
		}
		for _, entry := range page.Entries {
			raw, _ := json.Marshal(entry)
			archiveEntries = append(archiveEntries, string(raw))
		}
		if !page.More || page.Next == "" {
			break
		}
		archiveCursor = page.Next
	}

	// 每个问题在"时间线 + 归档"里恰好出现一次，且出现在正确的那一边
	countIn := func(list []string, marker string) int {
		n := 0
		for _, text := range list {
			if strings.Contains(text, marker) {
				n++
			}
		}
		return n
	}
	check := func(marker string, wantTimeline bool) {
		t.Helper()
		inTimeline, inArchive := countIn(timelineItems, marker), countIn(archiveEntries, marker)
		if inTimeline+inArchive != 1 {
			t.Errorf("%s 应恰好出现一次：时间线 %d 次，归档 %d 次", marker, inTimeline, inArchive)
			return
		}
		if wantTimeline != (inTimeline == 1) {
			t.Errorf("%s 出现在了错误的位置（时间线=%d 归档=%d）", marker, inTimeline, inArchive)
		}
	}
	for n := 1; n <= oldRounds; n++ {
		// kept 的那几轮属于当前会话（来自快照），其余在归档里
		check(fmt.Sprintf("旧问题-%03d", n), n > oldRounds-keptRounds)
	}
	for n := 1; n <= newRounds; n++ {
		check(fmt.Sprintf("新问题-%03d", 1000+n), true)
	}
}
