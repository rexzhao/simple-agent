package message_test

import (
	"encoding/json"
	"simple-agent/internal/message"
	"strings"
	"testing"
)

func TestPartStrictOneOf(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string // 空表示应成功
	}{
		{"text", `{"text":"hi"}`, ""},
		{"raw", `{"raw":"3q2+7w=="}`, ""},
		{"url", `{"url":"https://x.test/a.png"}`, ""},
		{"data object", `{"data":{"k":1}}`, ""},
		{"data null 是合法值", `{"data":null}`, ""},
		{"text + data 同时出现", `{"text":"x","data":{"k":1}}`, "恰好一个"},
		{"零个内容字段", `{"mediaType":"text/plain"}`, "实际：[]"},
		{"空对象", `{}`, "实际：[]"},
		{"text 类型错误", `{"text":123}`, "cannot unmarshal"},
		{"raw 非法 base64", `{"raw":"!!!"}`, "base64"},
		{"未知字段默认忽略（向前兼容）", `{"text":"x","kind":"text"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p message.Part
			err := json.Unmarshal([]byte(tc.input), &p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望成功，实际错误：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望错误包含 %q，实际成功（content=%#v）", tc.wantErr, p.Content)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应包含 %q，实际：%v", tc.wantErr, err)
			}
		})
	}
}

// null 的普通字段视为未设置（ProtoJSON 语义）。
func TestPartNullFieldsTreatedAsAbsent(t *testing.T) {
	var p message.Part
	if err := json.Unmarshal([]byte(`{"text":null,"url":"https://x.test/a"}`), &p); err != nil {
		t.Fatalf("null text 应视为未设置：%v", err)
	}
	if got, _ := p.AsURL(); got != "https://x.test/a" {
		t.Fatalf("应解析为 url part，实际 %#v", p.Content)
	}

	// 只有 null 字段 → 视为没有内容
	var empty message.Part
	err := json.Unmarshal([]byte(`{"text":null}`), &empty)
	if err == nil || !strings.Contains(err.Error(), "实际：[]") {
		t.Fatalf("只有 null 字段应报错并提示 null 语义，实际：%v", err)
	}
}

func TestPartBase64Lenient(t *testing.T) {
	// 0xDE 0xAD 0xBE 0xEF 的四种合法写法都应解码成功
	for _, encoded := range []string{"3q2+7w==", "3q2+7w", "3q2-7w==", "3q2-7w"} {
		var p message.Part
		if err := json.Unmarshal([]byte(`{"raw":"`+encoded+`"}`), &p); err != nil {
			t.Fatalf("%s 应可解码：%v", encoded, err)
		}
		raw, ok := p.AsRaw()
		if !ok || len(raw) != 4 || raw[0] != 0xDE || raw[3] != 0xEF {
			t.Fatalf("%s 解码结果错误：%#v", encoded, raw)
		}
	}

	// 输出统一为标准字母表 + padding
	out, err := json.Marshal(message.NewRawPart([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"raw":"3q2+7w=="}` {
		t.Fatalf("序列化应使用标准 base64，实际：%s", out)
	}
}

func TestPartDataNullRoundTrip(t *testing.T) {
	var p message.Part
	if err := json.Unmarshal([]byte(`{"data":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind() != message.PartKindData {
		t.Fatalf("data:null 应是 data part，实际 %v", p.Kind())
	}
	if data, ok := p.AsData(); !ok || data != nil {
		t.Fatalf("data 应为 nil，实际 %#v", data)
	}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"data":null}` {
		t.Fatalf("roundtrip 应保持 data:null，实际：%s", out)
	}
}

func TestRoleSerialization(t *testing.T) {
	out, err := json.Marshal(message.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"ROLE_USER"` {
		t.Fatalf("应序列化为 ROLE_USER，实际：%s", out)
	}

	for _, in := range []string{`"ROLE_AGENT"`, `"agent"`} {
		var r message.Role
		if err := json.Unmarshal([]byte(in), &r); err != nil {
			t.Fatalf("%s 应可解析：%v", in, err)
		}
		if r != message.RoleAgent {
			t.Fatalf("%s 应解析为 ROLE_AGENT，实际 %v", in, r)
		}
	}

	var r message.Role
	err = json.Unmarshal([]byte(`"ROLE_UNSPECIFIED"`), &r)
	if err == nil || !strings.Contains(err.Error(), "不支持的 role") {
		t.Fatalf("ROLE_UNSPECIFIED 应被拒绝，实际：%v", err)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	msg := message.Message{
		MessageID: "m-1",
		ContextID: "ctx-1",
		Role:      message.RoleUser,
		Parts: []message.Part{
			message.NewTextPart("帮我分析这张图"),
			message.NewURLPart("https://example.com/a.png").WithMediaType("image/png"),
			message.NewDataPart(map[string]any{"depth": "deep"}).WithMediaType("application/json"),
			message.NewRawPart([]byte{1, 2, 3}).WithFilename("blob.bin"),
		},
		Metadata:   message.Metadata{"origin": "test"},
		Extensions: []string{"https://example.com/ext/v1"},
	}

	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// A2A 字段名
	for _, want := range []string{`"messageId":"m-1"`, `"contextId":"ctx-1"`, `"mediaType":"image/png"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("序列化应包含 %s，实际：%s", want, data)
		}
	}

	got, err := message.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID != msg.MessageID || got.Role != msg.Role || len(got.Parts) != len(msg.Parts) {
		t.Fatalf("roundtrip 不一致：%#v", got)
	}
	if text, _ := got.Parts[0].AsText(); text != "帮我分析这张图" {
		t.Fatalf("文本 part 不一致：%q", text)
	}
	if got.Metadata["origin"] != "test" {
		t.Fatalf("metadata 不一致：%#v", got.Metadata)
	}
}

func TestUnmarshalRequiresMessageID(t *testing.T) {
	_, err := message.Unmarshal([]byte(`{"role":"ROLE_USER","parts":[]}`))
	if err == nil {
		t.Fatal("缺少 messageId 应报错（严格模式）")
	}
}

// 规范风格的输入（小写 role、mediaType、data 嵌套）应能解析。
func TestParseSpecStyleJSON(t *testing.T) {
	msg, err := message.Unmarshal([]byte(`{
		"messageId": "m-spec",
		"role": "ROLE_USER",
		"parts": [{"text":"Find restaurants near me","mediaType":"text/plain","filename":"q.txt"}],
		"metadata": {"https://example.com/ext/v1": {"latitude": 37.7749}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Parts[0].MediaType != "text/plain" || msg.Parts[0].Filename != "q.txt" {
		t.Fatalf("共享字段未正确读入：%#v", msg.Parts[0])
	}
	if msg.PreviewText() != "Find restaurants near me" {
		t.Fatalf("PreviewText 不一致：%q", msg.PreviewText())
	}
}

// A2A 0.3 风格的载荷带 kind 判别字段：默认入口应接受（未知字段忽略）。
// 严格入口（UnmarshalStrict）才拒绝，用于测试/诊断字段名拼写。
func TestUnknownFieldsPermissiveByDefault(t *testing.T) {
	payload := `{
		"messageId": "m-1",
		"kind": "message",
		"role": "ROLE_USER",
		"parts": [{"text":"hi","kind":"text"}]
	}`

	msg, err := message.Unmarshal([]byte(payload))
	if err != nil {
		t.Fatalf("默认入口应接受带 kind 的载荷：%v", err)
	}
	if text, _ := msg.Parts[0].AsText(); text != "hi" {
		t.Fatalf("文本不一致：%q", text)
	}

	if _, err := message.UnmarshalStrict([]byte(payload)); err == nil {
		t.Fatal("UnmarshalStrict 应拒绝未知字段")
	}

	// oneof 冲突仍然严格（不受宽松模式影响）
	var p message.Part
	if err := json.Unmarshal([]byte(`{"text":"x","data":1}`), &p); err == nil {
		t.Fatal("oneof 冲突必须报错")
	}
}

// 回归（opus 探针）：写出去的东西必须能严格读回来。
func TestMarshalUnmarshalSymmetric(t *testing.T) {
	// 空 role / 空 messageId 在序列化时就应报错，而不是写出后读不回来
	if _, err := (message.Message{MessageID: "m1"}).Marshal(); err == nil {
		t.Fatal("空 role 应序列化失败")
	}
	if _, err := (message.Message{Role: message.RoleUser}).Marshal(); err == nil {
		t.Fatal("空 messageId 应序列化失败")
	}

	valid := []message.Message{
		{MessageID: "m1", Role: message.RoleUser, Parts: []message.Part{message.NewTextPart("hi")}},
		{MessageID: "m2", Role: message.RoleAgent},
		{MessageID: "m3", ContextID: "c", TaskID: "t", Role: message.RoleAgent, Parts: []message.Part{message.NewDataPart(nil)}},
		{MessageID: "m4", Role: message.RoleUser, Parts: []message.Part{message.NewRawPart([]byte{1, 2}), message.NewURLPart("https://x.test/a")}},
	}
	for _, msg := range valid {
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("合法消息应能序列化：%v", err)
		}
		if _, err := message.Unmarshal(data); err != nil {
			t.Fatalf("序列化结果应能读回：%v（JSON：%s）", err, data)
		}
	}
}

func TestPreviewTextJoinsAndSkipsNonText(t *testing.T) {
	msg := message.Message{Parts: []message.Part{
		message.NewTextPart("第一段"),
		message.NewURLPart("https://x.test/a.png"),
		message.NewTextPart("第二段"),
	}}
	if got := msg.PreviewText(); got != "第一段\n第二段" {
		t.Fatalf("PreviewText 应有损拼接，实际：%q", got)
	}
}

// 回归（opus）：严格模式必须覆盖嵌套的 Part 层，只查顶层是不够的。
func TestStrictModeCoversNestedParts(t *testing.T) {
	payload := `{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi","kind":"text"}]}`

	// 默认宽松：接受（向前兼容）
	if _, err := message.Unmarshal([]byte(payload)); err != nil {
		t.Fatalf("默认入口应接受 Part 层未知字段：%v", err)
	}
	// 严格：Part 层的未知字段也要拒绝（选项经 UnmarshalDecode 传播）
	if _, err := message.UnmarshalStrict([]byte(payload)); err == nil {
		t.Fatal("严格模式应拒绝 Part 层的未知字段")
	}
	// 顶层未知字段同样拒绝
	if _, err := message.UnmarshalStrict([]byte(`{"messageId":"m1","role":"ROLE_USER","parts":[],"extra":1}`)); err == nil {
		t.Fatal("严格模式应拒绝顶层未知字段")
	}
}
