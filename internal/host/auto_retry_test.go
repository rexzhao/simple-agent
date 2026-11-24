package host

import (
	"testing"
	"time"
)

// 退避时间：0.8s 起，指数增长，上限 8s。
//
// 立刻重发容易再撞一次同样的抖动，等一会儿成功率高得多
// （pi 的 provider-retry 也是这个策略）。
func TestRetryBackoffGrows(t *testing.T) {
	first := retryBackoff(1)
	if first != 800*time.Millisecond {
		t.Fatalf("首次退避应是 0.8s，实际 %s", first)
	}

	prev := time.Duration(0)
	for attempt := 1; attempt <= 8; attempt++ {
		cur := retryBackoff(attempt)
		if cur < prev {
			t.Fatalf("第 %d 次的退避（%s）不该小于上一次（%s）", attempt, cur, prev)
		}
		if cur > 8*time.Second {
			t.Fatalf("退避不该超过 8s：%s", cur)
		}
		prev = cur
	}

	// 0 / 负数按第一次处理，不能 panic 也不能返回 0
	if got := retryBackoff(0); got != 800*time.Millisecond {
		t.Fatalf("attempt=0 应按首次处理，实际 %s", got)
	}
	if got := retryBackoff(-3); got != 800*time.Millisecond {
		t.Fatalf("负数应按首次处理，实际 %s", got)
	}
}

// 自动重试次数上限是 2：网关偶发抖动通常一次就好，
// 连续两次都失败多半不是抖动（配置错、额度问题），再试只是让用户多等。
func TestMaxAutoRetry(t *testing.T) {
	if maxAutoRetry != 2 {
		t.Fatalf("上限应是 2，实际 %d", maxAutoRetry)
	}
}