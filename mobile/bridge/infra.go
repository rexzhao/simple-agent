package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
)

// 本文件是 bridge 的"基础设施"：串行投递器与图片缓存。

// poster 是一个**串行**执行队列。
//
// 为什么需要它：host.Remote 要求"所有镜像修改都发生在同一个线程"（见
// docs/remote-design.md §1.6）。桌面有 mygo 主线程，Android 有 UI 线程——
// 但那个线程在 Kotlin 侧，Go 看不到。
//
// 这里提供一个 Go 侧的串行线程：所有对镜像的读写、所有回调都排在它上面执行，
// 于是"同一个线程"这个前提就成立了；Kotlin 收到回调后自己再切到 UI 线程。
type poster struct {
	mu     sync.Mutex
	queue  []func()
	run    chan struct{}
	once   sync.Once
	closed bool
}

func newPoster() *poster {
	p := &poster{run: make(chan struct{}, 1)}
	go p.loop()
	return p
}

// submit 把函数排进队列（非阻塞；队列无限增长，因为每项都很小）。
func (p *poster) submit(fn func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.queue = append(p.queue, fn)
	p.mu.Unlock()
	select {
	case p.run <- struct{}{}:
	default:
		// 已经有唤醒信号在路上：loop 会把队列清空
	}
}

// loop 串行执行队列里的函数。
//
// **每个回调都必须 recover**：这是在一条 goroutine 上跑所有镜像修改与回调，
// 任何一处 panic（host 层的解码、driver 的状态机、甚至 Kotlin 回调里抛出的
// 异常穿过 JNI 回来）都会让整个 Go 运行时 abort ——Android 上表现为
// SIGABRT + runtime.raise，进程直接消失，而且**拿不到任何 panic 原因**
// （Go 的 fatal 信息默认不进 logcat）。真机上就是这么崩过两次。
//
// 现在 panic 只影响那一次回调：记一条日志，队列继续跑。
func (p *poster) loop() {
	for range p.run {
		for {
			p.mu.Lock()
			if len(p.queue) == 0 {
				p.mu.Unlock()
				break
			}
			batch := p.queue
			p.queue = nil
			p.mu.Unlock()
			for _, fn := range batch {
				runSafely(fn)
			}
		}
	}
}

// runSafely 执行一个回调并把 panic 转成日志（不中断串行队列）。
func runSafely(fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// 带栈：下次真出问题时能直接看到是哪条路径
			log.Printf("bridge：回调 panic（已忽略，队列继续）：%v\n%s",
				recovered, debug.Stack())
		}
	}()
	fn()
}

func (p *poster) close() {
	p.mu.Lock()
	p.closed = true
	p.queue = nil
	p.mu.Unlock()
	p.once.Do(func() { close(p.run) })
}

// saveBlob 把图片写进本地缓存目录，返回文件路径。
//
// 内容寻址：文件名就是 sha256，重复写入无副作用；读的时候按 hash 找。
func (c *Client) saveBlob(hash string, data []byte) (string, error) {
	if c.blobDir == "" {
		return "", errors.New("没有配置缓存目录")
	}
	sum := sha256.Sum256(data)
	if actual := hex.EncodeToString(sum[:]); actual != hash {
		return "", errors.New("图片校验失败（sha256 不匹配）")
	}
	dir := filepath.Join(c.blobDir, "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hash)
	if existing, err := os.ReadFile(path); err == nil && len(existing) == len(data) {
		return path, nil // 已有缓存
	}
	// 先写临时文件再改名：中途失败不会留下半个文件被当成有效缓存
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	return path, nil
}
