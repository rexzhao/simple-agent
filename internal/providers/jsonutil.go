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

func marshalCompact(v any) string {
	data, err := json.Marshal(v)
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
