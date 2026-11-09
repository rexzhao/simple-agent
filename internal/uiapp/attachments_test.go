package uiapp_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/locale"

	"simple-agent/internal/driver"
	"simple-agent/internal/message"
	"simple-agent/internal/providers"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
	"simple-agent/internal/uiapp"
)

// 这组测试固定「粘贴/拖入图片」的界面行为：
//
//   - Ctrl/Cmd+V 只在剪贴板真有图片时被认领（否则放行给编辑器贴文本）；
//   - 附件先写 blob 存储、再发消息（引用进 JSONL，字节不进）；
//   - 模型不支持图片时禁止发送并提示；
//   - 缩略图/失效附件都能看到并删除。

// pngBytes 返回一张真实的 2×2 PNG（用标准库编码，保证能嗅探与解码）。
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 60, B: 60, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// newAppWithVision 构造一个"支持图片"的界面环境（带临时 blob 存储）。
func newAppWithVision(t *testing.T, vision bool) (*uiapp.App, *fakeRuntime, *poster, *store.Blobs) {
	t.Helper()
	dataDir := t.TempDir()
	blobs, err := store.NewBlobs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	p := &poster{}
	app := uiapp.NewApp(rt, uiapp.Options{
		Selection:                providers.Selection{ProviderID: "fake", Model: "m"},
		SystemPrompt:             "你是助手",
		Workspace:                t.TempDir(),
		Title:                    "测试会话",
		MaxToolRounds:            3,
		ContextWindow:            200000,
		SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
		Language:                 locale.ZH, // 测试固定中文文案
		Post:                     p.Post,
		Blobs:                    blobs,
		VisionFor:                func(providers.Selection) bool { return vision },
		PasteImages:              func() ([][]byte, error) { return nil, nil },
	})
	return app, rt, p, blobs
}

// 粘贴图片：附件出现缩略图，发送后消息里是 blob 引用，JSONL 不存字节。
func TestPasteImageAndSend(t *testing.T) {
	app, rt, p, blobs := newAppWithVision(t, true)
	image := pngBytes(t)
	app.PasteImagesForTest = func() ([][]byte, error) { return [][]byte{image}, nil }

	tester := ui.NewTester(app.View, 900, 700)
	clickComposer(t, tester)
	tester.Type("看这张")
	tester.Key(ui.Cmd, ui.KeyV) // 认领：剪贴板里有图片
	pump(tester, p)

	waitFor(t, tester, p, "附件就绪", func() bool {
		return app.AttachmentsForTest() == 1 && app.AttachmentsReadyForTest()
	})
	// 文本没有被图片粘贴吞掉
	if got := app.DraftForTest(); got != "看这张" {
		t.Fatalf("贴图不应改动文本，实际 %q", got)
	}

	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "请求打开", func() bool { return rt.streamCount() == 1 })

	// 消息里是 blob 引用
	entries := app.Current().Conversation().Entries()
	var imagePart message.Part
	for _, entry := range entries {
		if entry.Message == nil || entry.Message.Message.Role != message.RoleUser {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if view := render.Classify(part); view.Kind == render.KindImage {
				imagePart = part
			}
		}
	}
	if imagePart.Content == nil {
		t.Fatalf("消息里应有图片 part")
	}
	url, ok := imagePart.AsURL()
	if !ok || !strings.HasPrefix(url, "blob:sha256:") {
		t.Fatalf("图片应以 blob 引用保存，实际 %#v", imagePart.Content)
	}
	hash, ok := store.BlobHash(url)
	if !ok || !blobs.Has(hash) {
		t.Fatalf("blob 存储里应有这张图：%q", url)
	}
	// 附件发完就清空
	if got := app.AttachmentsForTest(); got != 0 {
		t.Fatalf("发送后附件应清空，实际 %d", got)
	}
	// 模型支持图片 → 请求里是多模态
	msgs, err := providers.ToOpenAIMessages(rt.requests[0].Messages, blobs.Open)
	if err != nil {
		t.Fatal(err)
	}
	sawArray := false
	for _, msgMap := range msgs {
		if _, ok := msgMap["content"].([]any); ok {
			sawArray = true
		}
	}
	if !sawArray {
		t.Fatalf("请求里应带图片：%#v", msgs)
	}
}

