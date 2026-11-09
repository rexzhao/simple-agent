package bridge_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
	"simple-agent/mobile/bridge"
)

// askRuntime 让"工具调用"返回一个待回答的提问（模拟模型调 ask_user_question）。
//
// 内嵌 fakeRuntime：OpenStream / RunTools 的通用部分复用，这里只覆盖 RunTools。
type askRuntime struct {
	fakeRuntime
	// asked 保证只让第一次工具调用产生提问，避免无限循环。
	asked bool
}

func (r *askRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		if call.Name == "ask_user_question" && !r.asked {
			r.asked = true
			out = append(out, tools.ToolOutcome{
				ID: call.ID, Name: call.Name, Output: "等我回答",
				Pending: []tools.QuestionSpec{{
					ID: "q1", Question: "选哪个方案？", Header: "方案",
					Options: []tools.QuestionOption{{Label: "方案 A"}, {Label: "方案 B"}},
				}},
			})
			continue
		}
		out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
	}
	return out
}

// newPair 起一个执行端 + 一个 bridge.Client（进程内管道）。
func newPair(t *testing.T, runtime host.Runtime, cfg func(*host.Config)) (*serialPoster, *host.Local, *bridge.Client, string) {
	t.Helper()
	poster := &serialPoster{}
	config := host.Config{
		Runtime:       runtime,
		Selection:     providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:  "你是助手",
		Workspace:     t.TempDir(),
		MaxToolRounds: 4,
		ContextWindow: 200_000,
		Post:          poster.post,
	}
	if cfg != nil {
		cfg(&config)
	}
	local := host.NewLocal(config)
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sessionID := local.Active().ID

	viewerConn, execConn := wire.PipePair()
	go func() { _ = host.Serve(execConn, local, poster.post) }()
	remote, err := host.NewRemote(viewerConn, poster.post)
	if err != nil {
		t.Fatal(err)
	}
	client := bridge.NewClient(&listener{}, "zh", t.TempDir())
	t.Cleanup(client.Close)
	bridge.AttachForTest(client, remote)
	waitFor(t, poster, func() bool {
		var sessions []bridge.SessionSummary
		if err := json.Unmarshal([]byte(client.SessionsJSON()), &sessions); err != nil {
			return false
		}
		return len(sessions) == 1
	})
	return poster, local, client, sessionID
}

