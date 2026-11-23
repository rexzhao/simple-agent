package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// idleTimeout 是流空闲超时：超过这个时间没有收到任何字节（心跳行也算有数据）
// 就主动断开。Rust 阶段就是因为缺这个超时，连接静默死掉后会话一直停在「正在生成」。
const idleTimeout = 180 * time.Second

// contentTimeout 是"没有任何**事件**输出"的上限。
//
// 只按字节算空闲不够：有些网关会持续发 SSE 注释/心跳行（`: ping`），字节不断，
// 但一个真正的 chunk 都不来——按字节判断的话会永远挂着（用户看到的"停止按钮还在、
// 一直没有输出"）。所以按"解析出事件"计时。
var contentTimeout = 90 * time.Second

// firstEventTimeout 是"**首字节之前**"的等待上限，比 contentTimeout 宽松得多。
//
// 为什么必须分开（真实的坑）：contentTimeout 原本从请求发出就开始计时，
// 于是"首字节前的等待"也被当成"没有输出"。对大上下文 + 慢网关，首字节
// 本来就慢 —— 实测 101 条消息的会话首字节要 41 秒，网关再稍慢一点就撞上
// 90 秒被自己掐断，日志里表现为"卡 95 秒后失败、out=0"，看着像网关挂了，
// 其实是我们在服务端还在思考时就断了它。
//
// 首字节之前的等待是"服务端在处理"（排队、prefill、思考），长是正常的；
// 传出第一个事件之后，再长时间没新事件才真叫卡住。
//
// 取 5 分钟：与 idleTimeout 相比它更长，因为 prefill 大上下文确实可能很慢；
// 但也不能无限 —— 网关彻底不回时用户至少能在几分钟后看到失败。
var firstEventTimeout = 300 * time.Second

// SetContentTimeoutForTest 调整"无输出"上限（测试用）。
func SetContentTimeoutForTest(d time.Duration) { contentTimeout = d }

// SetFirstEventTimeoutForTest 调整"首字节之前"的等待上限（测试用）。
func SetFirstEventTimeoutForTest(d time.Duration) { firstEventTimeout = d }

// sharedClient 是包级共享的 HTTP 客户端。
//
// 不能每次请求都新建 Transport：连接无法复用，而且每个 Transport 的空闲连接与
// keep-alive goroutine 都不会回收，长时间运行会越积越多。
// 不设总超时（长回答合法），只设「响应头」超时与流空闲超时。
var sharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// 响应头超时：必须**大于** firstEventTimeout，否则它会先于我们的
		// watchdog 触发，用户看到的错误来自 net/http（"timeout awaiting
		// response headers"），既没人话解释、也没法区分"首字节慢"和"真卡住"。
		//
		// 大上下文 + 慢网关时首字节实测要 41 秒；原来的 120 秒余量太薄。
		ResponseHeaderTimeout: firstEventTimeout + 30*time.Second,
		DisableCompression:    true, // SSE 不压缩，避免中间层缓冲
		MaxIdleConnsPerHost:   4,
	},
}

// Parser 把一条 SSE 事件翻译成中立事件，并在流结束时兜底。
type Parser interface {
	// Parse 翻译一条 SSE 事件（可返回 0..n 个事件）。
	Parse(SSEEvent) []Event
	// Finish 在 EOF（且 ctx 未取消）时调用，必须给出终止事件：
	// 收到过停止原因就补 Done，否则报错——「通道关闭但没收到终止事件」应该是失败，
	// 不能让一次正常回答被判成中断，也不能让一次截断被当成正常结束。
	Finish() []Event
}

// streamOptions 描述一次流式 POST。
type streamOptions struct {
	URL     string
	Headers map[string]string
	Body    []byte
	Parser  Parser
	// IdleTimeout 为 0 时用 idleTimeout 默认值。
	IdleTimeout time.Duration
}

