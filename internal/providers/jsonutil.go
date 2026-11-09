package providers

import (
	json "encoding/json/v2"
	"strings"
)

// 本文件把 JSON 细节收在一处，便于以后从 v2 切换或调整选项。

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