// 提问面板：选项能切换（单选互斥）、能写自写答案、提交后执行端收到答案。
func TestAskPanelToggleAndSubmit(t *testing.T) {
	rt := &askRuntime{}
	poster, local, client, sessionID := newPair(t, rt, nil)

	// 发一条会触发工具（并产生提问）的消息
	payload, _ := host.EncodePayload(host.SendPayload{Text: "帮我选个方案"})
	if _, err := local.Submit(host.Command{SessionID: sessionID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	poster.pump()
	stream := waitStream(t, poster, &rt.fakeRuntime, 1)
	// 模型这一轮发起工具调用（assistant 消息带 tool_use）
	stream <- providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "call-1", Name: "ask_user_question"}
	stream <- providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[{"id":"q1","question":"选哪个方案？"}]}`}
	stream <- providers.Event{Kind: providers.EventToolEnd, Index: 0}
	stream <- providers.Event{Kind: providers.EventDone, StopReason: "tool_use"}

	var asks []bridge.AskView
	waitFor(t, poster, func() bool {
		var view bridge.TimelineView
		if err := json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &view); err != nil {
			return false
		}
		asks = view.Asks
		return len(asks) == 1 && len(asks[0].Questions) == 1
	})
	ask := asks[0]
	if len(ask.Questions[0].Options) != 2 {
		t.Fatalf("选项没同步过来：%+v", ask.Questions[0])
	}
	if ask.Questions[0].Answered {
		t.Fatal("还没作答，Answered 不该为真")
	}

	// 选"方案 B"（下标 1）
	_ = client.ToggleAskOption(sessionID, ask.CallID, 0, 1)
	var after []bridge.AskView
	waitFor(t, poster, func() bool {
		var view bridge.TimelineView
		if err := json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &view); err != nil {
			return false
		}
		after = view.Asks
		return len(after) == 1 && len(after[0].Questions[0].Selected) == 1
	})
	if got := after[0].Questions[0].Selected[0]; got != 1 {
		t.Fatalf("应选中下标 1，实际 %d", got)
	}

	// 单选互斥：再选"方案 A"应替换而不是叠加
	_ = client.ToggleAskOption(sessionID, ask.CallID, 0, 0)
	waitFor(t, poster, func() bool {
		var view bridge.TimelineView
		_ = json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &view)
		if len(view.Asks) == 0 {
			return false
		}
		sel := view.Asks[0].Questions[0].Selected
		return len(sel) == 1 && sel[0] == 0
	})

	// 写自写答案：应清掉选项并以文字作答
	_ = client.SetAskCustom(sessionID, ask.CallID, 0, "两个都不好，用 C")
	waitFor(t, poster, func() bool {
		var view bridge.TimelineView
		_ = json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &view)
		if len(view.Asks) == 0 {
			return false
		}
		question := view.Asks[0].Questions[0]
		return len(question.Selected) == 0 && question.CustomActive &&
			question.Custom == "两个都不好，用 C"
	})

	// 提交：执行端接着开下一条流（回合继续）
	if errText := client.SubmitAsks(sessionID); errText != "" {
		t.Fatalf("提交失败：%s", errText)
	}
	poster.pump()
	next := waitStream(t, poster, &rt.fakeRuntime, 2)
	next <- providers.Event{Kind: providers.EventText, Text: "好的，用 C"}
	next <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	waitFor(t, poster, func() bool {
		var view bridge.TimelineView
		_ = json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &view)
		return len(view.Asks) == 0 && view.Runtime.Busy == "idle"
	})
}

// 上传图片：字节 → 执行端 blob，返回的 hash 能再拉回来，且带附件的消息发得出去。
func TestPutBlobAndSendWithAttachment(t *testing.T) {
	dataDir := t.TempDir()
	blobs, err := store.NewBlobs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	poster, local, client, sessionID := newPair(t, rt, func(cfg *host.Config) {
		cfg.Blobs = blobs
		cfg.Catalog = store.NewCatalog(dataDir)
		// 带附件发送的前提：执行端的模型支持图片（driver 会据此拒收）。
		cfg.VisionFor = func(providers.Selection) bool { return true }
	})

	// 一张结构完整的 1x1 PNG（内容寻址：hash 就是它内容的 sha256）
	png := pngBytes(t)
	// PutBlob 是**阻塞**的（等执行端确认落盘），而回包要靠界面线程的 post 队列投递：
	// 所以放后台跑，主线程这边持续 pump。
	hashCh := make(chan string, 1)
	go func() { hashCh <- client.PutBlob(png) }()
	var hash string
	waitFor(t, poster, func() bool {
		select {
		case hash = <-hashCh:
			return true
		default:
			return false
		}
	})
	if hash == "" {
		t.Fatalf("上传失败：%s", client.LastError())
	}
	if !blobs.Has(hash) {
		t.Fatal("执行端 blob 存储里没有这张图")
	}

	// 带附件发送：执行端应该收到引用
	attachments := `[{"hash":"` + hash + `","media_type":"image/png","name":"shot.png"}]`
	if errText := client.SendWithAttachments(sessionID, "看这张图", attachments); errText != "" {
		t.Fatalf("带附件发送失败：%s", errText)
	}
	poster.pump()
	stream := waitStream(t, poster, rt, 1)
	stream <- providers.Event{Kind: providers.EventText, Text: "我看到了"}
	stream <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}

	// 等执行端把带附件的那条用户消息写进会话（post 队列要 pump）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !foundAttachment(local, sessionID, hash) {
		poster.pump()
		time.Sleep(2 * time.Millisecond)
	}
	if !foundAttachment(local, sessionID, hash) {
		session := local.Session(sessionID)
		for _, entry := range session.Conversation().Entries() {
			if entry.Message == nil || entry.Message.Message == nil {
				continue
			}
			t.Logf("entry %s role=%s status=%s parts=%d",
				entry.ID(), entry.Message.Message.Role, entry.Message.Status(),
				len(entry.Message.Message.Parts))
			for _, part := range entry.Message.Message.Parts {
				url, _ := part.AsURL()
				text, _ := part.AsText()
				t.Logf("  part media=%q url=%q text=%.40q", part.MediaType, url, text)
			}
		}
		t.Fatal("执行端会话里没有带 blob 引用的附件")
	}
	// 展示端也能按 hash 把图拉回来（blob.get）：阻塞调用放后台，主线程 pump
	pathCh := make(chan string, 1)
	go func() { pathCh <- client.BlobPath(hash) }()
	var path string
	blobDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(blobDeadline) {
		poster.pump()
		select {
		case path = <-pathCh:
			blobDeadline = time.Time{} // 已拿到，退出循环
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if path == "" {
		t.Fatalf("拉取图片失败：%s", client.LastError())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(png) {
		t.Fatal("拉回来的图片内容不一致")
	}
	if filepath.Base(path) != hash {
		t.Fatalf("缓存文件名应是 hash，实际 %s", filepath.Base(path))
	}
}

// 加载更早归档：能拿到历史页，且不改变当前会话内容。
func TestLoadOlderArchive(t *testing.T) {
	dataDir := t.TempDir()
	rt := &fakeRuntime{}
	poster, local, client, sessionID := newPair(t, rt, func(cfg *host.Config) {
		cfg.Catalog = store.NewCatalog(dataDir)
		cfg.Blobs = mustBlobs(t, dataDir)
	})

	// 先发一条消息，让会话落盘（归档需要真实的 JSONL）
	payload, _ := host.EncodePayload(host.SendPayload{Text: "第一轮"})
	if _, err := local.Submit(host.Command{SessionID: sessionID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	poster.pump()
	stream := waitStream(t, poster, rt, 1)
	stream <- providers.Event{Kind: providers.EventText, Text: "回答一"}
	stream <- providers.Event{Kind: providers.EventDone, StopReason: "stop"}
	waitFor(t, poster, func() bool {
		return local.Session(sessionID).Busy() == "idle"
	})

	// 没有压缩过就没有归档：应当返回空页而不是报错
	var page bridge.ArchiveView
	if errText := json.Unmarshal([]byte(client.LoadOlder(sessionID, "")), &page); errText != nil {
		t.Fatalf("归档回包解析失败：%v", errText)
	}
	if page.Error != "" {
		t.Fatalf("不该报错：%s", page.Error)
	}
	if len(page.Entries) != 0 || page.More {
		t.Fatalf("没有归档时应当是空页：%+v", page)
	}

	// 会话内容不受影响
	var timeline bridge.TimelineView
	if err := json.Unmarshal([]byte(client.TimelineJSON(sessionID)), &timeline); err != nil {
		t.Fatal(err)
	}
	if len(timeline.Items) == 0 {
		t.Fatal("加载归档之后会话内容不该变空")
	}
}

func mustBlobs(t *testing.T, dir string) *store.Blobs {
	t.Helper()
	blobs, err := store.NewBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return blobs
}

// foundAttachment 报告执行端会话里是否已有引用该 hash 的 part。
func foundAttachment(local *host.Local, sessionID, hash string) bool {
	session := local.Session(sessionID)
	if session == nil {
		return false
	}
	for _, entry := range session.Conversation().Entries() {
		if entry.Message == nil || entry.Message.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if url, ok := part.AsURL(); ok && strings.Contains(url, hash) {
				return true
			}
		}
	}
	return false
}

// pngBytes 生成一张最小但**结构完整**的 1x1 PNG。
//
// 注意：不能随手拼几个字节——blob 是内容寻址的，拉回来时会按 sha256 校验，
// 数据本身不合法（例如缺 IDAT）虽然也能存取，但会掩盖真实问题。
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 200, G: 60, B: 60, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
