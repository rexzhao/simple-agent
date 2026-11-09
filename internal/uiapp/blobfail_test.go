package uiapp_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/uiapp"
)

// failingBlobService 模拟"上传到执行端失败"（断线 / 磁盘满 / 校验不过）。
type failingBlobService struct{ err error }

func (f failingBlobService) Put([]byte) (string, error)  { return "", f.err }
func (f failingBlobService) Open(string) ([]byte, error) { return nil, f.err }
func (f failingBlobService) Has(string) bool             { return false }

// countingHost 包住真实 host，只替换 blob 服务并数一数 send 命令。
type countingHost struct {
	host.Host
	blobs   host.BlobService
	sends   int
	submits int
}

func (h *countingHost) Blobs() host.BlobService { return h.blobs }

func (h *countingHost) Submit(cmd host.Command) (string, error) {
	h.submits++
	if cmd.Kind == "send" {
		h.sends++
	}
	return h.Host.Submit(cmd)
}

// 上传失败（断线/执行端拒绝）时：附件必须变成失败态，发送被挡住——不能出现
// "事件引用了执行端并不存在的 blob、模型看到[图片丢失]"。
func TestAttachmentUploadFailureBlocksSend(t *testing.T) {
	pngData := func() []byte {
		img := image.NewRGBA(image.Rect(0, 0, 4, 4))
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				img.Set(x, y, color.RGBA{R: 200, G: 10, B: 10, A: 255})
			}
		}
		var buf bytes.Buffer
		_ = png.Encode(&buf, img)
		return buf.Bytes()
	}()

	app, _, poster := newAppWithOptions(t, func(opts *uiapp.Options) {
		opts.Language = locale.ZH
	})
	local := app.Host()
	counting := &countingHost{Host: local, blobs: failingBlobService{err: errors.New("连接中断，图片传输中止")}}
	app.SetHostForTest(counting)

	app.AttachImageBytesForTest(pngData)
	app.SetDraftForTest("看看这张图")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		poster.Drain()
		if !app.AttachmentsReadyForTest() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if app.AttachmentsReadyForTest() {
		t.Fatal("上传失败后附件不该还在读")
	}

	// Enter 路径：附件没就绪 → 不发命令、草稿保留
	app.Send()
	time.Sleep(50 * time.Millisecond)
	if counting.sends != 0 {
		t.Fatalf("上传失败时不该发出 send，实际 %d 次", counting.sends)
	}
	if app.AttachmentsForTest() != 1 {
		t.Fatal("失败附件应保留在输入卡上（用户能删掉重试）")
	}
}
