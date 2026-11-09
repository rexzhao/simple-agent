package uiapp

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// 注册常见图片解码器（mygo 自带 gif/jpeg/png；webp 由 x/image 提供）
	_ "golang.org/x/image/webp"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"
)

// 图片加载：只支持 http(s) 与内嵌字节；异步下载 + 解码，限制大小与像素数。
//
// 安全与并发约束（G4）：
//   - **状态只在主线程改**：下载 goroutine 只算结果，赋值一律经 post 回主线程；
//   - 拒绝 loopback / 私有网段 / 链路本地地址：否则模型写一个
//     `![](http://127.0.0.1:8080/admin/delete?id=1)` 就能让应用去打本机服务；
//   - 只允许 http/https（不让 file: 读本地文件）。

const (
	// maxImageBytes 是单张图片的下载上限。
	maxImageBytes = 10 << 20
	// maxImagePixels 是解码上限（防解压炸弹）。
	maxImagePixels = 40_000_000
	// imageTimeout 是单张图片的总超时。
	imageTimeout = 30 * time.Second
)

// sharedImageClient 是全局复用的 HTTP 客户端（连接复用 + 统一超时）。
var sharedImageClient = &http.Client{
	Timeout: imageTimeout,
	Transport: &http.Transport{
		// 二次校验：DNS 解析之后再拒绝内网地址，防 DNS rebinding 绕过前置检查
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(resolved) == 0 {
				return nil, fmt.Errorf("%s", locale.Tr("image.resolveHost", host))
			}
			for _, addr := range resolved {
				if blockedIP(addr.IP) {
					return nil, fmt.Errorf("%s", locale.Tr("image.privateIP", host))
				}
			}
			// **直接拨已经校验过的 IP**：按主机名再拨一次会重新做 DNS 解析，
			// 那正是 DNS rebinding 的窗口（第一次返回公网地址、第二次返回 127.0.0.1）。
			// TLS 的 SNI 由 http.Transport 按 URL 里的主机名设置，不受影响。
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			var lastErr error
			for _, addr := range resolved {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	},
}

// cgnat 是运营商级 NAT 网段（100.64.0.0/10），net.IP 没有内置判断。
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// blockedIP 判断地址是否属于不该访问的范围。
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// checkImageScheme 只检查 scheme（测试注入下载时用）。
func checkImageScheme(url string) error {
	lower := strings.ToLower(strings.TrimSpace(url))
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return fmt.Errorf("%s", locale.Tr("image.onlyHTTP"))
	}
	return nil
}

// checkImageURL 做加载前的地址检查（scheme + 主机）。
func checkImageURL(url string) error {
	if err := checkImageScheme(url); err != nil {
		return err
	}
	lower := strings.ToLower(strings.TrimSpace(url))
	rest := lower[strings.Index(lower, "//")+2:]
	if index := strings.IndexAny(rest, "/?#"); index >= 0 {
		rest = rest[:index]
	}
	host := rest
	if index := strings.LastIndex(rest, ":"); index >= 0 && !strings.Contains(rest[index:], "]") {
		host = rest[:index]
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return fmt.Errorf("%s", locale.Tr("image.noHost"))
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("%s", locale.Tr("image.privateIP", host))
		}
		return nil
	}
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain", "ip6-localhost":
		return fmt.Errorf("%s", locale.Tr("image.localIP", host))
	}
	return nil
}

// imageState 是一次图片加载的状态（只在主线程读写）。
type imageState struct {
	bitmap  *ui.Bitmap
	failed  string
	started bool
}

// imageStore 按 URL 缓存图片状态。
type imageStore struct {
	mu     sync.Mutex
	states map[string]*imageState
}

func newImageStore() *imageStore { return &imageStore{states: map[string]*imageState{}} }

// state 取（或创建）某 URL 的状态（主线程调用）。
func (s *imageStore) state(url string) *imageState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.states[url]; ok {
		return state
	}
	state := &imageState{}
	s.states[url] = state
	return state
}

// clearFailed 清掉所有失败状态，让下次渲染重新发起加载。
//
// 展示端重连成功后调用：断线导致的"加载失败"不该是永久结论。
func (s *imageStore) clearFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for url, state := range s.states {
		if state.failed != "" && state.failed != "missing" {
			delete(s.states, url)
		}
	}
}

// lookup 只读查询（测试用）。
func (s *imageStore) lookup(url string) (*imageState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[url]
	return state, ok
}

// count 返回已登记的状态数（测试用）。
func (s *imageStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.states)
}

// fetchImage 下载图片字节（可在 goroutine 里调用，不碰状态）。
func (a *App) fetchImage(url string) ([]byte, error) {
	if a.opts.ImageFetcher != nil {
		// 测试注入：仍走一遍 scheme 检查，但不做内网地址拦截（测试服务器在 loopback）
		if err := checkImageScheme(url); err != nil {
			return nil, err
		}
		atomic.AddInt64(&a.imageFetches, 1)
		return a.opts.ImageFetcher(url)
	}
	if err := checkImageURL(url); err != nil {
		return nil, err
	}
	atomic.AddInt64(&a.imageFetches, 1)
	resp, err := sharedImageClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("%s", locale.Tr("image.tooLarge", maxImageBytes>>20))
	}
	return data, nil
}

// decodeImageBytes 解码图片字节并检查像素上限（纯函数，可在 goroutine 里调用）。
func decodeImageBytes(data []byte) (*ui.Bitmap, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s", locale.Tr("image.unknownFormat"))
	}
	if config.Width*config.Height > maxImagePixels {
		return nil, fmt.Errorf("%s", locale.Tr("image.tooManyPixels", config.Width, config.Height))
	}
	bitmap, err := ui.DecodeBitmap(data)
	if err != nil {
		return nil, fmt.Errorf("%s", locale.Tr("image.decodeFailed", err.Error()))
	}
	return bitmap, nil
}

