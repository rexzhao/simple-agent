package wire

import (
	"net"
	"sync"
	"testing"
)

// 回归测试：SetDialFunc 与 Dial 并发调用必须安全。
//
// 这是 Android 端真实崩溃的根因：以前 dialFunc 是裸全局变量，
// 连接/断开/重连在不同协程里切换拨号方式，同时另一条协程可能在拨号。
// Go 运行时检测到并发读写会直接 fatal error（SIGABRT），进程消失。
//
// 用 -race 跑这个测试能直接看出问题；即使不加 -race，
// 反复并发调用也有机会触发 fatal error。
func TestDialFuncConcurrent(t *testing.T) {
	defer SetDialFunc(nil)

	const rounds = 200
	var wg sync.WaitGroup

	// 写者：不停切换拨号方式
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				SetDialFunc(func(addr string) (net.Conn, error) { return nil, net.ErrClosed })
			} else {
				SetDialFunc(nil)
			}
		}
	}()

	// 读者：不停读取当前拨号函数（不真的建立连接）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			fn := DialFunc()
			if fn == nil {
				t.Error("DialFunc 不该返回 nil")
				return
			}
		}
	}()

	wg.Wait()
}

// 并发调用 Dial 自身也不能崩（用一定会失败的拨号函数，避免真的建连接）。
func TestDialConcurrentWithSetDialFunc(t *testing.T) {
	defer SetDialFunc(nil)
	SetDialFunc(func(addr string) (net.Conn, error) { return nil, net.ErrClosed })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := Dial("127.0.0.1:1"); err == nil {
					t.Error("应当返回错误")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// SetDialFunc(nil) 必须恢复到能用的默认实现。
func TestSetDialFuncNilRestoresDefault(t *testing.T) {
	SetDialFunc(func(addr string) (net.Conn, error) { return nil, net.ErrClosed })
	SetDialFunc(nil)
	if _, err := Dial("127.0.0.1:1"); err == nil {
		t.Fatal("恢复默认后应当尝试真实拨号（对 127.0.0.1:1 会失败）")
	}
}