// postStream 发起流式 POST，把解析后的事件写入返回的 channel。
//
// channel 在以下情况关闭：流正常结束、解析出错、ctx 取消。
// 首个请求错误（非 2xx、无法连接）通过返回值报告；流内错误通过 EventError 报告。
//
// 调用方契约：必须么读到 channel 关闭，要么 cancel ctx——channel 有缓冲但不是
// 无限大，长期不读会让发送方 goroutine 阻塞在 select 上。
func postStream(ctx context.Context, opts streamOptions) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL, bytes.NewReader(opts.Body))
	if err != nil {
		return nil, &Error{Kind: "request", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.Canceled
		}
		return nil, &Error{Kind: "network", Message: err.Error()}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &Error{
			Kind:    "http",
			Status:  resp.StatusCode,
			Message: strings.TrimSpace(string(body)),
		}
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		timeout := opts.IdleTimeout
		if timeout <= 0 {
			timeout = idleTimeout
		}
		body := newIdleReader(resp.Body, timeout)
		defer body.Stop()
		defer resp.Body.Close()

		var aborted atomic.Bool
		// 首字节之前用**更宽**的超时：那时服务端还在排队/prefill/思考，
		// 长是正常的（实测大上下文首字节要 41 秒）。收到第一个事件后
		// 才切到 contentTimeout —— 那时"长时间没新事件"才真叫卡住。
		//
		// 不这么分的话，90 秒的阈值会把"慢但正常"的请求掐死，
		// 表现成"卡 95 秒后失败、out=0"，看着像网关挂了。
		watchdog := time.AfterFunc(firstEventTimeout, func() {
			aborted.Store(true)
			_ = resp.Body.Close() // 让阻塞中的读立刻返回
		})
		defer watchdog.Stop()
		reader := NewSSEReader(body)
		// sawEvent 让超时消息说清"是没等到首字节"还是"输出到一半停了"——
		// 两者排查方向完全不同（前者看服务端/网络，后者看内容长度/限流）。
		sawEvent := false
		for {
			sse, err := reader.Next()
			// 收到事件就把计时重置为**内容超时**（比首字节超时短）：
			// 第一个事件到达说明服务端已经开始产出，之后就该按
			// "多久没有新内容"来判断是不是卡住了。
			watchdog.Reset(contentTimeout)
			sawEvent = sawEvent || err == nil

			if err != nil {
				// 取消：不产生任何事件，直接关通道（Cancelled 不进模型历史）
				if ctx.Err() != nil {
					return
				}
				// 空闲超时：报错误，不能当成正常结束
				if aborted.Load() {
					message := fmt.Sprintf("超过 %s 没有收到任何模型输出，已中止", contentTimeout)
					if !sawEvent {
						// 一个事件都没等到：这是"首字节前"的超时，
						// 上限比 contentTimeout 宽得多，要把真实时长说出来，
						// 否则用户会以为 90 秒就断了。
						message = fmt.Sprintf(
							"等待 %s 仍未收到任何响应（服务端可能过载或网络不通），已中止",
							firstEventTimeout)
					}
					emit(ctx, out, Event{Kind: EventError, Err: &Error{
						Kind:    "timeout",
						Message: message,
					}})
					return
				}
				if body.TimedOut() {
					emit(ctx, out, Event{Kind: EventError, Err: &Error{
						Kind:    "timeout",
						Message: fmt.Sprintf("连接空闲超过 %s，已中止", timeout),
					}})
					return
				}
				if errors.Is(err, io.EOF) {
					emitAll(ctx, out, opts.Parser.Finish())
					return
				}
				emit(ctx, out, Event{Kind: EventError, Err: &Error{Kind: "stream", Message: err.Error()}})
				return
			}

			for _, event := range opts.Parser.Parse(sse) {
				if !emit(ctx, out, event) {
					return
				}
				if event.Kind == EventDone || event.Kind == EventError {
					return
				}
			}
		}
	}()

	return out, nil
}

// emit 写一个事件；ctx 取消时返回 false。
func emit(ctx context.Context, out chan<- Event, event Event) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func emitAll(ctx context.Context, out chan<- Event, events []Event) {
	for _, event := range events {
		if !emit(ctx, out, event) {
			return
		}
	}
}

// idleReader 在读数时重置定时器；超时后关闭底层连接。
type idleReader struct {
	r        io.ReadCloser
	timeout  time.Duration
	timer    *time.Timer
	timedOut atomic.Bool
}

func newIdleReader(r io.ReadCloser, timeout time.Duration) *idleReader {
	ir := &idleReader{r: r, timeout: timeout}
	ir.timer = time.AfterFunc(timeout, func() {
		ir.timedOut.Store(true)
		_ = r.Close()
	})
	return ir
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.timer.Reset(ir.timeout)
	}
	return n, err
}

// TimedOut 报告是否因空闲超时被关闭。
func (ir *idleReader) TimedOut() bool { return ir.timedOut.Load() }

// Stop 停止看门狗。
func (ir *idleReader) Stop() { ir.timer.Stop() }

// doneEvent 构造结束事件。
func doneEvent(reason string) Event {
	return Event{Kind: EventDone, StopReason: reason}
}

// errorEvent 构造流内错误。
func errorEvent(format string, args ...any) Event {
	return Event{Kind: EventError, Err: fmt.Errorf(format, args...)}
}
