package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 前一版 bug：Close() 会关掉 crash 输出句柄，但 setupLogging 在
// "路径没变" 时提前返回，于是 crash 输出再也不会被接上。
// 真机上的表现是 panic 只剩一行 "panic: ..."，没有栈，排查只能靠猜。
//
// 这里直接构造那个状态（logFile 还在、crashFile 已被关掉），
// 验证 setupLogging 会把它重新接上。
func TestCrashOutputRearmedWhenLogFileAlive(t *testing.T) {
	dir := t.TempDir()

	path := setupLogging(dir)
	if path == "" {
		t.Fatal("setupLogging 应返回日志路径")
	}
	retainLogging()

	// 手动模拟"只关了 crash 输出、日志句柄还在"的状态
	// （closeLogFileLocked 里 crashFile 与 logFile 是分开处理的）
	logSetupMu.Lock()
	if crashFile != nil {
		_ = crashFile.Close()
		crashFile = nil
	}
	if logFile == nil {
		logSetupMu.Unlock()
		t.Fatal("这个测试需要 logFile 仍然存活")
	}
	logSetupMu.Unlock()

	// 再次 setupLogging 同一个目录：必须把 crash 输出重新接上
	again := setupLogging(dir)
	if again != path {
		t.Fatalf("路径应相同：%q vs %q", again, path)
	}

	logSetupMu.Lock()
	rearmed := crashFile
	logSetupMu.Unlock()
	if rearmed == nil {
		t.Fatal("logFile 存活时再次 setupLogging，crash 输出必须重新接上（否则 panic 拿不到栈）")
	}

	// 确认新句柄真的能写到 go.log
	if _, err := rearmed.WriteString("\n__rearm_probe__\n"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	_ = rearmed.Sync()

	data, err := os.ReadFile(filepath.Join(dir, "go.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "__rearm_probe__") {
		t.Fatal("写进 crash 句柄的内容没有落到 go.log")
	}

	releaseLogging()
}
