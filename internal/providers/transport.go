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

// SetContentTimeoutForTest 调整"无输出"上限（测试用）。
func SetContentTimeoutForTest(d time.Duration) { contentTimeout = d }

// sharedClient 是包级共享的 HTTP 客户端。
//
// 不能每次请求都新建 Transport：连接无法复用，而且每个 Transport 的空闲连接与
// keep-alive goroutine 都不会回收，长时间运行会越积越多。
// 不设总超时（长回答合法），只设「响应头」超时与流空闲超时。
var sharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 120 * time.Second,
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
		watchdog := time.AfterFunc(contentTimeout, func() {
			aborted.Store(true)
			_ = resp.Body.Close() // 让阻塞中的读立刻返回
		})
		defer watchdog.Stop()
		reader := NewSSEReader(body)
		for {
			sse, err := reader.Next()
			watchdog.Reset(contentTimeout)

			if err != nil {
				// 取消：不产生任何事件，直接关通道（Cancelled 不进模型历史）
				if ctx.Err() != nil {
					return
				}
				// 空闲超时：报错误，不能当成正常结束
				if aborted.Load() {
					emit(ctx, out, Event{Kind: EventError, Err: &Error{
						Kind:    "timeout",
						Message: fmt.Sprintf("超过 %s 没有收到任何模型输出，已中止", contentTimeout),
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
