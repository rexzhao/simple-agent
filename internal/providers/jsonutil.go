package providers

import (
	json "encoding/json/v2"
	"strings"
)

// 本文件把 JSON 细节收在一处，便于以后从 v2 切换或调整选项。

// marshalDeterministic 序列化请求体，**键顺序固定**。
//
// 为什么必须固定：OpenAI / Anthropic 的 prompt caching 要求**前缀逐字节一致**。
// 请求体里 messages 全是从 map[string]any 拼出来的，而 `encoding/json/v2`
// 默认按 map 迭代顺序输出键（随机！）—— 同一个会话连续两次请求，
// `{"content":...,"role":...}` 与 `{"role":...,"content":...}` 会交替出现，
// 前缀在第一条消息处就分叉，缓存**永远不可能命中**。
//
// v1 会自动按键名排序，v2 把这个行为去掉了（性能考虑），所以这里显式开启。
// 这个问题很隐蔽：请求内容语义完全相同，provider 只回 cached_tokens=0，
// 看不出任何异常。真机实测 28 万 token 的会话 0% 命中才发现。
func marshalDeterministic(v any) ([]byte, error) {
	return json.Marshal(v, json.Deterministic(true))
}

// marshalCompact 把工具参数等结构序列化成紧凑 JSON。
//
// **必须是确定性的**：这个结果会作为 `tool_calls[].function.arguments`
// 写进**历史消息**，而 prompt caching 要求前缀逐字节一致。
// 用随机键顺序的话，同一条历史消息在每次请求里的字节表示都不同，
// 从它开始往后的全部缓存都失效 —— 真机上表现为 cached_tokens 只有
// 4000 出头（≈ 差异出现之前的开头那一段），命中率 0.6%。
//
// 之前这里用的是裸 json.Marshal（json/v2 默认随机顺序），
// 而同一文件的 marshalDeterministic 只管了请求体顶层，漏了这一处。
func marshalCompact(v any) string {
	data, err := marshalDeterministic(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func unmarshalJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func indexByte(s string, b byte) int { return strings.IndexByte(s, b) }

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), b)
}
