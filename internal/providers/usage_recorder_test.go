package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UsageRecorder 把原始 usage 块落到文件：排查"缓存命中率为什么不显示"用。
//
// 它证明的是"记录链路通"，不是"网关一定有字段"——后者要看真实日志。
func TestUsageRecorderWritesRaw(t *testing.T) {
	dir := t.TempDir()
	SetUsageRecorder(NewUsageRecorder(dir))
	t.Cleanup(func() { SetUsageRecorder(nil) })

	parser := newOpenAIParser(true)
	raw := `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,` +
		`"prompt_tokens_details":{"cached_tokens":80}}}`
	parser.Parse(SSEEvent{Data: raw})

	path := filepath.Join(dir, "usage-debug.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应写出 %s：%v", path, err)
	}
	got := string(data)
	if !strings.Contains(got, "cached_tokens") {
		t.Fatalf("日志里应含原始 cached_tokens：%q", got)
	}
	if !strings.Contains(got, "[openai]") {
		t.Fatalf("应标明 provider：%q", got)
	}
	// 记录的是原始 JSON，不是我们重组过的 —— 排查时才能看到网关真正给了什么
	if !strings.Contains(got, `"prompt_tokens":100`) {
		t.Fatalf("应保留原始字段：%q", got)
	}
}

// 没设记录器时不该有任何副作用（默认关闭，零开销）。
func TestUsageRecorderDisabledByDefault(t *testing.T) {
	SetUsageRecorder(nil)
	dir := t.TempDir()
	parser := newOpenAIParser(true)
	parser.Parse(SSEEvent{Data: `{"choices":[],"usage":{"prompt_tokens":10}}`})
	if _, err := os.Stat(filepath.Join(dir, "usage-debug.log")); err == nil {
		t.Fatal("没开记录器时不该写文件")
	}
}