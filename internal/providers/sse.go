package providers

import (
	"bufio"
	"io"
	"strings"
)

// SSEEvent 是一条 SSE 事件的数据体（已去掉 `data:` 前缀）。
type SSEEvent struct {
	// Event 是 `event:` 字段（Anthropic 会带，OpenAI 不带）。
	Event string
	// Data 是 `data:` 行拼接后的内容（多行用 \n 连接）。
	Data string
}

// SSEReader 逐条读取 SSE 事件。
//
// 规范细节：
//   - CRLF / LF 都接受；
//   - `data:` 后的单个空格是分隔符，要去掉；
//   - 多行 `data:` 用 \n 连接；
//   - 以 `:` 开头的注释行（心跳）忽略；
//   - 流在没有结尾空行时中断，仍把已收到的数据交出去。
type SSEReader struct {
	scanner *bufio.Scanner
}

// NewSSEReader 构造读取器。
func NewSSEReader(r io.Reader) *SSEReader {
	scanner := bufio.NewScanner(r)
	// 单行上限放宽（长 JSON chunk）
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &SSEReader{scanner: scanner}
}

// Next 读取下一条事件；流结束返回 io.EOF。
func (r *SSEReader) Next() (SSEEvent, error) {
	var event SSEEvent
	var data []string

	for {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return SSEEvent{}, err
			}
			// 流结束：交出未以空行结尾的残余事件
			if joined := strings.Join(data, "\n"); joined != "" {
				event.Data = joined
				return event, nil
			}
			return SSEEvent{}, io.EOF
		}

		line := strings.TrimRight(r.scanner.Text(), "\r")
		if line == "" {
			// 事件分隔符：无 data 或 data 全为空则跳过（心跳）
			if joined := strings.Join(data, "\n"); joined != "" {
				event.Data = joined
				return event, nil
			}
			event.Event = ""
			data = data[:0]
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue // 注释/心跳
		}

		switch {
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			chunk = strings.TrimPrefix(chunk, " ")
			data = append(data, chunk)
		case strings.HasPrefix(line, "event:"):
			event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			// event/id/retry 之外的字段忽略
		}
	}
}
