package host

import (
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// netLogOn 控制"网络收发"日志（--net-log）。
// 只在排查连接/转发问题时打开：每个包一行，量大。
var netLogOn atomic.Bool

// netLogOutput 是收发日志的落点。**只写日志文件，不写控制台**：
// 每个包一行会把终端刷满，而排查时只需要事后翻文件。
var (
	netLogMu  sync.Mutex
	netLogger = log.New(io.Discard, "", log.LstdFlags)
)

// EnableNetLog 打开/关闭网络收发日志。
func EnableNetLog(on bool) { netLogOn.Store(on) }

// SetNetLogOutput 指定收发日志写到哪里（通常是 simple-agent*.log 的文件句柄）。
func SetNetLogOutput(w io.Writer) {
	netLogMu.Lock()
	defer netLogMu.Unlock()
	if w == nil {
		netLogger = log.New(io.Discard, "", log.LstdFlags)
		return
	}
	netLogger = log.New(w, "", log.LstdFlags)
}

// netLog 打一行收发包日志（未开启时零成本）。
func netLog(dir, conn string, connID uint64, kind string, size int) {
	if !netLogOn.Load() {
		return
	}
	netLogMu.Lock()
	logger := netLogger
	netLogMu.Unlock()
	logger.Printf("net %s %s#%d kind=%s bytes=%d", dir, conn, connID, kind, size)
}
