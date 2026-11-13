package bridge

import (
	"sync"
	"testing"
	"time"
)

// 收集回调的测试监听器。
type countingListener struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingListener() *countingListener {
	return &countingListener{counts: map[string]int{}}
}

func (c *countingListener) OnChange(kind string, sessionID string) {
	c.mu.Lock()
	c.counts[kind]++
	c.mu.Unlock()
}

func (c *countingListener) count(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[kind]
}

// 高频事件必须被合并：连发 100 次 conversation，回调次数应远小于 100。
func TestEmitCoalescesHighFrequency(t *testing.T) {
	listener := newCountingListener()
	client := NewClient(listener, "zh", t.TempDir())
	t.Cleanup(client.Close)

	// 200 次连续 emit，模拟流式输出
	for i := 0; i < 200; i++ {
		client.emit("conversation", "s1")
	}
	// 等合并窗口过去
	time.Sleep(changeFlushInterval * 3)

	got := listener.count("conversation")
	if got == 0 {
		t.Fatal("合并后仍应至少回调一次（否则内容不会更新）")
	}
	if got > 10 {
		t.Fatalf("200 次 emit 应被合并成很少几次回调，实际 %d 次", got)
	}
	t.Logf("200 次 emit → %d 次回调", got)
}

// 不同 kind 必须各自独立：runtime 不能被当成 conversation 合并掉。
func TestEmitKeepsKindSemantics(t *testing.T) {
	listener := newCountingListener()
	client := NewClient(listener, "zh", t.TempDir())
	t.Cleanup(client.Close)

	client.emit("conversation", "s1")
	client.emit("runtime", "s1")
	client.emit("notice", "s1")
	time.Sleep(changeFlushInterval * 3)

	for _, kind := range []string{"conversation", "runtime", "notice"} {
		if listener.count(kind) == 0 {
			t.Fatalf("%s 事件丢了（合并时不能跨 kind 合并）", kind)
		}
	}
}

// 低频事件必须立即发：延迟会让用户觉得"点了没反应"。
func TestEmitSendsLowFrequencyImmediately(t *testing.T) {
	listener := newCountingListener()
	client := NewClient(listener, "zh", t.TempDir())
	t.Cleanup(client.Close)

	client.emit("sessions", "")
	// 不等 flush 窗口
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if listener.count("sessions") > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sessions 这类低频事件应当立即回调，不应等合并窗口")
}