// loadImageAsync 在后台加载图片；**结果经 post 回到主线程再赋值**（G4）。
func (a *App) loadImageAsync(url string, state *imageState) {
	go func() {
		data, err := a.fetchImage(url)
		var bitmap *ui.Bitmap
		if err == nil {
			bitmap, err = decodeImageBytes(data)
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		a.post(func() {
			if err != nil {
				state.failed = message
				return
			}
			state.bitmap = bitmap
		})
	}()
}

// renderImage 渲染一张图片（alt 是占位文本）。
func (a *App) renderImage(c *ui.Context, alt, url string) {
	theme := c.Theme()
	state := a.images.state(url)
	if !state.started {
		state.started = true
		if err := checkImageURL(url); err != nil {
			state.failed = err.Error()
		} else {
			a.loadImageAsync(url, state)
		}
	}

	if bitmap := state.bitmap; bitmap != nil {
		width, height := bitmap.Size()
		dipWidth, dipHeight := float32(width), float32(height)
		const maxWidth = float32(420)
		if dipWidth > maxWidth {
			ratio := maxWidth / dipWidth
			dipWidth, dipHeight = maxWidth, dipHeight*ratio
		}
		ui.Image(c, bitmap).Width(dipWidth).Height(dipHeight)
		if alt != "" {
			ui.Text(c, alt).FontSize(11).TextColor(theme.TextMuted)
		}
		return
	}
	if state.failed != "" {
		ui.Text(c, a.tr("chat.imageFailedNamed", alt, state.failed)).FontSize(12).TextColor(theme.Danger)
		return
	}
	ui.Text(c, a.tr("chat.imageLoadingNamed", alt)).FontSize(12).TextColor(theme.TextMuted)
}

// renderImagePart 渲染消息里的图片 part（URL 或内嵌字节走同一套限制）。
// renderBlobImage 渲染 blob 存储里的图片：后台读文件 + 解码，缺失时显示「[图片丢失]」。
func (a *App) renderBlobImage(c *ui.Context, name, hash, mediaType string) {
	theme := c.Theme()
	key := "blob:" + hash
	state := a.images.state(key)
	if !state.started {
		state.started = true
		if a.blobs() == nil {
			state.failed = "missing"
		} else {
			blobs := a.blobs()
			go func() {
				data, err := blobs.Open(hash)
				message := ""
				if err != nil {
					message = "missing" // blob 被删/损坏 → 「[图片丢失]」，不是加载失败
				}
				var bitmap *ui.Bitmap
				if message == "" {
					decoded, decodeErr := decodeImageBytes(data)
					if decodeErr != nil {
						message = decodeErr.Error()
					} else {
						bitmap = decoded
					}
				}
				a.post(func() {
					if message != "" {
						state.failed = message
						return
					}
					state.bitmap = bitmap
				})
			}()
		}
	}
	if bitmap := state.bitmap; bitmap != nil {
		width, height := bitmap.Size()
		dipWidth, dipHeight := float32(width), float32(height)
		const maxWidth = float32(420)
		if dipWidth > maxWidth {
			ratio := maxWidth / dipWidth
			dipWidth, dipHeight = maxWidth, dipHeight*ratio
		}
		ui.Image(c, bitmap).Width(dipWidth).Height(dipHeight)
		if name != "" {
			ui.Text(c, name).FontSize(11).TextColor(theme.TextMuted)
		}
		return
	}
	if state.failed == "missing" {
		// blob 被删/损坏：明确占位，不要显示成"加载失败"让人以为是网络问题
		ui.Text(c, a.tr("chat.imageMissing")).FontSize(12).TextColor(theme.TextMuted)
		return
	}
	if state.failed != "" {
		ui.Text(c, a.tr("chat.imageFailed", state.failed)).FontSize(12).TextColor(theme.Danger)
		return
	}
	ui.Text(c, a.tr("chat.imageLoading")).FontSize(12).TextColor(theme.TextMuted)
}

func (a *App) renderImagePart(c *ui.Context, name string, url string, raw []byte, mediaType string) {
	theme := c.Theme()
	if len(raw) > 0 {
		key := "inline:" + strconv.Itoa(len(raw)) + ":" + mediaType
		state := a.images.state(key)
		if !state.started {
			state.started = true
			data := raw
			go func() {
				bitmap, err := decodeImageBytes(data)
				message := ""
				if err != nil {
					message = err.Error()
				}
				a.post(func() {
					if err != nil {
						state.failed = message
						return
					}
					state.bitmap = bitmap
				})
			}()
		}
		if bitmap := state.bitmap; bitmap != nil {
			width, height := bitmap.Size()
			dipWidth, dipHeight := float32(width), float32(height)
			const maxWidth = float32(420)
			if dipWidth > maxWidth {
				ratio := maxWidth / dipWidth
				dipWidth, dipHeight = maxWidth, dipHeight*ratio
			}
			ui.Image(c, bitmap).Width(dipWidth).Height(dipHeight)
			return
		}
		if state.failed != "" {
			ui.Text(c, a.tr("chat.imageFailedNamed", name, state.failed)).
				FontSize(12).TextColor(theme.Danger)
			return
		}
		ui.Text(c, a.tr("chat.imageLoading")).FontSize(12).TextColor(theme.TextMuted)
		return
	}
	if url != "" {
		a.renderImage(c, name, url)
		return
	}
	ui.Text(c, a.tr("chat.file", name)).FontSize(12).TextColor(theme.TextMuted)
}
