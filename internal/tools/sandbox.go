package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// skipDirs 是遍历时跳过的目录（构建产物与 VCS/工具目录）。
var skipDirs = map[string]bool{
	"target":       true,
	"node_modules": true,
	".git":         true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	".pi":          true,
}

// maxScanFileBytes 决定 grep 是否把文件当文本处理（更大的文件跳过）。
const maxScanFileBytes = 4 << 20

func isWindows() bool { return runtime.GOOS == "windows" }

type dirEntry = os.DirEntry

func osReadDir(dir string) ([]dirEntry, error) { return os.ReadDir(dir) }

// Resolve 把模型给的相对路径解析到会话 cwd 内。
//
// 三道校验：
//  1. 拒绝绝对路径（模型应该用相对路径）；
//  2. 词法规范化后必须仍在工作区内（拦住 `..`）；
//  3. lstat + EvalSymlinks 必须仍在工作区内（拦住链接与 junction）。
func Resolve(tc *ToolContext, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	cwd := tc.Cwd()
	if raw == "" {
		return cwd, nil
	}
	if filepath.IsAbs(raw) {
		return "", fmt.Errorf("拒绝绝对路径：%s（只允许相对路径）", raw)
	}

	joined := normalize(filepath.Join(cwd, raw))
	root := normalize(tc.workspace)
	if !within(joined, root) {
		return "", fmt.Errorf("路径越界：%s", raw)
	}
	if err := checkSymlinkEscape(joined, root, raw); err != nil {
		return "", err
	}
	return joined, nil
}

// normalize 做纯词法规范化（不访问文件系统）。
func normalize(path string) string {
	return filepath.Clean(path)
}

// within 判断 path 是否在 root 之内（大小写按平台处理）。
func within(path, root string) bool {
	if isWindows() {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, prefix)
}

// checkSymlinkEscape 拦住「经符号链接/junction 逃出工作区」。
//
// 做法：找到最深的已存在祖先，canonicalize 它，再把尚未创建的剩余段按词法拼回，
// 要求结果仍在 canonicalize 后的根目录下（写新文件时目标本身不存在，校验父目录）。
//
// 两个关键细节（Rust 阶段 opus 的结论）：
//   - 判断存在用 os.Lstat，**不用 os.Stat**：后者跟随链接，悬空链接会被当成
//     「不存在」而漏检，随后 write 会顺着链接在工作区外建文件；
//   - 已 lstat 到却 EvalSymlinks 失败 → 一律拒绝（悬空链接或权限异常）。
//
// 已知局限：检查与使用之间存在 TOCTOU 窗口。单用户本地工具可接受。
func checkSymlinkEscape(path, root, raw string) error {
	realRoot, err := canonicalExisting(root)
	if err != nil {
		// 根目录尚不存在（还没创建的会话目录）→ 没有可解析的链接
		return nil
	}

	existing := path
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append(rest, filepath.Base(existing))
		existing = parent
	}

	real, err := canonicalExisting(existing)
	if err != nil {
		// 已 lstat 到却无法解析：悬空链接 / 权限异常，宁可拒绝
		return fmt.Errorf("无法解析路径 %s（可能是悬空链接）：%w", existing, err)
	}
	for i := len(rest) - 1; i >= 0; i-- {
		real = filepath.Join(real, rest[i])
	}

	if within(real, realRoot) {
		return nil
	}
	return fmt.Errorf("路径越界：%s 经符号链接/联接解析后指向工作区外（%s）", raw, real)
}

// isTextFile 判断文件是否当作文本处理（超过上限或含 NUL 视为二进制）。
func isTextFile(path string, info os.FileInfo) bool {
	if info.Size() > maxScanFileBytes {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	head := make([]byte, 4096)
	n, _ := file.Read(head)
	for i := 0; i < n; i++ {
		if head[i] == 0 {
			return false
		}
	}
	return true
}
