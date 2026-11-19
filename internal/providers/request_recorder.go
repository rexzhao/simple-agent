package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RequestRecorder 把每次请求的**完整请求体**落盘，用来排查缓存命中率。
//
// 为什么需要它：prompt caching 要求"前缀逐字节一致"。只要请求体里任何一个
// 字节在两次请求之间变了（时间戳、map 顺序、浮点格式、路径写法…），
// 缓存就从那个位置往后全部失效，而 provider 只会告诉你 cached_tokens=0，
// 不会告诉你是哪一段变了。
//
// 用法：把连续两次请求体存下来，逐字节对比前缀，第一处差异就是断点。
// 录的是完整 JSON（可能很大），所以默认只录前 N 次，避免把磁盘写爆。
type RequestRecorder struct {
	mu      sync.Mutex
	dir     string
	limit   int // 最多录几次请求体；0 表示不录
	written int
}

var (
	requestRecorderMu sync.RWMutex
	requestRecorder   *RequestRecorder
)

// NewRequestRecorder 创建记录器，写入 dir/requests/。
//
// limit 是这次运行最多录多少份完整请求体（每份可能几 MB）。
func NewRequestRecorder(dir string, limit int) *RequestRecorder {
	if dir == "" || limit <= 0 {
		return nil
	}
	target := filepath.Join(dir, "requests")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil
	}
	return &RequestRecorder{dir: target, limit: limit}
}

// SetRequestRecorder 设置全局记录器（nil 关闭）。
func SetRequestRecorder(recorder *RequestRecorder) {
	requestRecorderMu.Lock()
	requestRecorder = recorder
	requestRecorderMu.Unlock()
}

// recordRequest 记录一次请求体（provider 名 + 请求体）。
//
// 同时在 requests/index.log 里记一行摘要：序号、时间、大小、sha256。
// 那份摘要本身就能看出"两次请求是不是完全一样"，不用逐字节 diff。
func recordRequest(provider string, body []byte) {
	requestRecorderMu.RLock()
	recorder := requestRecorder
	requestRecorderMu.RUnlock()
	if recorder == nil {
		return
	}
	recorder.write(provider, body)
}

func (r *RequestRecorder) write(provider string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.written >= r.limit {
		return
	}
	r.written++
	seq := r.written

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	// 文件名带序号与 hash 前 8 位：一眼能看出"这份和上一份有没有变"。
	name := filepath.Join(r.dir, formatSeq(seq)+"-"+hash[:8]+".json")
	if err := os.WriteFile(name, body, 0o644); err != nil {
		return
	}

	// index.log 累积：每行 = 序号 时间 provider 大小 hash。
	// 两次请求前缀相同的话，从这里就能看出哪几份的 hash 一模一样。
	index, err := os.OpenFile(filepath.Join(r.dir, "index.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer index.Close()
	line := formatSeq(seq) + " " + time.Now().Format("15:04:05.000") + " " +
		provider + " " + itoa(len(body)) + "B " + hash + "\n"
	_, _ = index.WriteString(line)
}

// formatSeq 把序号补成 3 位（001、002…），文件按名字排序就是时间顺序。
func formatSeq(seq int) string {
	if seq < 10 {
		return "00" + itoa(seq)
	}
	if seq < 100 {
		return "0" + itoa(seq)
	}
	return itoa(seq)
}

// itoa 是只处理非负整数的极简实现（避免为此引入 strconv）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := [20]byte{}
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}