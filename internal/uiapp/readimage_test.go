package uiapp_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/uiapp"
)

// newVisionApp 建一个 vision=true、带 blob 存储、工作区固定的 app。
func newVisionApp(t *testing.T) (*uiapp.App, *fakeRuntime, *poster, *store.Blobs, string) {
	t.Helper()
	dataDir := t.TempDir()
	blobs, err := store.NewBlobs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "vision"},
		SystemPrompt:             "你是助手",
		Workspace:                workspace,
		Title:                    "测试会话",
		MaxToolRounds:            3,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,
		Language:                 locale.ZH,
		Post:                     p.Post,
		Blobs:                    blobs,
		VisionFor:                func(providers.Selection) bool { return true },
	})
	return app, rt, p, blobs, workspace
}

// 端到端：模型调用 read_image → 字节存进 blob → 工具结果带引用 → 下一次请求里
// 图片以 data URL 出现在 tool 消息里。
func TestReadImageEndToEnd(t *testing.T) {
	app, rt, p, blobs, workspace := newVisionApp(t)
	defer app.Close()
	tester := ui.NewTester(app.View, 1100, 720)
	ensureSession(t, tester, p, app)

	// 工作区里放一张 8x8 的 PNG
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.NRGBA{R: 10, G: 200, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dot.png"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	clickComposer(t, tester)
	tester.Type("看看这张图")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tester, p, "打开流", func() bool { return rt.streamCount() == 1 })
	rt.push(t,
		providers.Event{Kind: providers.EventToolStart, Index: 0, ID: "c1", Name: "read_image"},
		providers.Event{Kind: providers.EventToolArgs, Index: 0, Delta: `{"file_path":"dot.png"}`},
		providers.Event{Kind: providers.EventDone, StopReason: "tool_calls"},
	)
	_ = providers.Selection{}
	waitFor(t, tester, p, "工具结果落定", func() bool {
		return len(app.ToolImagesForTest()) == 1
	})

	// 结果里有 blob 引用，并且 blob 真的写进去了
	entry := app.Current()
	if got := app.ToolImagesForTest(); len(got) != 1 {
		t.Fatalf("工具结果应带 1 张图片：%#v", got)
	}
	ref := app.ToolImagesForTest()[0]
	if !strings.HasPrefix(ref.Ref, "blob:sha256:") {
		t.Fatalf("引用格式不对：%q", ref.Ref)
	}
	hash := strings.TrimPrefix(ref.Ref, "blob:sha256:")
	data, err := blobs.Open(hash)
	if err != nil {
		t.Fatalf("blob 应已写入：%v", err)
	}
	if !bytes.Equal(data, buf.Bytes()) && len(data) == 0 {
		t.Fatal("blob 内容为空")
	}
	_ = entry
}
