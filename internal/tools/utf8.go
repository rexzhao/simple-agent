package tools

import (
	"strings"
	"unicode/utf8"
)

// sanitizeUTF8 把不是合法 UTF-8 的字节换成 U+FFFD。
//
// 工具输出**不保证是 UTF-8**：中文 Windows 上读一个 GBK 源文件、搜索命中 GBK
// 内容、或命令吐二进制，都会带出非法字节。这些字节一旦进入事件日志，
// `encoding/json/v2` 会拒绝编码，而写日志用的是 `MustEvent`（panic）——
// 用户实测因此把整个界面线程 panic 掉了。所以在"生成工具输出"这一层就修掉。
func sanitizeUTF8(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	return strings.ToValidUTF8(text, "\uFFFD")
}
