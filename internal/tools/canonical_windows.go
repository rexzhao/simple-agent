//go:build windows

package tools

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandleW = kernel32.NewProc("GetFinalPathNameByHandleW")
)

// canonicalExisting 返回把所有 reparse point（符号链接与 junction）解析后的真实路径。
//
// 为什么不用 filepath.EvalSymlinks：它在 Windows 上只处理符号链接，**不解析
// junction**（实测对 junction 原样返回 junction 自身路径），而 junction 普通用户
// 就能用 `mklink /J` 创建，正是工作区逃逸的真实攻击面。Rust 的 canonicalize 走
// GetFinalPathNameByHandleW，所以那边没这个洞。
//
// os.Open 能打开目录：Go 的 syscall 只读打开时会加上 FILE_FLAG_BACKUP_SEMANTICS，
// 且不带 FILE_FLAG_OPEN_REPARSE_POINT（只有 O_CREAT|O_EXCL 才带），所以既能打开
// 目录，也会跟随 reparse point。这是 Go 自己设的标志，不依赖 Windows 版本。
//
// 用 VOLUME_NAME_DOS 调用时，只有「卷没有盘符」（挂载在文件夹下）才可能返回
// GLOBALROOT 之类的非 DOS 路径；那种情况我们直接拒绝（返回错误），符合默认拒绝原则。
func canonicalExisting(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	handle := syscall.Handle(file.Fd())
	buf := make([]uint16, 512)
	for {
		n, _, callErr := procGetFinalPathNameByHandleW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			uintptr(0), // VOLUME_NAME_DOS
		)
		if n == 0 {
			return "", fmt.Errorf("GetFinalPathNameByHandle 失败：%v", callErr)
		}
		if int(n) <= len(buf) {
			break
		}
		buf = make([]uint16, n)
	}

	return stripExtendedPrefix(syscall.UTF16ToString(buf))
}

// stripExtendedPrefix 去掉 GetFinalPathNameByHandleW 返回的扩展长度前缀。
//
// 返回值形如 `\\?\C:\dir\file` 或 `\\?\UNC\server\share\file`；前缀必须去掉，
// 否则错误信息里会出现 `\\?\`，而且规范化路径一旦与普通路径比较就会出错。
// 注意 Go 源码里要写两个反斜杠：`\\?\` 是 4 个字符。
func stripExtendedPrefix(path string) (string, error) {
	const (
		uncPrefix = `\\?\UNC\`
		dosPrefix = `\\?\`
	)
	switch {
	case strings.HasPrefix(path, uncPrefix):
		return `\\` + strings.TrimPrefix(path, uncPrefix), nil
	case strings.HasPrefix(path, dosPrefix):
		return strings.TrimPrefix(path, dosPrefix), nil
	case strings.HasPrefix(path, `\\`):
		return path, nil // 已经是普通 UNC 路径
	case strings.HasPrefix(path, `\`):
		return "", fmt.Errorf("无法识别的路径格式：%s", path)
	default:
		return path, nil
	}
}
