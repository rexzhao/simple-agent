package main

import (
	"os"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"

	"simple-agent/internal/locale"
	"simple-agent/internal/render"
)

// pasteImages 读取剪贴板里的图片。
//
// mygo 的 `ui.Context.ReadClipboard()` 只返回文本；图片要走 `mygo.Clipboard`：
//   - `Read(transfer.PNG)` 拿位图（截图工具复制的那类）；
//   - `ReadFiles()` 拿"从资源管理器复制的图片文件"的路径，再读文件。
//
// 两者都没有时返回空切片：调用方（Composer）据此**放行** Ctrl/Cmd+V，
// 让编辑器按普通文本粘贴——不能把用户贴文本的动作也吞掉。
//
// 剪贴板访问必须在主线程（Windows OLE 剪贴板对线程有要求），而这个函数就是从
// 输入框的 HandleInput（主线程）里调用的；返回的是字节，后续的写入与解码由
// uiapp 放到后台做。
func pasteImages() ([][]byte, error) {
	var out [][]byte

	// 位图
	if data, err := mygo.Clipboard.ReadFormat(transfer.PNG); err == nil && len(data) > 0 {
		out = append(out, data)
	}

	// 图片文件（资源管理器里 Ctrl+C 一个 png 再 Ctrl+V）
	if paths, err := mygo.Clipboard.ReadFiles(); err == nil {
		for _, path := range paths {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			if sniffed := render.SniffImage(data); sniffed != "" {
				out = append(out, data)
			}
		}
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// pickImageFiles 打开系统文件对话框选图片。
//
// 注意：`mygo.Dialog.Open` **会阻塞到对话框关闭**，所以调用方（uiapp）把它放在
// goroutine 里执行，结果再 post 回主线程。
func pickImageFiles() ([]string, error) {
	return mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:    locale.Tr("composer.attach"),
		Multiple: true,
		Filters: []mygo.FileFilter{{
			Name:       locale.Tr("composer.attachFilter"),
			Extensions: []string{"png", "jpg", "jpeg", "gif", "webp"},
		}},
	})
}

// pickDirectory 打开系统**目录**选择器（添加项目用）。
//
// 与 pickImageFiles 同一套约束：`mygo.Dialog.Open` 阻塞到对话框关闭，
// 调用方（uiapp.addProject）负责放到 goroutine 里。
//
// `CreateDirectories` 让对话框自带「新建文件夹」按钮——对齐 dsh 的设计：
// 添加工作区只有"选一个目录"这一条路，新建目录由系统选择器提供，
// 应用自己不做"新建项目"表单。
func pickDirectory() (string, error) {
	dirs, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:             locale.Tr("project.add"),
		Directory:         true,
		Multiple:          false,
		CreateDirectories: true,
	})
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", nil // 用户取消
	}
	return dirs[0], nil
}
