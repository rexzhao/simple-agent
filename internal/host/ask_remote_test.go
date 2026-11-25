package host_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/driver"
	"simple-agent/internal/host"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/tools"
	"simple-agent/internal/wire"
)

// **远端 viewer 勾选的选项必须真的传到模型**。
//
// 用户报："远端桌面版 viewer ask question 的回答没有传给执行端"。
//
// 现有测试 `TestAskAnsweredElsewhere` 只验证"第二个回答会被拒"，
// 没验证"第一个回答的内容真的落地" —— 而后者才是用户实际遇到的。
//
// 这条覆盖完整链路：
//
//	viewer 勾选 → answer 命令 → 执行端 apply 到 AskGroup
//	→ SubmitAsks → 模型收到的 tool_result 里的 selected 列表
func TestRemoteViewerAnswerReachesModel(t *testing.T) {
	rt := &captureRuntime{}
	p := &poster{}
	local := host.NewLocal(host.Config{
		Runtime: rt, Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt: "你是助手", Workspace: t.TempDir(), MaxToolRounds: 3,
		ContextWindow: 200_000, Post: p.Post,
	})
	t.Cleanup(func() { _ = local.Close() })
	if local.Active() == nil {
		local.NewSession("")
	}
	sessID := local.Active().ID

	// 远端 viewer 连上来
	viewerConn, execConn := wire.PipePair()
	go func() { _ = host.Serve(execConn, local, p.Post) }()
	remote, err := host.NewRemote(viewerConn, p.Post)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	results := make(chan host.CmdResult, 8)
	remote.Subscribe(func(change host.Change) {
		if change.Kind == "cmdResult" {
			results <- change.Result
		}
	})

	// 发一条消息，触发 ask_user_question
	payload, _ := host.EncodePayload(host.SendPayload{Text: "问我"})
	if _, err := remote.Submit(host.Command{SessionID: sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	live := waitStream(t, rt, p, 1)

	// 模型调用 ask_user_question：一道多选题
	live <- providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "ask_user_question"}
	live <- providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"questions":[{"id":"q1","question":"选哪些？","multiSelect":true,"options":[{"label":"A"},{"label":"B"},{"label":"C"}]}]}`}
	live <- providers.Event{Kind: providers.EventToolEnd, Index: 0}
	live <- providers.Event{Kind: providers.EventDone, StopReason: "tool_use"}

	waitAwaitingAnswer(t, local, sessID, p)

	// **勾选 A(idx 0) 和 C(idx 2)** —— 不选第一个、也不全选，
	// 这样才能区分"没传"和"传了默认值"。
	answer, _ := host.EncodePayload(host.AnswerPayload{
		Answers: []host.AskAnswer{{
			CallID:   "c1",
			Selected: map[string][]int{"q1": {0, 2}},
			Custom:   map[string]string{},
		}},
	})
	id, err := remote.Submit(host.Command{SessionID: sessID, Kind: "answer", Payload: answer})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitResult(t, results, id, p); !result.OK {
		t.Fatalf("回答没有被接受：%+v", result)
	}

	// **关键断言：模型收到的 selected 里必须有 A 和 C**
	//
	// 注意 selected 里存的是**选项标签**（"A"/"C"），不是索引 ——
	// driver 的 AskGroup.answers() 把下标翻译成 label 再交给模型。
	// 等答案落地后模型被再次调用（第 2 个请求带上了 tool_result）
	req := waitRequest(t, rt, p, 2)
	selected := selectedForQuestion(req, "q1")
	if len(selected) != 2 || selected[0] != "A" || selected[1] != "C" {
		t.Fatalf("模型收到的选项不对：%v（期望 [A C]）", selected)
	}
}

// waitAwaitingAnswer 等会话进入"等待回答"状态。
func waitAwaitingAnswer(t *testing.T, local *host.Local, sessionID string, p *poster) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if session := local.Session(sessionID); session != nil && session.Busy() == driver.BusyAwaitingAnswer {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	busy := driver.BusyIdle
	if session := local.Session(sessionID); session != nil {
		busy = session.Busy()
	}
	t.Fatalf("没有进入等待回答：%v", busy)
}

// waitResult 等某条命令的执行结果。
func waitResult(t *testing.T, results <-chan host.CmdResult, cmdID string, p *poster) host.CmdResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		select {
		case result := <-results:
			if result.CmdID == cmdID {
				return result
			}
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("命令 %s 没有返回结果", cmdID)
	return host.CmdResult{}
}

// captureRuntime 记录每次发给模型的请求，并保留流以驱动事件。
type captureRuntime struct {
	mu       sync.Mutex
	requests []providers.ChatRequest
	streams  []chan providers.Event
}

func (r *captureRuntime) OpenStream(ctx context.Context, sel providers.Selection, req providers.ChatRequest) (<-chan providers.Event, error) {
	events := make(chan providers.Event, 64)
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.streams = append(r.streams, events)
	r.mu.Unlock()
	return events, nil
}

func (r *captureRuntime) RunTools(ctx context.Context, tc *tools.ToolContext, calls []tools.Call) []tools.ToolOutcome {
	out := make([]tools.ToolOutcome, 0, len(calls))
	for _, call := range calls {
		if call.Name == "ask_user_question" {
			// **必须返回 Pending**：提问内容从这里进入 driver，
			// 返回普通 "ok" 的话会话永远停在 streaming（真实踩到）。
			out = append(out, tools.ToolOutcome{
				ID: call.ID, Name: "ask_user_question",
				Pending: []tools.QuestionSpec{{
					ID: "q1", Question: "选哪些？", MultiSelect: true,
					Options: []tools.QuestionOption{{Label: "A"}, {Label: "B"}, {Label: "C"}},
				}},
			})
			continue
		}
		out = append(out, tools.ToolOutcome{ID: call.ID, Name: call.Name, Output: "ok"})
	}
	return out
}

func (r *captureRuntime) stream(n int) chan providers.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 1 || n > len(r.streams) {
		return nil
	}
	return r.streams[n-1]
}

func (r *captureRuntime) request(n int) (providers.ChatRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 1 || n > len(r.requests) {
		return providers.ChatRequest{}, false
	}
	return r.requests[n-1], true
}

func (r *captureRuntime) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

// waitStream 等第 n 个流出现。
func waitStream(t *testing.T, rt *captureRuntime, p *poster, n int) chan providers.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if rt.count() >= n {
			if stream := rt.stream(n); stream != nil {
				return stream
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("第 %d 个请求没有开始", n)
	return nil
}

// waitRequest 等第 n 个请求被记录。
func waitRequest(t *testing.T, rt *captureRuntime, p *poster, n int) providers.ChatRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.pump()
		if req, ok := rt.request(n); ok {
			return req
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("第 %d 个请求没有发出", n)
	return providers.ChatRequest{}
}

// selectedForQuestion 从请求的 tool_result 里找出某个问题选中的**标签**。
//
// # 工具结果是 DataPart（媒体类型 render.MediaToolResult），不是 TextPart
//
// 早先按 TextPart 解析，结果什么也找不到，测试误报"答案没传到"。
// 答案藏在 `part.Content.(message.DataPart).Data` 里的 `output` 字段
// （JSON 字符串），再解析出来才是 {"answers":[...]}。
func selectedForQuestion(req providers.ChatRequest, questionID string) []string {
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			dataPart, ok := part.Content.(message.DataPart)
			if !ok {
				continue
			}
			text := toolResultOutput(dataPart.Data)
			if text == "" {
				continue
			}
			var parsed struct {
				Answers []struct {
					// 字段名是 id（tools.QuestionAnswer 的 JSON tag）
					ID       string   `json:"id"`
					Selected []string `json:"selected"`
				} `json:"answers"`
			}
			if err := json.Unmarshal([]byte(text), &parsed); err != nil {
				continue
			}
			for _, answer := range parsed.Answers {
				if answer.ID == questionID {
					return answer.Selected
				}
			}
		}
	}
	return nil
}

// toolResultOutput 从工具结果的 data 里取出 output 字符串。
func toolResultOutput(data any) string {
	fields, ok := data.(map[string]any)
	if !ok {
		return ""
	}
	out, _ := fields["output"].(string)
	return out
}