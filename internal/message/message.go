// Package message 定义与 A2A 1.0 对齐的消息模型（wire 层）。
//
// 语义约定（迁移自 Rust 版并保持等价）：
//   - Part 是 oneof：text / raw / url / data **恰好一个**。解析严格，
//     零个或多个都报错，且错误里列出实际出现的字段。
//   - 普通字段为 null 等同于「未设置」（ProtoJSON 语义）。
//   - data 对应 google.protobuf.Value，null 是合法值 → 必须区分
//     「字段缺失」与「data: null」。
//   - raw 在 JSON 中是 base64；解码同时接受标准/URL-safe 字母表与省略 padding。
//   - Role 序列化为 ROLE_USER / ROLE_AGENT（ProtoJSON 成员名），
//     反序列化兼容小写别名，拒绝 ROLE_UNSPECIFIED 等未知值。
//   - 字段名 camelCase（messageId / contextId / mediaType …），与 A2A 一致。
package message

import (
	"encoding/base64"
	jsontext "encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"strings"
)

// Metadata 是任意 JSON 元数据（A2A 的 metadata）。
type Metadata map[string]any

// Role 标识消息发送方。
type Role string

const (
	RoleUser  Role = "ROLE_USER"
	RoleAgent Role = "ROLE_AGENT"
)

// MarshalJSONTo 按 ProtoJSON 成员名输出。
//
// **写出去的东西必须能读回来**：空值/未知值直接报错，而不是写出一个
// 自己都解析不了的值（否则一条坏消息就能让整个日志无法加载）。
func (r Role) MarshalJSONTo(enc *jsontext.Encoder) error {
	switch r {
	case RoleUser, RoleAgent:
		return enc.WriteToken(jsontext.String(string(r)))
	default:
		return fmt.Errorf("不能序列化非法 role：%q", string(r))
	}
}

// UnmarshalJSONFrom 兼容小写别名，拒绝未知值。
func (r *Role) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '"' {
		return fmt.Errorf("role 必须是字符串，实际是 %v", tok.Kind())
	}
	switch tok.String() {
	case string(RoleUser), "user":
		*r = RoleUser
	case string(RoleAgent), "agent":
		*r = RoleAgent
	default:
		return fmt.Errorf("不支持的 role：%q（可选 ROLE_USER / ROLE_AGENT）", tok.String())
	}
	return nil
}

// PartContent 是 Part 的内容（oneof）。只有本包内类型能实现。
type PartContent interface {
	isPartContent()
}

// TextPart 是纯文本。
type TextPart struct{ Text string }

// RawPart 是内嵌字节（JSON 中为 base64）。
type RawPart struct{ Raw []byte }

// URLPart 是指向文件内容的 URL。
type URLPart struct{ URL string }

// DataPart 是任意结构化 JSON（可为 nil，表示 JSON null）。
type DataPart struct{ Data any }

func (TextPart) isPartContent() {}
func (RawPart) isPartContent()  {}
func (URLPart) isPartContent()  {}
func (DataPart) isPartContent() {}

// Part 是消息的最小内容单元。
type Part struct {
	Content   PartContent
	MediaType string
	Filename  string
	Metadata  Metadata
}

// NewTextPart 构造文本 part。
func NewTextPart(text string) Part { return Part{Content: TextPart{Text: text}} }

// NewDataPart 构造数据 part。
func NewDataPart(data any) Part { return Part{Content: DataPart{Data: data}} }

// NewRawPart 构造原始字节 part。
func NewRawPart(raw []byte) Part { return Part{Content: RawPart{Raw: raw}} }

// NewURLPart 构造 URL part。
func NewURLPart(url string) Part { return Part{Content: URLPart{URL: url}} }

// WithMediaType 设置 MIME 类型。
func (p Part) WithMediaType(mediaType string) Part {
	p.MediaType = mediaType
	return p
}

// WithFilename 设置文件名。
func (p Part) WithFilename(name string) Part {
	p.Filename = name
	return p
}

// AsText 返回文本内容。
func (p Part) AsText() (string, bool) {
	t, ok := p.Content.(TextPart)
	return t.Text, ok
}

