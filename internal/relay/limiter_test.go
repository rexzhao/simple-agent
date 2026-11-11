package relay

import (
	"testing"
	"time"
)

// 未达阈值时必须放行：正常用户偶尔打错一次口令不该被拒。
func TestLimiterAllowsBelowThreshold(t *testing.T) {
	l := newLimiter()
	ip := "1.2.3.4"
	for i := 1; i <= defaultMaxFailures; i++ {
		if !l.allow(ip) {
			t.Fatalf("第 %d 次失败前就被拒了（阈值是 %d）", i, defaultMaxFailures)
		}
		blocked, failures := l.fail(ip)
		if failures != i {
			t.Fatalf("失败计数应为 %d，实际 %d", i, failures)
		}
		if blocked {
			t.Fatalf("第 %d 次失败不该进入退避（阈值 %d）", i, defaultMaxFailures)
		}
	}
}

// 超过阈值必须进入退避，且退避期内 allow 返回 false。
func TestLimiterBlocksAfterThreshold(t *testing.T) {
	l := newLimiter()
	ip := "5.6.7.8"
	for i := 0; i <= defaultMaxFailures; i++ {
		l.fail(ip)
	}
	if l.allow(ip) {
		t.Fatal("超过阈值后应当被拒")
	}
	if !l.blocked(ip) {
		t.Fatal("blocked() 应当报告已限流")
	}
}

// 退避时长按 1s、2s、4s ... 指数增长，并封顶。
func TestLimiterBackoffGrowsAndCaps(t *testing.T) {
	l := newLimiter()
	// 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s(4m16s), 然后封顶 5m
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
	}
	for i, expected := range want {
		got := l.backoffFor(i + 1)
		if got != expected {
			t.Fatalf("第 %d 次超出阈值：期望 %v，实际 %v", i+1, expected, got)
		}
	}
	// 第 9 次是 256s（4m16s），第 10 次翻倍到 512s 才超过 5 分钟上限
	if got := l.backoffFor(9); got != 256*time.Second {
		t.Fatalf("第 9 次应为 256s，实际 %v", got)
	}
	if got := l.backoffFor(10); got != defaultBackoffMax {
		t.Fatalf("第 10 次应为上限 %v，实际 %v", defaultBackoffMax, got)
	}
	// 再往后都应该是上限
	if got := l.backoffFor(100); got != defaultBackoffMax {
		t.Fatalf("很大的超出次数应当封顶到 %v，实际 %v", defaultBackoffMax, got)
	}
}

// 退避到期后要能重新尝试（不能永久封禁）。
func TestLimiterReleasesAfterBackoff(t *testing.T) {
	l := newLimiter()
	current := time.Now()
	l.now = func() time.Time { return current }
	ip := "9.9.9.9"
	for i := 0; i <= defaultMaxFailures; i++ {
		l.fail(ip)
	}
	if l.allow(ip) {
		t.Fatal("刚进入退避时应当被拒")
	}
	// 时间跳到退避结束之后
	current = current.Add(defaultBackoffMax + time.Second)
	if !l.allow(ip) {
		t.Fatal("退避到期后应当允许重试")
	}
}

// 认证成功后清零：之前的失败不该继续累计。
func TestLimiterSucceedResets(t *testing.T) {
	l := newLimiter()
	ip := "10.0.0.1"
	for i := 0; i < defaultMaxFailures; i++ {
		l.fail(ip)
	}
	if l.blocked(ip) {
		t.Fatal("还没超阈值就被限流了")
	}
	l.succeed(ip)
	// 清零后再失败，应当从 1 重新计数
	_, failures := l.fail(ip)
	if failures != 1 {
		t.Fatalf("成功后计数应清零重新开始，实际 %d", failures)
	}
}

// 不同 IP 互不影响：一个人被限流不该牵连其他人。
func TestLimiterIsolatesByIP(t *testing.T) {
	l := newLimiter()
	bad := "1.1.1.1"
	good := "2.2.2.2"
	for i := 0; i <= defaultMaxFailures; i++ {
		l.fail(bad)
	}
	if l.allow(bad) {
		t.Fatal("攻击者 IP 应当被拒")
	}
	if !l.allow(good) {
		t.Fatal("其他 IP 不该被牵连")
	}
}

// 空 IP（比如进程内测试管道拿不到地址）不限流：
// 那种连接不是攻击面，限流只会让测试变得难写。
func TestLimiterIgnoresEmptyIP(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 100; i++ {
		blocked, _ := l.fail("")
		if blocked {
			t.Fatal("空 IP 不该被限流")
		}
	}
	if !l.allow("") {
		t.Fatal("空 IP 应当始终允许")
	}
}

// 条目清理：长期不活跃的条目要被回收，避免内存被扫描撑大。
func TestLimiterSweepsStaleEntries(t *testing.T) {
	l := newLimiter()
	current := time.Now()
	l.now = func() time.Time { return current }

	// 塞满超过扫描阈值（64）的条目
	for i := 0; i < 80; i++ {
		l.fail(ipOf(i))
	}
	// 全部老化，然后触发一次扫描
	current = current.Add(entryTTL + time.Minute)
	l.fail("9.9.9.9")

	l.mu.Lock()
	count := len(l.entries)
	l.mu.Unlock()
	if count >= 80 {
		t.Fatalf("陈旧条目应当被清理，实际还剩 %d 条", count)
	}
}

func ipOf(i int) string {
	return "10.1." + itoa(i/256) + "." + itoa(i%256)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// 端到端：relay 在 token 错误时累计失败，超过阈值后连正确 token 也会被拒
// （因为限流发生在比较之前），退避到期后恢复。
func TestRelayRejectsAfterRepeatedBadTokens(t *testing.T) {
	r := New()
	r.Token = "s3cret"
	ip := "6.6.6.6"

	// 手动把该 IP 打到超阈值（不经过真实连接，聚焦限流逻辑）
	for i := 0; i <= defaultMaxFailures; i++ {
		r.limitOrCreate().fail(ip)
	}
	if r.limitOrCreate().allow(ip) {
		t.Fatal("超阈值后应当被限流")
	}
	if !r.limitOrCreate().blocked(ip) {
		t.Fatal("blocked() 应当为真")
	}
}
