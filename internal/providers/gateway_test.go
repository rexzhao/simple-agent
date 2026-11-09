package providers_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"simple-agent/internal/message"
	"simple-agent/internal/providers"
)

// sseServer 起一个回放固定 SSE 内容的网关。
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// drain 收集事件直到 Done/Error 或 channel 关闭。
func drain(t *testing.T, events <-chan providers.Event) []providers.Event {
	t.Helper()
	var out []providers.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Kind == providers.EventDone || ev.Kind == providers.EventError {
				return out
			}
		case <-timeout:
			t.Fatal("等待事件超时")
		}
	}
}

func openaiConfig(url string, includeUsage bool) providers.ProviderConfig {
	return providers.ProviderConfig{
		ID: "test", Label: "Test", Kind: providers.KindOpenAI,
		BaseURL: url, Models: []providers.ModelConfig{{ID: "m"}}, IncludeUsage: includeUsage,
	}
}

// OpenAI 兼容协议：文本增量 + finish_reason + usage（在 finish_reason 之后单独一块）+ [DONE]。
func TestOpenAIStreamEndToEnd(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你好"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"，世界"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		// usage 单独一块（choices 为空）
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	srv := sseServer(t, body)
	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, true), "key")

	events, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	collected := drain(t, events)
	var text strings.Builder
	var usage *providers.Event
	var done *providers.Event
	for i := range collected {
		ev := collected[i]
		switch ev.Kind {
		case providers.EventText:
			text.WriteString(ev.Text)
		case providers.EventUsage:
			usage = &collected[i]
		case providers.EventDone:
			done = &collected[i]
		}
	}
	if text.String() != "你好，世界" {
		t.Fatalf("文本不一致：%q", text.String())
	}
	if usage == nil || usage.InputTokens != 7 || usage.OutputTokens != 9 {
		t.Fatalf("usage 应在 finish_reason 之后被收到：%#v", usage)
	}
	if done == nil || done.StopReason != "stop" {
		t.Fatalf("Done 应带 stop_reason：%#v", done)
	}
}

// include_usage=false 时不能干等 [DONE]：看到 finish_reason 就应结束。
func TestOpenAIStreamEndsWithoutDoneMarker(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"嗨\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	srv := sseServer(t, body)
	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, false), "key")

	events, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	collected := drain(t, events)
	if last := collected[len(collected)-1]; last.Kind != providers.EventDone {
		t.Fatalf("最后应为 Done，实际 %v", last.Kind)
	}
}

// 工具调用：分片参数累积由上层负责，这里断言事件形状。
func TestOpenAIStreamToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
	}, "\n")

	srv := sseServer(t, body)
	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, false), "key")
	events, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	collected := drain(t, events)
	var start *providers.Event
	var args strings.Builder
	for i := range collected {
		switch collected[i].Kind {
		case providers.EventToolStart:
			start = &collected[i]
		case providers.EventToolArgs:
			args.WriteString(collected[i].Delta)
		}
	}
	if start == nil || start.ID != "call_1" || start.Name != "read" || start.Index != 0 {
		t.Fatalf("工具开始事件不一致：%#v", start)
	}
	if args.String() != `{"path":"a.txt"}` {
		t.Fatalf("参数分片应能拼成合法 JSON，实际 %q", args.String())
	}
}

// Anthropic：文本块也会发 content_block_stop，不能当工具结束。
func TestAnthropicStreamTextBlockStopIsNotToolEnd(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":15}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
	}, "\n")

	srv := sseServer(t, body)
	cfg := providers.ProviderConfig{
		ID: "a", Label: "A", Kind: providers.KindAnthropic,
		BaseURL: srv.URL, Models: []providers.ModelConfig{{ID: "m"}},
	}
	p := providers.NewAnthropicProvider(cfg, "key")
	events, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	collected := drain(t, events)
	for _, ev := range collected {
		if ev.Kind == providers.EventToolEnd {
			t.Fatalf("文本块的 stop 不应产生 ToolEnd：%#v", ev)
		}
	}
	var text string
	for _, ev := range collected {
		if ev.Kind == providers.EventText {
			text += ev.Text
		}
	}
	if text != "你好" {
		t.Fatalf("文本不一致：%q", text)
	}
}

// Anthropic 工具调用：content_block_stop 只对 tool_use 报结束。
func TestAnthropicStreamToolUse(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
	}, "\n")

	srv := sseServer(t, body)
	cfg := providers.ProviderConfig{
		ID: "a", Label: "A", Kind: providers.KindAnthropic,
		BaseURL: srv.URL, Models: []providers.ModelConfig{{ID: "m"}},
	}
	p := providers.NewAnthropicProvider(cfg, "key")
	events, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	collected := drain(t, events)
	var sawStart, sawEnd bool
	var args strings.Builder
	for _, ev := range collected {
		switch ev.Kind {
		case providers.EventToolStart:
			sawStart = ev.ID == "toolu_1" && ev.Name == "read"
		case providers.EventToolArgs:
			args.WriteString(ev.Delta)
		case providers.EventToolEnd:
			sawEnd = ev.Index == 1
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("tool_use 的开始/结束事件缺失：%#v", collected)
	}
	if args.String() != `{"path":"a.txt"}` {
		t.Fatalf("参数分片不一致：%q", args.String())
	}
}