// AsData 返回数据内容。
func (p Part) AsData() (any, bool) {
	d, ok := p.Content.(DataPart)
	return d.Data, ok
}

// AsURL 返回 URL 内容。
func (p Part) AsURL() (string, bool) {
	u, ok := p.Content.(URLPart)
	return u.URL, ok
}

// AsRaw 返回原始字节。
func (p Part) AsRaw() ([]byte, bool) {
	r, ok := p.Content.(RawPart)
	return r.Raw, ok
}

// PartKind 是内容类型标签（供渲染层分发）。
type PartKind string

const (
	PartKindText PartKind = "text"
	PartKindRaw  PartKind = "raw"
	PartKindURL  PartKind = "url"
	PartKindData PartKind = "data"
)

// Kind 返回内容类型标签。
func (p Part) Kind() PartKind {
	switch p.Content.(type) {
	case TextPart:
		return PartKindText
	case RawPart:
		return PartKindRaw
	case URLPart:
		return PartKindURL
	case DataPart:
		return PartKindData
	default:
		return ""
	}
}

// partWire 是 Part 的平铺表示，用于严格 oneof 校验。
//
// 未知字段**默认忽略**（与 serde/protobuf 解析器的默认行为一致）：
// A2A 0.3 风格的载荷会带 `kind` 判别字段，日志也需要向前兼容——
// 以后给 Part 加字段不能让旧版本读不了新日志。
type partWire struct {
	Text      *string       `json:"text,omitempty"`
	Raw       *string       `json:"raw,omitempty"`
	URL       *string       `json:"url,omitempty"`
	Data      optionalValue `json:"data,omitempty"`
	MediaType *string       `json:"mediaType,omitempty"`
	Filename  *string       `json:"filename,omitempty"`
	Metadata  Metadata      `json:"metadata,omitempty"`
}

// optionalValue 记录字段是否出现，以及原始 JSON（含 null）。
//
// 不能用 *jsontext.Value：JSON null 会把它解成 nil 指针，丢失「字段出现过」这个信息，
// 而 data 对应 google.protobuf.Value，null 是合法值。
type optionalValue struct {
	present bool
	raw     jsontext.Value
}

func (o *optionalValue) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	o.present = true
	o.raw = raw
	return nil
}

// UnmarshalJSONFrom 严格解析 Part：text/raw/url/data 恰好取其一。
//
// 用 json.UnmarshalDecode 而不是「ReadValue + Unmarshal」：这样调用方的
// Options（如 RejectUnknownMembers）会通过 Decoder.Options() 继续传给嵌套字段。
func (p *Part) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var wire partWire
	if err := json.UnmarshalDecode(dec, &wire); err != nil {
		return err
	}

	// data 对应 google.protobuf.Value：null 也是「有值」。
	// *jsontext.Value 能区分「字段缺失」（nil）与「data: null」（指向 "null"）。

	present := make([]string, 0, 4)
	if wire.Text != nil {
		present = append(present, "text")
	}
	if wire.Raw != nil {
		present = append(present, "raw")
	}
	if wire.URL != nil {
		present = append(present, "url")
	}
	if wire.Data.present {
		present = append(present, "data")
	}

	switch {
	case wire.Text != nil && len(present) == 1:
		p.Content = TextPart{Text: *wire.Text}
	case wire.Raw != nil && len(present) == 1:
		raw, err := decodeBase64Lenient(*wire.Raw)
		if err != nil {
			return fmt.Errorf("raw 需要 base64：%w", err)
		}
		p.Content = RawPart{Raw: raw}
	case wire.URL != nil && len(present) == 1:
		p.Content = URLPart{URL: *wire.URL}
	case wire.Data.present && len(present) == 1:
		if string(wire.Data.raw) == "null" {
			p.Content = DataPart{Data: nil}
			break
		}
		var data any
		if err := json.Unmarshal(wire.Data.raw, &data); err != nil {
			return err
		}
		p.Content = DataPart{Data: data}
	case len(present) == 0:
		return fmt.Errorf("Part 必须包含 text/raw/url/data 之一，实际：[]（null 字段视为未设置）")
	default:
		return fmt.Errorf("Part 必须包含 text/raw/url/data 中恰好一个，实际：[%s]", strings.Join(present, ", "))
	}

	if wire.MediaType != nil {
		p.MediaType = *wire.MediaType
	}
	if wire.Filename != nil {
		p.Filename = *wire.Filename
	}
	p.Metadata = wire.Metadata
	return nil
}

