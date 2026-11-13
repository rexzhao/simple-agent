package bridge

import (
	"sync"
	"testing"
	"time"
)

// 回归测试：投递器里的一个回调 panic，不能让整个进程挂掉，也不能让队列停摆。
//
// 这是 Android 崩溃（SIGABRT + runtime.raise）最可能的原因：所有镜像修改与
// 回调都排在这条 goroutine 上，之前没有 recover，任何一处 panic 都会让 Go
// 运行时 abort，而且**拿不到任何原因**。
func TestPosterSurvivesPanic(t *testing.T) {
	p := newPoster()
	defer p.close()

	var mu sync.Mutex
	ran := 0
	done := make(chan struct{}, 1)

	p.submit(func() {
		panic("故意 panic，模拟某个回调出错")
	})
	p.submit(func() {
		mu.Lock()
		ran++
		mu.Unlock()
	})
	// 第三个：确认 panic 之后队列仍然继续执行
	p.submit(func() {
		mu.Lock()
		ran++
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panic 之后队列停摆了（后续回调没有执行）")
	}

	mu.Lock()
	defer mu.Unlock()
	if ran != 2 {
		t.Fatalf("panic 之后应继续执行 2 个回调，实际 %d", ran)
	}
}