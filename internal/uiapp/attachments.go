package uiapp

import (
	"os"
	"strings"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/message"
	"simple-agent/internal/render"
	"simple-agent/internal/store"
)

// 附件（待发送的图片）：粘贴或拖入得到字节 → 后台写入 blob 存储 → 发送时只带引用。
//
// 约束（方案 §12 第 26–28 条）：
//   - **先 Put 成功、再写引用它的事件**：所以发送按钮在附件还没就绪时禁用；
//   - 读文件/写入存储/解码缩略图都在后台 goroutine，结果经 post 回主线程；
//   - 只收 png/jpeg/gif/webp（按内容嗅探），其他格式给提示而不是静默忽略。

// attachmentStatus 是附件的状态。
type attachmentStatus int

const (
	// attachPending 表示正在后台写入 blob 存储。
	attachPending attachmentStatus = iota
	// attachReady 表示已经拿到 blob hash，可以发送。
	attachReady
	// attachFailed 表示读取或写入失败（附件的 err 里有原因）。
	attachFailed
)

// attachment 是一条待发送的图片。
type attachment struct {
	status    attachmentStatus
	hash      string
	mediaType string
	name      string
	err       string
	// thumb 是缩略图（主线程解码后赋值，用于输入框上方的预览）。
	thumb *ui.Bitmap
}

// supportedImageTypes 是可发送的图片类型（按内容判断，不看扩展名）。
var supportedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// attachImages 把一段段字节挂成附件（粘贴位图走这里）。
//
// 嗅探与写入都在后台；不支持的格式、写入失败都会在附件上留下失败原因。
func (a *App) attachImages(session *Session, items [][]byte) {
	if session == nil || len(items) == 0 {
		return
	}
	blobs := a.blobs()
	// 附件条出现会改变元素树、抢走焦点：下一帧把焦点还给输入框
	a.refocusComposer = true
	for _, data := range items {
		if len(data) == 0 {
			continue
		}
		entry := &attachment{status: attachPending, name: a.tr("attach.pasted")}
		session.attachments = append(session.attachments, entry)
		go func(data []byte, entry *attachment) {
			mediaType := render.SniffImage(data)
			if !supportedImageTypes[mediaType] {
				a.post(func() {
					entry.status = attachFailed
					entry.err = a.tr("attach.unsupported")
				})
				return
			}
			if blobs == nil {
				a.post(func() {
					entry.status = attachFailed
					entry.err = a.tr("attach.noStore")
				})
				return
			}
			hash, err := blobs.Put(data)
			if err != nil {
				a.post(func() {
					entry.status = attachFailed
					entry.err = err.Error()
				})
				return
			}
			thumb, thumbErr := decodeImageBytes(data)
			a.post(func() {
				entry.status = attachReady
				entry.hash = hash
				entry.mediaType = mediaType
				if thumbErr == nil {
					entry.thumb = thumb
				}
			})
		}(data, entry)
	}
}

// attachFiles 读取拖入/粘贴得到的图片文件（读取在后台，不阻塞界面）。
func (a *App) attachFiles(session *Session, paths []string) {
	if session == nil || len(paths) == 0 {
		return
	}
	a.refocusComposer = true
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			session.attachments = append(session.attachments, &attachment{
				status: attachFailed,
				name:   baseName(path),
				err:    a.tr("attach.notFile"),
			})
			continue
		}
		entry := &attachment{status: attachPending, name: baseName(path)}
		session.attachments = append(session.attachments, entry)
		a.readImageFile(session.ID, path, entry)
	}
}

// readImageFile 在后台读文件，然后按内容判断格式并写入 blob 存储。
func (a *App) readImageFile(sessionID, path string, entry *attachment) {
	blobs := a.blobs()
	go func() {
		data, err := os.ReadFile(path)
		if err != nil {
			a.post(func() {
				entry.status = attachFailed
				entry.err = a.tr("attach.readFailed", err.Error())
			})
			return
		}
		mediaType := render.SniffImage(data)
		if !supportedImageTypes[mediaType] {
			a.post(func() {
				entry.status = attachFailed
				entry.err = a.tr("attach.unsupported")
			})
			return
		}
		if blobs == nil {
			a.post(func() {
				entry.status = attachFailed
				entry.err = a.tr("attach.noStore")
			})
			return
		}
		hash, err := blobs.Put(data)
		if err != nil {
			a.post(func() {
				entry.status = attachFailed
				entry.err = err.Error()
			})
			return
		}
		thumb, thumbErr := decodeImageBytes(data)
		a.post(func() {
			entry.status = attachReady
			entry.hash = hash
			entry.mediaType = mediaType
			if thumbErr == nil {
				entry.thumb = thumb
			}
		})
	}()
}

