package providers

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UsageRecorder 把每次请求收到的 usage 原始内容记到文件，供排查用。
//
// 为什么需要它：缓存命中率显示不出来时，最可能的分歧是"网关到底有没有
// 返回 cached_tokens"。解析层的单测只能证明"给了就能解析"，证明不了
// 真实响应里有没有这个字段。把原始 usage 块落盘，一眼就能分辨：
//   - 原文里有 cached_tokens → 我们解析/传递有问题
//   - 原文里没有            → 网关没给（或这轮真的没命中）
//
// 默认关闭（nil recorder 时零开销）。打开方式见 SetUsageRecorder。
type UsageRecorder struct {
	mu   sync.Mutex
	path string
}

var (
	usageRecorderMu sync.RWMutex
	usageRecorder   *UsageRecorder
)

// NewUsageRecorder 创建一个记录器，写入 dir/usage-debug.log。
func NewUsageRecorder(dir string) *UsageRecorder {
	if dir == "" {
		return nil
	}
	return &UsageRecorder{path: filepath.Join(dir, "usage-debug.log")}
}

// SetUsageRecorder 设置全局记录器（nil 表示关闭）。
//
// 全局是因为 provider 是值类型、由配置构造，不想为了排查把 recorder
// 一路透传下去。
func SetUsageRecorder(recorder *UsageRecorder) {
	usageRecorderMu.Lock()
	usageRecorder = recorder
	usageRecorderMu.Unlock()
}

// recordUsageRaw 记录一条原始 usage（provider 名 + SSE 原始片段）。
//
// 由解析器在**遇到 usage 块时**调用。数据里有 API key 之类的风险很低
// （usage 只含计数），但仍只缓存这一小块，不记整个响应。
func recordUsageRaw(provider, raw string) {
	usageRecorderMu.RLock()
	recorder := usageRecorder
	usageRecorderMu.RUnlock()
	if recorder == nil {
		return
	}
	recorder.write(provider, raw)
}

func (r *UsageRecorder) write(provider, raw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	line := time.Now().Format("15:04:05.000") + " [" + provider + "] " + raw + "\n"
	_, _ = file.WriteString(line)
}
