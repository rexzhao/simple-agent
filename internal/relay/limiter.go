package relay

import (
	"log"
	"net"
	"sync"
	"time"
)

// limiter 按来源 IP 限制认证失败的频率。
//
// # 解决什么
//
// 没有限流时，攻击者可以每秒尝试几千次 token。即使 token 有一定强度，
// 弱口令几分钟就能被穷举；而且你完全不知道有人在试。
//
// # 策略
//
// 连续失败达到阈值后进入退避期，期间**直接拒绝**（连 token 都不比较）：
//
//	失败次数  1  2  3  4  5(阈值)  6  7  8 ...
//	延迟      0  0  0  0  0        1s 2s 4s  ... (上限 backoffMax)
//
// 成功一次就清零。
//
// 为什么"直接拒绝"而不是"变慢"：变慢会占用连接与 goroutine，
// 攻击者用少量并发就能耗尽资源（拒绝服务的副作用）。直接拒绝更省，也更简单。
//
// # 已知取舍
//
// 按 IP 计数意味着**同一 NAT/代理后面的正常用户会被连带影响**。
// 自用场景（几台自己的设备）可以接受；阈值调高即可缓解。
type limiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry

	// maxFailures 是进入退避前允许的连续失败次数（0 表示用默认值）。
	maxFailures int
	// backoffMax 是退避上限。
	backoffMax time.Duration
	// now 便于测试注入时间。
	now func() time.Time
}

type limitEntry struct {
	failures int
	// blockedUntil 非零表示在此时刻之前拒绝。
	blockedUntil time.Time
	// lastSeen 用于清理长期不活跃的条目，避免 map 无限增长。
	lastSeen time.Time
}

const (
	defaultMaxFailures = 5
	defaultBackoffMax  = 5 * time.Minute
	// entryTTL 是条目保留时长：超过这个时间没动静就丢弃（防止内存被扫描撑大）。
	entryTTL = 30 * time.Minute
)

func newLimiter() *limiter {
	return &limiter{
		entries:     make(map[string]*limitEntry),
		maxFailures: defaultMaxFailures,
		backoffMax:  defaultBackoffMax,
		now:         time.Now,
	}
}

// allow 报告来自 ip 的连接是否可以继续尝试认证。
//
// 被限流时不返回具体原因给客户端（避免给出"你被限流了"这种可用于探测的信息）。
func (l *limiter) allow(ip string) bool {
	if ip == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[ip]
	if !ok {
		return true
	}
	return !entry.blockedUntil.After(l.now())
}

// fail 记一次认证失败，返回是否因此进入退避。
//
// 返回值用于"只在状态变化时打日志"，避免日志洪水。
func (l *limiter) fail(ip string) (blocked bool, failures int) {
	if ip == "" {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)

	entry, ok := l.entries[ip]
	if !ok {
		entry = &limitEntry{}
		l.entries[ip] = entry
	}
	wasBlocked := entry.blockedUntil.After(now)
	entry.failures++
	entry.lastSeen = now

	maxFailures := l.maxFailures
	if maxFailures <= 0 {
		maxFailures = defaultMaxFailures
	}
	if entry.failures > maxFailures {
		delay := l.backoffFor(entry.failures - maxFailures)
		entry.blockedUntil = now.Add(delay)
	}
	return !wasBlocked && entry.blockedUntil.After(now), entry.failures
}

// backoffFor 算第 n 次超出阈值后的退避时长：1s, 2s, 4s ... 上限 backoffMax。
func (l *limiter) backoffFor(over int) time.Duration {
	if over <= 0 {
		return 0
	}
	backoffMax := l.backoffMax
	if backoffMax <= 0 {
		backoffMax = defaultBackoffMax
	}
	delay := time.Second
	for i := 1; i < over; i++ {
		delay *= 2
		if delay >= backoffMax {
			return backoffMax
		}
	}
	if delay > backoffMax {
		return backoffMax
	}
	return delay
}

// succeed 在认证成功后清零该 IP 的失败记录。
func (l *limiter) succeed(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// sweepLocked 清理长期不活跃的条目（调用方持锁）。
func (l *limiter) sweepLocked(now time.Time) {
	// 只在条目较多时才扫描，避免每次失败都遍历
	if len(l.entries) < 64 {
		return
	}
	for ip, entry := range l.entries {
		if now.Sub(entry.lastSeen) > entryTTL && !entry.blockedUntil.After(now) {
			delete(l.entries, ip)
		}
	}
}

// blocked 报告某 IP 当前是否处于退避中（测试与日志用）。
func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[ip]
	if !ok {
		return false
	}
	return entry.blockedUntil.After(l.now())
}

// remoteIP 从连接上取出来源 IP（取不到时返回空，表示"不按 IP 限流"）。
//
// wire.Conn 的接口故意不暴露地址（它是个协议层抽象），所以这里用接口断言
// 探测底层是否提供了远端地址。进程内测试用的管道连接没有地址，返回空即可
// ——测试里那条路径本来也不会被暴力尝试。
func remoteIP(conn any) string {
	type addrProvider interface {
		RemoteAddr() net.Addr
	}
	provider, ok := conn.(addrProvider)
	if !ok {
		return ""
	}
	addr := provider.RemoteAddr()
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// logAuthFailure 打一条认证失败日志（只在首次进入退避时打，避免洪水）。
func logAuthFailure(ip string, failures int, blocked bool) {
	if blocked {
		log.Printf("relay：认证失败 ip=%s 连续失败=%d，已进入退避（后续尝试会被直接拒绝）", ip, failures)
		return
	}
	// 未达阈值时只记录前几次，便于发现"有人在试"但不刷屏
	if failures <= 3 {
		log.Printf("relay：认证失败 ip=%s 连续失败=%d", ip, failures)
	}
}

// logRateLimited 记录被限流拒绝的连接（同样避免洪水：只在每 IP 首次时打）。
func logRateLimited(ip string) {
	log.Printf("relay：ip=%s 处于退避中，拒绝连接", ip)
}