// 剪贴板里没有图片时，Ctrl+V 放行给编辑器（不影响文本粘贴）。
func TestPasteWithoutImageFallsThrough(t *testing.T) {
	app, _, p, _ := newAppWithVision(t, true)
	tester := ui.NewTester(app.View, 900, 700)
	clickComposer(t, tester)
	pump(tester, p)

	tester.SetClipboard("剪贴板文本")
	tester.Key(ui.Cmd, ui.KeyV)
	pump(tester, p)
	if got := app.DraftForTest(); got != "剪贴板文本" {
		t.Fatalf("没有图片时应按文本粘贴，实际 %q", got)
	}
	if got := app.AttachmentsForTest(); got != 0 {
		t.Fatalf("不该产生附件，实际 %d", got)
	}
}

// 模型不支持图片：附件在时禁止发送，并给出提示。
func TestImageBlockedWhenModelCannotSee(t *testing.T) {
	app, _, p, _ := newAppWithVision(t, false)
	app.PasteImagesForTest = func() ([][]byte, error) { return [][]byte{pngBytes(t)}, nil }

	tester := ui.NewTester(app.View, 900, 700)
	clickComposer(t, tester)
	tester.Type("看这张")
	tester.Key(ui.Cmd, ui.KeyV)
	pump(tester, p)
	waitFor(t, tester, p, "附件就绪", func() bool {
		return app.AttachmentsForTest() == 1 && app.AttachmentsReadyForTest()
	})

	if !app.SendDisabledForTest() {
		t.Fatal("模型不支持图片时发送按钮应禁用")
	}
	if !tester.HasText("当前模型不支持图片") {
		t.Fatalf("应提示不支持图片：%v", tester.Texts())
	}

	// 移除附件后恢复可发送
	if err := tester.Click("×"); err != nil {
		t.Fatalf("点不到删除：%v", err)
	}
	pump(tester, p)
	if app.AttachmentsForTest() != 0 {
		t.Fatalf("附件应被删除，实际 %d", app.AttachmentsForTest())
	}
	if app.SendDisabledForTest() && strings.TrimSpace(app.DraftForTest()) != "" {
		t.Fatal("移除附件后应可发送")
	}
}

// 拖入图片文件：读取在后台，就绪后与粘贴一样发送。
func TestDropImageFile(t *testing.T) {
	app, _, p, blobs := newAppWithVision(t, true)
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, pngBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 700)
	app.DropFilesForTest([]string{path})
	pump(tester, p)
	waitFor(t, tester, p, "拖入的图片就绪", func() bool {
		return app.AttachmentsForTest() == 1 && app.AttachmentsReadyForTest()
	})
	if err := tester.Click(locSend); err != nil {
		t.Fatalf("空文本 + 附件也应能发送：%v", err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "打开请求", func() bool { return app.Current().Busy() != driver.BusyIdle })
	entries := app.Current().Conversation().Entries()
	if len(entries) == 0 {
		t.Fatal("应产生一条用户消息")
	}
	for _, entry := range entries {
		if entry.Message == nil {
			continue
		}
		for _, part := range entry.Message.Message.Parts {
			if url, ok := part.AsURL(); ok {
				if hash, ok := store.BlobHash(url); !ok || !blobs.Has(hash) {
					t.Fatalf("拖入的图片应写进 blob 存储：%q", url)
				}
			}
		}
	}
}

// 不支持的格式：给提示，不进附件（不静默忽略）。
func TestDropUnsupportedFileShowsError(t *testing.T) {
	app, _, p, _ := newAppWithVision(t, true)
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("不是图片"), 0o644); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 900, 700)
	app.DropFilesForTest([]string{path})
	pump(tester, p)
	waitFor(t, tester, p, "出现失败提示", func() bool {
		return tester.HasText("不支持的图片格式")
	})
}