// 非 2xx：错误通过返回值报告（含状态码与服务端错误体）。
func TestHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, true), "bad")
	_, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var perr *providers.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("应为 *providers.Error，实际 %T", err)
	}
	if perr.Status != 401 {
		t.Fatalf("状态码应为 401，实际 %d", perr.Status)
	}
	if !strings.Contains(perr.Message, "invalid api key") {
		t.Fatalf("错误体应透传，实际 %q", perr.Message)
	}
}

// 取消：ctx 取消后 channel 应尽快关闭，不再产出事件。
func TestCancelStopsStream(t *testing.T) {
	// 服务端持续输出，直到客户端断开
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			if _, err := io.WriteString(w, fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", i)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := providers.NewOpenAIProvider(openaiConfig(srv.URL, false), "key")
	events, err := p.Stream(ctx, providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 收到第一条后取消
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("未收到首条事件")
	}
	cancel()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // channel 已关闭
			}
		case <-deadline:
			t.Fatal("取消后 channel 未关闭")
		}
	}
}

// 模拟提供商走同一套事件接口。
func TestSimulatedProviderStream(t *testing.T) {
	p := providers.NewSimulatedProvider()
	events, err := p.Stream(context.Background(), providers.ChatRequest{Model: "none"})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for ev := range events {
		if ev.Kind == providers.EventText {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != providers.SimulatedNotice() {
		t.Fatalf("模拟输出应与提示一致，实际 %q", text.String())
	}
	if p.Config().Kind != providers.KindSimulated {
		t.Fatalf("Kind 应为 simulated，实际 %v", p.Config().Kind)
	}
}

// 注册表：内置列表、凭据过滤、初始选择。
func TestRegistryBasics(t *testing.T) {
	r := providers.NewRegistry()
	if len(r.All()) == 0 {
		t.Fatal("应有内置提供商")
	}
	// ollama 无 Key 也算凭据齐备；模拟提供商不能出现在选择器里
	for _, p := range r.Configured() {
		if p.Config.Kind == providers.KindSimulated {
			t.Fatal("模拟提供商不应出现在 Configured 列表")
		}
	}
	if _, err := r.Build(providers.SimulatedSelection()); err != nil {
		t.Fatalf("模拟 provider 应能构造：%v", err)
	}
	if _, err := r.Build(providers.Selection{ProviderID: "nope", Model: "m"}); err == nil {
		t.Fatal("未知 provider 应报错")
	}
}

// 没有环境变量凭据时不自动选本地服务：否则用户看到的是连接失败，而不是「请配置凭据」。
func TestRegistryInitialRequiresCredentials(t *testing.T) {
	for _, key := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "MOONSHOT_API_KEY", "ZHIPU_API_KEY", "DASHSCOPE_API_KEY"} {
		t.Setenv(key, "")
	}
	r := providers.NewRegistry()
	if sel, ok := r.Initial(); ok {
		t.Fatalf("没有凭据时不应自动选中提供商，实际 %#v", sel)
	}
	// 用户已确认：无 Key 时走模拟提示，不自动选 Ollama（Rust 版会选）
	if sel, ok := r.Initial(); ok && sel.ProviderID == "ollama" {
		t.Fatal("无 Key 时不应自动选中 ollama（用户已确认新行为）")
	}

	// 显式配置 active_provider 时才允许用本地服务
	r.ActiveProvider = "ollama"
	if sel, ok := r.Initial(); !ok || sel.ProviderID != "ollama" {
		t.Fatalf("配置了 active_provider=ollama 时应选中它，实际 %#v ok=%v", sel, ok)
	}

	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	r = providers.NewRegistry()
	sel, ok := r.Initial()
	if !ok || sel.ProviderID != "deepseek" {
		t.Fatalf("有凭据时应选中 deepseek，实际 %#v ok=%v", sel, ok)
	}
}

// 编译期：适配器的 Config 与 Kind 一致（防止注册表与实现漂移）。
func TestProviderKindsMatch(t *testing.T) {
	cfg := openaiConfig("http://localhost", true)
	if got := providers.NewOpenAIProvider(cfg, "").Config().Kind; got != providers.KindOpenAI {
		t.Fatalf("OpenAI 适配器 Kind 不一致：%v", got)
	}
	acfg := providers.ProviderConfig{ID: "a", Kind: providers.KindAnthropic, BaseURL: "http://localhost"}
	if got := providers.NewAnthropicProvider(acfg, "").Config().Kind; got != providers.KindAnthropic {
		t.Fatalf("Anthropic 适配器 Kind 不一致：%v", got)
	}
}

// 请求体形状：system 位置、tools 字段名、max_tokens 必填。
func TestRequestBodyShapes(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	temp := 0.3
	req := providers.ChatRequest{
		Model:       "m",
		System:      "你是助手",
		Messages:    []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
		Tools:       []providers.ToolSpec{{Name: "read", Description: "读文件", Parameters: map[string]any{"type": "object"}}},
		Temperature: &temp,
	}

	// OpenAI：system 作为第一条消息
	openaiProvider := providers.NewOpenAIProvider(openaiConfig(srv.URL, true), "k")
	events, err := openaiProvider.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	body := string(captured)
	for _, want := range []string{`"role":"system"`, `"tools":`, `"parameters":`, `"temperature":0.3`, `"include_usage":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("OpenAI 请求体应包含 %s，实际 %s", want, body)
		}
	}

	// Anthropic：system 是顶层字段、tools 用 input_schema、max_tokens 必填
	acfg := providers.ProviderConfig{ID: "a", Kind: providers.KindAnthropic, BaseURL: srv.URL}
	anthropicProvider := providers.NewAnthropicProvider(acfg, "k")
	events, err = anthropicProvider.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	body = string(captured)
	for _, want := range []string{`"system":"你是助手"`, `"input_schema":`, `"max_tokens":4096`} {
		if !strings.Contains(body, want) {
			t.Fatalf("Anthropic 请求体应包含 %s，实际 %s", want, body)
		}
	}
}

// 工具结果配对边界：压缩后的历史至少首条为 user（供 M6 复用）。
func TestHistoryStartsWithUser(t *testing.T) {
	history := []message.Message{
		{MessageID: "a0", Role: message.RoleAgent, Parts: []message.Part{message.NewTextPart("摘要")}},
		{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}},
	}
	out, err := providers.ToAnthropicMessages(history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 2 || out[1]["role"] != "user" {
		t.Fatalf("user 消息应存在：%#v", out)
	}
}

func asProviderError(err error, target **providers.Error) bool {
	if pe, ok := err.(*providers.Error); ok {
		*target = pe
		return true
	}
	return false
}

// 模型级参数覆盖厂商级参数，其它模型不受影响。
func TestModelExtraBodyOverrides(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := openaiConfig(srv.URL, false)
	cfg.ExtraBody = map[string]any{"reasoning_effort": "low"}
	cfg.Models = []providers.ModelConfig{
		{ID: "m", ExtraBody: map[string]any{"reasoning_effort": "medium"}},
		{ID: "other"},
	}
	p := providers.NewOpenAIProvider(cfg, "k")
	events, err := p.Stream(context.Background(), providers.ChatRequest{
		Model:    "m",
		Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	if len(bodies) != 1 {
		t.Fatalf("应发出 1 个请求，实际 %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"reasoning_effort":"medium"`) {
		t.Fatalf("模型级参数应覆盖厂商级：%s", bodies[0])
	}

	// 换一个没有 model_extra 的模型 → 用厂商级
	events, err = p.Stream(context.Background(), providers.ChatRequest{
		Model:    "other",
		Messages: []message.Message{{MessageID: "u2", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, events)
	if !strings.Contains(bodies[1], `"reasoning_effort":"low"`) {
		t.Fatalf("没有模型级参数时应退回厂商级：%s", bodies[1])
	}
}

// 请求里的档位覆盖模型配置：普通值写入、off 删除、空串沿用配置。
func TestReasoningEffortOverride(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := openaiConfig(srv.URL, false)
	cfg.Models = []providers.ModelConfig{{
		ID: "m", ExtraBody: map[string]any{"reasoning_effort": "medium"},
	}}
	p := providers.NewOpenAIProvider(cfg, "k")

	send := func(effort string) string {
		events, err := p.Stream(context.Background(), providers.ChatRequest{
			Model: "m", ReasoningEffort: effort,
			Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, events)
		return bodies[len(bodies)-1]
	}

	if body := send(""); !strings.Contains(body, `"reasoning_effort":"medium"`) {
		t.Fatalf("空串应沿用配置：%s", body)
	}
	if body := send("high"); !strings.Contains(body, `"reasoning_effort":"high"`) {
		t.Fatalf("应使用请求里的档位：%s", body)
	}
	if body := send("off"); strings.Contains(body, "reasoning_effort") {
		t.Fatalf("off 时不应带该参数：%s", body)
	}
}

// Anthropic 不认 reasoning_effort（未知顶层字段会 400）：请求体里绝不能出现它，
// 即使请求里带了档位（思考请走 extra_body 的 thinking）。
func TestAnthropicNeverSendsReasoningEffort(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	cfg := providers.ProviderConfig{
		ID: "a", Label: "A", Kind: providers.KindAnthropic,
		BaseURL: srv.URL, Models: []providers.ModelConfig{{ID: "m"}},
	}
	p := providers.NewAnthropicProvider(cfg, "key")

	for _, effort := range []string{"", "low", "high", "off"} {
		events, err := p.Stream(context.Background(), providers.ChatRequest{
			Model: "m", ReasoningEffort: effort,
			Messages: []message.Message{{MessageID: "u1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, events)
		body := bodies[len(bodies)-1]
		if strings.Contains(body, "reasoning_effort") {
			t.Fatalf("档位 %q：Anthropic 请求体里不应出现 reasoning_effort：%s", effort, body)
		}
		if strings.Contains(body, "thinking") {
			t.Fatalf("档位 %q：不应自动生成 thinking 字段：%s", effort, body)
		}
	}
}
