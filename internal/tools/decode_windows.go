//go:build windows

package tools

import (
	"bytes"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	decodeKernel32          = syscall.NewLazyDLL("kernel32.dll")
	procMultiByteToWideChar = decodeKernel32.NewProc("MultiByteToWideChar")
)

// cpOEM 是系统 OEM 代码页（中文系统为 936/GBK）。
const cpOEM = 1

// decodeConsole 把命令输出解码成 UTF-8。
//
// 为什么需要这一步：命令输出走的是**控制台代码页**，不是 UTF-8。中文系统上
// `echo 中文` 重定向到文件得到的是 GBK 字节；而 `chcp 65001` 在没有控制台的
// GUI 进程里不生效（实测子进程仍按 OEM 代码页输出）。
//
// **逐行判断**而不是整体判断：同一份输出里可能混着 UTF-8 与 GBK
// （`git log` 吐 UTF-8、`dir` 吐 GBK）。整体按 UTF-8 校验不通过就把全文按 GBK
// 解码，会把本来就正确的 UTF-8 部分变成乱码。所以只有**不是合法 UTF-8 的行**
// 才按 OEM 代码页转换。
func decodeConsole(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	// 去掉截断点上的半个字符，再逐行判断
	data = trimPartialUTF8(data)

	if utf8.Valid(data) {
		return string(data)
	}

	// 按 \n 切行，保留行尾的 \r，拼接时原样还原
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if utf8.Valid(line) {
			continue
		}
		lines[i] = []byte(decodeOEM(line))
	}
	return string(bytes.Join(lines, []byte("\n")))
}

// decodeOEM 按 OEM 代码页把字节转成 UTF-8。
func decodeOEM(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	need, _, _ := procMultiByteToWideChar.Call(
		uintptr(cpOEM), 0,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		0, 0,
	)
	if need == 0 {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	buf := make([]uint16, need)
	written, _, _ := procMultiByteToWideChar.Call(
		uintptr(cpOEM), 0,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
	)
	if written == 0 {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return string(utf16.Decode(buf[:written]))
}

// trimPartialUTF8 去掉末尾被截断的半个 UTF-8 字符（最多 3 字节）。
func trimPartialUTF8(data []byte) []byte {
	for i := 0; i < 4 && len(data) > 0; i++ {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size > 1 {
			return data
		}
		data = data[:len(data)-1]
	}
	return data
}