// baseName 返回路径的最后一段（Windows 与 POSIX 分隔符都认）。
func baseName(path string) string {
	if index := strings.LastIndexAny(path, "/\\"); index >= 0 && index+1 < len(path) {
		return path[index+1:]
	}
	return path
}

// removeAttachment 移除第 index 个附件。
func (a *App) removeAttachment(session *Session, index int) {
	if session == nil || index < 0 || index >= len(session.attachments) {
		return
	}
	session.attachments = append(session.attachments[:index], session.attachments[index+1:]...)
}

// attachmentParts 把已就绪的附件转成消息 part（blob 引用），并清空附件列表。
//
// 有附件还没就绪（或失败）时返回 ok=false：调用方应禁止发送——先 Put 成功、
// 再写引用它的事件，这是崩溃后不留下悬空引用的前提。
// 注意：它**不清理**附件列表——发送成功后由调用方清空（失败时内容必须留着）。
func (a *App) attachmentParts(session *Session) ([]message.Part, bool) {
	if session == nil || len(session.attachments) == 0 {
		return nil, true
	}
	parts := make([]message.Part, 0, len(session.attachments))
	for _, entry := range session.attachments {
		if entry.status != attachReady {
			return nil, false
		}
		parts = append(parts, message.NewURLPart(store.BlobURL(entry.hash)).
			WithMediaType(entry.mediaType).WithFilename(entry.name))
	}
	return parts, true
}

// attachmentsReady 报告附件是否可以发送（没有附件或全部就绪）。
func attachmentsReady(session *Session) bool {
	for _, entry := range session.attachments {
		if entry.status != attachReady {
			return false
		}
	}
	return true
}

// pasteImages 处理 Ctrl/Cmd+V：剪贴板里没有图片就返回 false（放行给编辑器）。
//
// 剪贴板读取在**主线程**（Windows OLE 剪贴板要求），字节的写入与解码在后台。
func (a *App) pasteImages(session *Session) bool {
	paste := a.opts.PasteImages
	if a.PasteImagesForTest != nil {
		paste = a.PasteImagesForTest
	}
	if paste == nil {
		return false
	}
	items, err := paste()
	if err != nil || len(items) == 0 {
		return false
	}
	a.attachImages(session, items)
	return true
}

// attachmentsPanel 渲染输入框上方的待发送图片（缩略图 + 删除；失败态可见）。
func (a *App) attachmentsPanel(c *ui.Context, session *Session) {
	theme := c.Theme()
	ui.Row(c).Gap(8).Wrap().Children(func() {
		for index, entry := range session.attachments {
			index := index
			entry := entry
			ui.Column(c).Gap(2).Children(func() {
				switch entry.status {
				case attachReady:
					if entry.thumb != nil {
						ui.Image(c, entry.thumb).Width(56).Height(56).Radius(6)
					} else {
						ui.Text(c, a.tr("attach.image")).FontSize(11).TextColor(theme.TextMuted)
					}
				case attachPending:
					ui.Box(c).Width(56).Height(56).Radius(6).Background(theme.Surface)
					ui.Text(c, a.tr("attach.reading")).FontSize(10).TextColor(theme.TextMuted)
				default:
					ui.Box(c).Width(56).Height(56).Radius(6).Background(theme.Surface).
						Border(1, theme.Danger)
					ui.Text(c, a.tr("attach.failed")).FontSize(10).TextColor(theme.Danger)
				}
				ui.Row(c).Gap(4).Children(func() {
					ui.Text(c, clipLine(entry.name, 12)).FontSize(10).
						TextColor(theme.TextMuted).MaxLines(1)
					if textButton(c, "×", 11, theme.TextMuted) {
						a.removeAttachment(session, index)
					}
				})
			})
		}
	})
	// 失败原因单独一行（缩略图上的「失败」放不下原因）
	for _, entry := range session.attachments {
		if entry.status == attachFailed {
			ui.Text(c, entry.name+"："+entry.err).FontSize(11).TextColor(theme.Danger)
		}
	}
}
