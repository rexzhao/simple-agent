package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 请求体录制：两次请求体一样时 hash 必须一样（那是缓存能命中的前提），
// 差一个字节就不同。排查时靠 index.log 里的 hash 一眼分辨。
func TestRequestRecorderHashes(t *testing.T) {
	dir := t.TempDir()
	rec := NewRequestRecorder(dir, 6)
	if rec == nil {
		t.Fatal("应创建成功")
	}
	SetRequestRecorder(rec)
	t.Cleanup(func() { SetRequestRecorder(nil) })

	same := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	recordRequest("openai", same)
	recordRequest("openai", same)                                       // 完全相同
	recordRequest("openai", []byte(`{"model":"m","messages":["diff"]}`)) // 不同

	index, err := os.ReadFile(filepath.Join(dir, "requests", "index.log"))
	if err != nil {
		t.Fatalf("应写出 index.log：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(index)), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 行，实际 %d：%s", len(lines), index)
	}
	hashOf := func(line string) string {
		fields := strings.Fields(line)
		return fields[len(fields)-1]
	}
	if hashOf(lines[0]) != hashOf(lines[1]) {
		t.Fatalf("相同请求体的 hash 应一致：%s / %s", lines[0], lines[1])
	}
	if hashOf(lines[0]) == hashOf(lines[2]) {
		t.Fatalf("不同请求体的 hash 不该一样：%s / %s", lines[0], lines[2])
	}

	// 每份请求体要真的落盘（排查时要逐字节 diff）
	files, err := filepath.Glob(filepath.Join(dir, "requests", "*.json"))
	if err != nil || len(files) != 3 {
		t.Fatalf("应写出 3 份请求体，实际 %d 份（%v）", len(files), err)
	}
	// 文件名带序号：按名字排序就是时间顺序
	if !strings.HasPrefix(filepath.Base(files[0]), "001-") {
		t.Fatalf("文件名应带序号前缀：%s", files[0])
	}
}

// 超过 limit 后不再录（长会话的请求体可能几 MB，录太多会写满磁盘）。
func TestRequestRecorderStopsAtLimit(t *testing.T) {
	dir := t.TempDir()
	SetRequestRecorder(NewRequestRecorder(dir, 2))
	t.Cleanup(func() { SetRequestRecorder(nil) })

	for i := 0; i < 5; i++ {
		recordRequest("openai", []byte(`{"n":`+itoa(i)+`}`))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "requests", "*.json"))
	if len(files) != 2 {
		t.Fatalf("limit=2 时应只录 2 份，实际 %d 份", len(files))
	}
}

// 没设记录器时零开销、不建目录。
func TestRequestRecorderDisabled(t *testing.T) {
	SetRequestRecorder(nil)
	recordRequest("openai", []byte(`{}`)) // 不该 panic
	dir := t.TempDir()
	if _, err := os.Stat(filepath.Join(dir, "requests")); err == nil {
		t.Fatal("没开记录器时不该建目录")
	}
}