// MarshalJSONTo 把 Part 序列化为 A2A 的平铺形式。
func (p Part) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	writeField := func(name string, value any) error {
		if err := enc.WriteToken(jsontext.String(name)); err != nil {
			return err
		}
		return json.MarshalEncode(enc, value)
	}

	switch c := p.Content.(type) {
	case TextPart:
		if err := writeField("text", c.Text); err != nil {
			return err
		}
	case RawPart:
		if err := writeField("raw", base64.StdEncoding.EncodeToString(c.Raw)); err != nil {
			return err
		}
	case URLPart:
		if err := writeField("url", c.URL); err != nil {
			return err
		}
	case DataPart:
		// data 即使是 nil 也要写出 null（它是合法值，与「字段缺失」不同）
		if err := enc.WriteToken(jsontext.String("data")); err != nil {
			return err
		}
		if err := json.MarshalEncode(enc, c.Data); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Part 没有内容")
	}

	if p.MediaType != "" {
		if err := writeField("mediaType", p.MediaType); err != nil {
			return err
		}
	}
	if p.Filename != "" {
		if err := writeField("filename", p.Filename); err != nil {
			return err
		}
	}
	if len(p.Metadata) > 0 {
		if err := writeField("metadata", p.Metadata); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// decodeBase64Lenient 同时接受标准与 URL-safe 字母表，padding 可有可无。
func decodeBase64Lenient(s string) ([]byte, error) {
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.URLEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// Message 是一次通信回合（A2A Message）。
type Message struct {
	MessageID        string   `json:"messageId"`
	ContextID        string   `json:"contextId,omitempty"`
	TaskID           string   `json:"taskId,omitempty"`
	Role             Role     `json:"role"`
	Parts            []Part   `json:"parts"`
	Metadata         Metadata `json:"metadata,omitempty"`
	Extensions       []string `json:"extensions,omitempty"`
	ReferenceTaskIDs []string `json:"referenceTaskIds,omitempty"`
}

// PreviewText 拼接所有文本 part。**有损**：仅用于标题/列表预览，
// 非文本 part 会被跳过；需要完整内容请遍历 Parts。
func (m Message) PreviewText() string {
	parts := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		if text, ok := p.AsText(); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// UnmarshalJSONFrom 要求 messageId 非空；未知字段的处理交给调用方的 Options
// （默认忽略，UnmarshalStrict 时拒绝，且该选项会通过 UnmarshalDecode 传到 Part 层）。
func (m *Message) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	// alias 避开递归调用本方法
	type alias Message
	var a alias
	if err := json.UnmarshalDecode(dec, &a); err != nil {
		return err
	}
	if a.MessageID == "" {
		return fmt.Errorf("messageId 必填（规范 §4.1.4）")
	}
	*m = Message(a)
	return nil
}

// MarshalJSONTo 序列化 Message，并对必填字段做对称校验。
func (m Message) MarshalJSONTo(enc *jsontext.Encoder) error {
	if m.MessageID == "" {
		return fmt.Errorf("messageId 不能为空")
	}
	if m.Role != RoleUser && m.Role != RoleAgent {
		return fmt.Errorf("不能序列化非法 role：%q", string(m.Role))
	}
	type alias Message
	return json.MarshalEncode(enc, alias(m))
}

// Marshal 序列化为 A2A JSON。
func (m Message) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// Unmarshal 从 A2A JSON 解析（宽松：未知字段忽略；语义仍严格：oneof、
// messageId、role）。日志与网络数据都走这里。
func Unmarshal(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// UnmarshalStrict 额外拒绝未知字段（测试/诊断用，避免字段名拼写错误）。
//
// 选项经 json.UnmarshalDecode 传到嵌套的 Part，因此 Part 层的未知字段同样被拒绝。
func UnmarshalStrict(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m, json.RejectUnknownMembers(true)); err != nil {
		return Message{}, err
	}
	return m, nil
}