// blob 丢失：界面显示「[图片丢失]」，请求里替换成文字，不阻塞发送。
func TestMissingBlobShowsPlaceholder(t *testing.T) {
	app, rt, p, blobs := newAppWithVision(t, true)
	hash, err := blobs.Put(pngBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobs.Path(hash)); err != nil {
		t.Fatal(err)
	}
	// 直接塞一条带该引用的用户消息
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{
			message.NewTextPart("之前那张图"),
			message.NewURLPart(store.BlobURL(hash)).WithMediaType("image/png"),
		},
	}, "complete"); err != nil {
		t.Fatal(err)
	}

	tester := ui.NewTester(app.View, 900, 700)
	// 读取在后台 goroutine（post 回主线程），等它落地
	waitFor(t, tester, p, "显示图片丢失占位", func() bool {
		return tester.HasText("[图片丢失]")
	})

	// 发送时替换成文字，不报错
	clickComposer(t, tester)
	tester.Type("继续")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "打开请求", func() bool { return rt.streamCount() == 1 })
	msgs, err := providers.ToOpenAIMessages(rt.requests[0].Messages, func(string) ([]byte, error) {
		return nil, errors.New("missing")
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msgMap := range msgs {
		switch content := msgMap["content"].(type) {
		case string:
			found = found || strings.Contains(content, "[图片丢失]")
		case []any:
			for _, item := range content {
				if block, ok := item.(map[string]any); ok && block["text"] == "[图片丢失]" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("请求里应替换成 [图片丢失]：%#v", msgs)
	}
}

// 切到不支持图片的模型时，历史里的图片被替换，并给出一次性提示。
func TestHistoryImagesNoticeWhenModelChanges(t *testing.T) {
	app, rt, p, blobs := newAppWithVision(t, true)
	hash, err := blobs.Put(pngBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Current().Conversation().AddMessage(message.Message{
		MessageID: "u1", Role: message.RoleUser,
		Parts: []message.Part{
			message.NewTextPart("看图"),
			message.NewURLPart(store.BlobURL(hash)).WithMediaType("image/png"),
		},
	}, "complete"); err != nil {
		t.Fatal(err)
	}
	// 关掉图片支持（模拟切到不支持图片的模型）
	app.SetVisionForTest(false)

	tester := ui.NewTester(app.View, 900, 700)
	clickComposer(t, tester)
	tester.Type("描述一下")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	waitFor(t, tester, p, "打开请求", func() bool { return rt.streamCount() == 1 })
	if !tester.HasText("[图片]") {
		t.Fatalf("应提示图片被替换：%v", tester.Texts())
	}
}

// P1：恢复会话时必须按**重放得到的模型**同步窗口 / 输出上限 / vision / 档位，
// 而不是默认模型的值（否则带图发出去会 400、压缩阈值也会用错）。
func TestRestoredSessionUsesReplayedModelOptions(t *testing.T) {
	dataDir := t.TempDir()
	catalog := store.NewCatalog(dataDir)
	const (
		defaultModel = "vision-model"
		savedModel   = "text-only-model"
	)
	makeApp := func(rt *fakeRuntime) (*uiapp.App, *poster) {
		p := &poster{}
		app := uiapp.NewApp(rt, uiapp.Options{
			Selection:                providers.Selection{ProviderID: "fake", Model: defaultModel},
			Workspace:                t.TempDir(),
			SidebarAutoCollapseWidth: -1,        // 测试窗口较窄：禁用自动收起
			Language:                 locale.ZH, // 测试固定中文文案
			Post:                     p.Post,
			Catalog:                  catalog,
			ContextWindow:            100_000,
			WindowFor: func(sel providers.Selection) int {
				if sel.Model == savedModel {
					return 4_000
				}
				return 100_000
			},
			VisionFor: func(sel providers.Selection) bool { return sel.Model != savedModel },
		})
		return app, p
	}

	// 第一次运行：把选择切到"不支持图片"的模型，并发一轮对话（选择会进日志）
	rt := &fakeRuntime{}
	app, p := makeApp(rt)
	tester := ui.NewTester(app.View, 900, 700)
	pump(tester, p)
	ensureSession(t, tester, p, app)
	app.SelectModelForTest(providers.Selection{ProviderID: "fake", Model: savedModel})
	pump(tester, p)
	if app.Current().VisionForTest() {
		t.Fatal("前置条件失败：切到 text-only 后应不支持图片")
	}
	clickComposer(t, tester)
	tester.Type("用这个模型说一句")
	pump(tester, p)
	if err := tester.Click(locSend); err != nil {
		t.Fatal(err)
	}
	pump(tester, p)
	rt.push(t,
		providers.Event{Kind: providers.EventText, Text: "好"},
		providers.Event{Kind: providers.EventDone, StopReason: "stop"},
	)
	waitFor(t, tester, p, "回合结束", func() bool {
		return app.Current().Busy() == driver.BusyIdle
	})
	app.Close()

	// 重启：默认模型支持图片，但会话里记的是 text-only
	app2, p2 := makeApp(&fakeRuntime{})
	defer app2.Close()
	tester2 := ui.NewTester(app2.View, 900, 700)
	pump(tester2, p2)
	session := app2.Current()
	if session == nil {
		t.Fatalf("应恢复会话：%v", tester2.Texts())
	}
	if session.Selection().Model != savedModel {
		t.Fatalf("应恢复成日志里的模型，实际 %q", session.Selection().Model)
	}
	if session.VisionForTest() {
		t.Fatal("恢复后应沿用该模型（不支持图片），不能按默认模型算")
	}
	if got := session.ContextWindowForTest(); got != 4_000 {
		t.Fatalf("恢复后的窗口应是该模型配置值 4000，实际 %d", got)
	}
}

// P1：按 Enter 发送失败时（模型不支持图片），草稿与附件都不能丢。
func TestEnterKeepsAttachmentsWhenUnsupported(t *testing.T) {
	app, rt, p, _ := newAppWithVision(t, false)
	app.PasteImagesForTest = func() ([][]byte, error) { return [][]byte{pngBytes(t)}, nil }

	tester := ui.NewTester(app.View, 900, 700)
	clickComposer(t, tester)
	tester.Type("这张图讲了什么")
	tester.Key(ui.Cmd, ui.KeyV)
	pump(tester, p)
	waitFor(t, tester, p, "附件就绪", func() bool {
		return app.AttachmentsForTest() == 1 && app.AttachmentsReadyForTest()
	})

	// Enter 绕过按钮禁用 → 必须被拦下且**什么都不清**
	tester.Key(0, ui.KeyEnter)
	pump(tester, p)

	if got := app.DraftForTest(); got != "这张图讲了什么" {
		t.Fatalf("发送失败后草稿应保留，实际 %q", got)
	}
	if got := app.AttachmentsForTest(); got != 1 {
		t.Fatalf("发送失败后附件应保留，实际 %d", got)
	}
	if rt.streamCount() != 0 {
		t.Fatalf("不该开流，实际 %d", rt.streamCount())
	}
	if !tester.HasText("当前模型不支持图片") {
		t.Fatalf("应给出提示：%v", tester.Texts())
	}
}

// P2：插话状态下带附件时不入队，提示用户，草稿与附件都保留。
func TestSteerRejectedWithAttachments(t *testing.T) {
	app, rt, p, _ := newAppWithVision(t, true)
	app.PasteImagesForTest = func() ([][]byte, error) { return [][]byte{pngBytes(t)}, nil }

	tester := ui.NewTester(app.View, 900, 700)
	sendFirst(t, tester, rt, p, "先开始")
	clickComposer(t, tester)
	tester.Type("顺便加张图")
	tester.Key(ui.Cmd, ui.KeyV)
	pump(tester, p)
	waitFor(t, tester, p, "附件就绪", func() bool {
		return app.AttachmentsForTest() == 1 && app.AttachmentsReadyForTest()
	})

	tester.Key(0, ui.KeyEnter)
	pump(tester, p)
	if got := app.Current().QueuedForTest(); len(got) != 0 {
		t.Fatalf("插话不该入队：%#v", got)
	}
	if got := app.DraftForTest(); got != "顺便加张图" {
		t.Fatalf("草稿应保留，实际 %q", got)
	}
	if got := app.AttachmentsForTest(); got != 1 {
		t.Fatalf("附件应保留，实际 %d", got)
	}
	if !tester.HasText("插话暂不支持附带图片") {
		t.Fatalf("应给出提示：%v", tester.Texts())
	}
}

// 附件还在读取（pending）时按 Enter：不能毫无反应，要提示"请稍等"。
func TestEnterWhileAttachmentPendingShowsHint(t *testing.T) {
	app, rt, p, _ := newAppWithVision(t, true)
	app.AddPendingAttachmentForTest("大图.png")

	tester := ui.NewTester(app.View, 900, 700)
	clickComposer(t, tester)
	tester.Type("等一下这张图")
	pump(tester, p)
	tester.Key(0, ui.KeyEnter)
	pump(tester, p)

	if rt.streamCount() != 0 {
		t.Fatalf("附件没就绪时不该开流，实际 %d", rt.streamCount())
	}
	if got := app.DraftForTest(); got != "等一下这张图" {
		t.Fatalf("草稿应保留，实际 %q", got)
	}
	if got := app.AttachmentsForTest(); got != 1 {
		t.Fatalf("附件应保留，实际 %d", got)
	}
	if !tester.HasText("图片还没准备好") {
		t.Fatalf("应提示请稍等：%v", tester.Texts())
	}
}
