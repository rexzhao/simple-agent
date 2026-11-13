package bridge

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
)

// 本文件负责**把 Go 侧的日志与崩溃信息留下来**。
//
// 为什么必需：Android 上 os.Stderr 不接 logcat，Go 的 log/panic/fatal 输出
// 全部丢掉。真机崩溃过两次（SIGABRT + runtime.raise），tombstone 里只有一条
// native 帧，看不到任何 Go 侧原因 —— 排查只能靠猜。
//
// 现在的做法：
//   - log 输出同时写 <cacheDir>/go.log 与控制台（logcat 里能看到 tag=GoLog）
//   - debug.SetCrashOutput 把 **fatal error 与 panic 的完整栈**写进同一个文件
//     （这个必须做：fatal error 不经 log，只在崩溃时由 runtime 输出到 stderr）

var (
	// logSetupMu 保护日志文件句柄的切换（多个 Client 可能并发调用 setupLogging）。
	logSetupMu sync.Mutex
	// logFile 是 log 包写入的文件句柄（与 crashFile 分开，见 setupLogging）。
	logFile *os.File
)

// logFilePath 记录本次运行的日志路径（可能是空：没配置缓存目录时）。
var logFilePath string

// setupLogging 配置 Go 侧日志文件。
//
// 返回日志文件路径（空表示不可写，此时只打控制台）。
//
// **不幂等**：如果已经指向别的目录，会关掉旧的重新打开。这样测试里每个
// TempDir 都能拿到自己的日志（用 sync.Once 的话第二个测试会写到第一个的目录）。
func setupLogging(dir string) string {
	logSetupMu.Lock()
	defer logSetupMu.Unlock()
	if dir == "" {
		return logFilePath
	}
	path := filepath.Join(dir, "go.log")
	if logFilePath == path && logFile != nil {
		return logFilePath // 已经是这个文件，不用重开
	}
	// 换目录：先把旧的关掉
	closeLogFileLocked()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("bridge：无法创建日志目录 %s：%v", dir, err)
		return ""
	}
	// 超过 8MB 就滚动一次（保留上一份），避免长期运行把存储占满
	rotateIfLarge(path, 8<<20)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("bridge：无法打开日志文件 %s：%v", path, err)
		return ""
	}
	logFile = file
	logFilePath = path

	// 每条日志带上时间戳与级别前缀，便于和 logcat 对齐
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(file, stderrWriter{}))

	// **关键**：fatal error（如 concurrent map write、out of memory、
	// all goroutines asleep）不经过 log 包，是 runtime 直接写到 fd 2 的。
	// SetCrashOutput 让这些信息（含完整 goroutine 栈）落到同一个文件里。
	// 单独开一个句柄：SetCrashOutput 可能在崩溃时异步写，不能和 log 共用。
	crash, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		_ = debug.SetCrashOutput(crash, debug.CrashOptions{})
		crashFile = crash
	}

	log.Printf("bridge：Go 日志开始写入 %s", path)
	return logFilePath
}

// closeLogFileLocked 关闭当前日志文件（调用方持有 logSetupMu）。
func closeLogFileLocked() {
	if logFile != nil {
		_ = logFile.Sync()
		_ = logFile.Close()
		logFile = nil
	}
	if crashFile != nil {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = crashFile.Close()
		crashFile = nil
	}
	logFilePath = ""
}

// crashFile 是 SetCrashOutput 接管的文件句柄。
//
// 用引用计数管理：可能同时有多个 Client（测试里很常见），只在实际没人用时
// 才关闭，否则一次 Close 会把别人的日志也切断。
var (
	crashFile  *os.File
	logRefCount int
	logRefMu   sync.Mutex
)

// retainLogging 记录一个使用者（与 releaseLogging 配对）。
func retainLogging() {
	logRefMu.Lock()
	logRefCount++
	logRefMu.Unlock()
}

// releaseLogging 释放一个使用者；最后一个释放时才真正关闭文件。
func releaseLogging() {
	logRefMu.Lock()
	if logRefCount > 0 {
		logRefCount--
	}
	last := logRefCount == 0
	logRefMu.Unlock()
	if !last {
		return
	}
	logSetupMu.Lock()
	log.SetOutput(io.Discard)
	closeLogFileLocked()
	logSetupMu.Unlock()
}

// rotateIfLarge 在文件超过上限时改名一份（只保留 .1 一份，够定位最近一次问题）。
func rotateIfLarge(path string, limit int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < limit {
		return
	}
	_ = os.Rename(path, path+".1")
}

// stderrWriter 把日志同时打到 stderr。
//
// Android 上 stderr 不进 logcat，但桌面/测试环境需要看到；而且 gomobile 的
// bind 环境里 stderr 有时被 Kotlin 侧的 System.out 重定向，能看到就赚到。
type stderrWriter struct{}

func (stderrWriter) Write(p []byte) (int, error) {
	return os.Stderr.Write(p)
}

// LogPath 返回本次运行的 Go 日志文件路径（界面/排障用；空表示没写文件）。
func LogPath() string { return logFilePath }

// ReadLogTail 返回日志文件末尾最多 maxBytes 字节（界面"查看日志"用）。
func ReadLogTail(maxBytes int64) string {
	path := logFilePath
	if path == "" {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	return string(content)
}
