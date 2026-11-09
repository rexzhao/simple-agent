package bridge

import (
	json "encoding/json/v2"

	"simple-agent/internal/message"
)

// partData 取 Data part 的对象视图。
func partData(part message.Part) (map[string]any, bool) {
	dataPart, ok := part.Content.(message.DataPart)
	if !ok {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	return data, ok
}

// stringField 取对象里的字符串字段（不存在或类型不符时返回空）。
func stringField(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

// encodeJSON 把任意值编成 JSON 字节（工具参数的未知类型降级用）。
func encodeJSON(value any) ([]byte, error) { return json.Marshal(value) }
