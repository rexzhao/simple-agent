package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// read 的返回上限。
const (
	maxReadLines     = 2000
	maxReadLineChars = 2000
	maxReadBytes     = 200_000
)

func readTool() *ToolDef {
	return &ToolDef{
		Name: "read",
		Description: "Read a text file and return line-numbered content. " +
			"Binary files (images, executables) are refused; images must be attached in the composer instead.",
		Guidance: "Use the read tool — not shell commands like cat — to inspect text files. " +
			"Use offset and limit to continue reading large files.",
		Parameters: schema(map[string]any{
			"file_path": map[string]any{"type": "string", "description": "Path to read, relative to the session working directory."},
			"offset":    map[string]any{"type": "number", "description": "1-based first line to return. Defaults to 1."},
			"limit":     map[string]any{"type": "number", "description": fmt.Sprintf("Maximum number of lines to return. Defaults to %d.", maxReadLines)},
		}, "file_path"),
		Run: readRun,
	}
}

func readRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	raw, err := args.Str("file_path")
	if err != nil {
		return ToolReturn{}, err
	}
	path, err := Resolve(tc, raw)
	if err != nil {
		return ToolReturn{}, err
	}
	if err := ctx.Err(); err != nil {
		return ToolReturn{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("读取失败 %s: %w", displayPath(tc, path), err)
	}
	// UTF-16 文本（Windows 的 .reg、部分 .txt/日志）几乎每个字符都带 NUL，会被
	// 二进制判定误伤；按 BOM 先解码成 UTF-8。UTF-8 的 BOM 顺手剥掉。
	data, utf16 := decodeBOMText(data)

	// 二进制文件直接拒绝（与 dsh 的 `readWholeText` 同策略：前 8KB 里出现 NUL
	// 即判为二进制）。用户的真实崩溃就是 `read` 读了一个 PNG：整段二进制被当成
	// 文本行返回，既没有意义，又把非法字节带进了事件日志。
	if isBinarySample(data) {
		return ToolReturn{}, fmt.Errorf(
			"无法读取 %s：二进制文件（不是文本）。如需查看图片，请在输入框粘贴或拖入图片作为附件。",
			displayPath(tc, path))
	}

	offset := 1
	if v, ok := args.Int("offset"); ok && v > 1 {
		offset = v
	}
	limit := maxReadLines
	if v, ok := args.Int("limit"); ok {
		if v < 1 {
			v = 1
		}
		if v > maxReadLines {
			v = maxReadLines
		}
		limit = v
	}

	// 文件不一定是 UTF-8（中文 Windows 上 GBK 很常见）：把无法识别的字节换成
	// U+FFFD，否则写日志时 json/v2 会拒绝编码（用户实测把界面线程 panic 掉了）。
	// 非 UTF-8 的文件会被按系统代码页解码显示；要告诉模型（和用户）这一点，
	// 否则它用 write 整体重写时会把文件悄悄变成 UTF-8。
	notUTF8 := !utf8.Valid(data) && !utf16
	lines := splitLines(decodeConsole(data))
	total := len(lines)
	var out strings.Builder
	emitted := 0
	var startLine int

	for index, line := range lines {
		number := index + 1
		if number < offset {
			continue
		}
		if emitted >= limit {
			break
		}
		if startLine == 0 {
			startLine = number
		}
		if runes := []rune(line); len(runes) > maxReadLineChars {
			line = string(runes[:maxReadLineChars]) + "…（行已截断）"
		}
		fmt.Fprintf(&out, "%6d→%s\n", number, line)
		emitted++
		if out.Len() >= maxReadBytes {
			out.WriteString("…（已达字节上限，请用 offset 继续读取）\n")
			break
		}
	}

	if emitted == 0 {
		return OutputOf(fmt.Sprintf("（文件共 %d 行，offset=%d 超出范围）", total, offset)), nil
	}
	if last := startLine + emitted - 1; last < total {
		fmt.Fprintf(&out, "…（共 %d 行，已返回第 %d-%d 行；用 offset=%d 继续）", total, startLine, last, last+1)
	}
	output := out.String()
	if utf16 {
		output = readUTF16Note + output
	}
	if notUTF8 {
		// 把编码这件事摆在最前面：模型据此才知道用 write 重写会改变文件编码
		output = readNonUTF8Note + output
	}
	return OutputOf(output), nil
}

// splitLines 按 \n 切行（保留最后一段，去掉行尾 \r）。
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	// 末尾换行产生的空元素不算一行
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// readNonUTF8Note 是 read 遇到非 UTF-8 文件时加在输出开头的提示。
//
// 为什么要显式说出来：`read` 会按系统代码页解码（中文 Windows = GBK）后以 UTF-8
// 给模型看；模型若用 `write` 整体重写这个文件，文件就会从 GBK 变成 UTF-8——
// 对某些老项目这是破坏性的。v1 不拦截这种行为，但至少让模型和用户知情。
// binarySampleBytes 是判定二进制时检查的前缀长度（与 dsh 的
// `BINARY_SAMPLE_BYTES = 8192` 一致）。
const binarySampleBytes = 8192

// isUTF16 报告数据是否以 UTF-16 的 BOM 开头（LE `FF FE` / BE `FE FF`）。
func isUTF16(data []byte) bool {
	return bytes.HasPrefix(data, []byte{0xFF, 0xFE}) || bytes.HasPrefix(data, []byte{0xFE, 0xFF})
}

// decodeBOMText 按 BOM 把 UTF-16 文本转成 UTF-8，并剥掉 UTF-8 的 BOM。
//
// 返回第二项表示"这是一份 UTF-16 文件，已被转换"。UTF-16 几乎每个 ASCII 字符都
// 带一个 NUL 字节，不先转换就会被 isBinarySample 当成二进制拒绝。
func decodeBOMText(data []byte) ([]byte, bool) {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}) && !bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0x00, 0x00}):
		return encodeUTF16(utf16BytesToRunes(data[2:], true)), true
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return encodeUTF16(utf16BytesToRunes(data[2:], false)), true
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return data[3:], false
	default:
		return data, false
	}
}

// utf16BytesToRunes 把 UTF-16 字节（含代理对）解成 rune 序列。
func utf16BytesToRunes(data []byte, littleEndian bool) []rune {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		if littleEndian {
			units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
		} else {
			units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
		}
	}
	return utf16.Decode(units)
}

// encodeUTF16 把 rune 序列编回 UTF-8 字节。
func encodeUTF16(runes []rune) []byte {
	return []byte(string(runes))
}

// isBinarySample 报告数据前缀里是否出现 NUL 字节。
//
// 文本文件不会含 NUL（UTF-8、GBK 都不会）；PNG/JPEG/可执行文件等几乎必在前 8KB
// 内出现 NUL。这是 dsh 的判定方式，简单可靠，不需要嗅探具体文件类型。
func isBinarySample(data []byte) bool {
	if len(data) > binarySampleBytes {
		data = data[:binarySampleBytes]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// readUTF16Note 是 read 遇到 UTF-16 文件时加在输出开头的提示。
const readUTF16Note = "（注意：文件是 UTF-16 编码，已转换为 UTF-8 显示；用 write 写回会变成 UTF-8）\n"

const readNonUTF8Note = "（注意：文件不是 UTF-8，已按系统代码页解码显示；用 write 写回会变成 UTF-8）\n"

func writeTool() *ToolDef {
	return &ToolDef{
		Name: "write",
		Description: "Create or fully replace a text file. Content is always written as UTF-8 " +
			"(rewriting an existing file in another encoding, e.g. GBK, converts it to UTF-8).",
		Guidance: "Read an existing file before overwriting it with write " +
			"and prefer edit for targeted changes.",
		Parameters: schema(map[string]any{
			"file_path": map[string]any{"type": "string", "description": "Path to write, relative to the session working directory."},
			"content":   map[string]any{"type": "string", "description": "Full text content to write; stored as UTF-8."},
		}, "file_path", "content"),
		Run: writeRun,
	}
}

func writeRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	raw, err := args.Str("file_path")
	if err != nil {
		return ToolReturn{}, err
	}
	content, err := args.Str("content")
	if err != nil {
		return ToolReturn{}, err
	}
	path, err := Resolve(tc, raw)
	if err != nil {
		return ToolReturn{}, err
	}
	if err := ctx.Err(); err != nil {
		return ToolReturn{}, err
	}

	if parent := filepath.Dir(path); parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return ToolReturn{}, fmt.Errorf("创建目录失败 %s: %w", parent, err)
		}
	}
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return ToolReturn{}, fmt.Errorf("写入失败 %s: %w", displayPath(tc, path), err)
	}

	verb := "已创建"
	if existed {
		verb = "已覆写"
	}
	return OutputOf(fmt.Sprintf("%s %s（%d 字节，%d 行）",
		verb, displayPath(tc, path), len(content), len(splitLines(content)))), nil
}

func editTool() *ToolDef {
	return &ToolDef{
		Name:        "edit",
		Description: "Edit an existing UTF-8 text file by replacing literal text.",
		Guidance: "Read a file before editing it, " +
			"unless you just created or edited it in this session.",
		Parameters: schema(map[string]any{
			"file_path": map[string]any{"type": "string", "description": "Path to edit, relative to the session working directory."},
			"old_string": map[string]any{"type": "string",
				"description": "Literal text to replace."},
			"new_string": map[string]any{"type": "string",
				"description": "Literal replacement text. Use an empty string to delete the match."},
			"replace_all": map[string]any{"type": "boolean",
				"description": "Replace all matches. Defaults to false; when false, old_string must appear exactly once."},
		}, "file_path", "old_string", "new_string"),
		Run: editRun,
	}
}

func editRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	raw, err := args.Str("file_path")
	if err != nil {
		return ToolReturn{}, err
	}
	oldString, err := args.Str("old_string")
	if err != nil {
		return ToolReturn{}, err
	}
	newString, err := args.Str("new_string")
	if err != nil {
		return ToolReturn{}, err
	}
	replaceAll := args.Bool("replace_all")

	path, err := Resolve(tc, raw)
	if err != nil {
		return ToolReturn{}, err
	}
	if oldString == "" {
		return ToolReturn{}, fmt.Errorf("old_string 不能为空")
	}
	if oldString == newString {
		return ToolReturn{}, fmt.Errorf("old_string 与 new_string 相同，无需编辑")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("读取失败 %s: %w", displayPath(tc, path), err)
	}
	// edit 不做编码转换：按 BOM 解码后写回会把文件悄悄变成 UTF-8，所以 UTF-16
	// 文件明确拒绝（.reg 这类文件改坏会连带系统设置）。
	if isUTF16(data) {
		return ToolReturn{}, fmt.Errorf("无法编辑 %s：文件是 UTF-16 编码，暂不支持按文本编辑", displayPath(tc, path))
	}

	// 二进制文件直接拒绝（与 dsh 的 edit 同策略：出现 NUL 即判为二进制）。
	if isBinarySample(data) {
		return ToolReturn{}, fmt.Errorf("无法编辑 %s：二进制文件（不是文本）", displayPath(tc, path))
	}
	text := string(data)

	// 模型给的 old_string 一定是 UTF-8；文件可能是 GBK。只有文件本身是合法
	// UTF-8 才做替换，避免按字节改坏非 UTF-8 文件（那种情况直接报错更安全）。
	if !utf8.ValidString(text) {
		return ToolReturn{}, fmt.Errorf("文件不是 UTF-8 编码，无法按文本编辑：%s", displayPath(tc, path))
	}

	// Windows 上文件普遍是 CRLF，而模型发来的 old_string 是 LF。
	// 首次匹配失败且文件确实是 CRLF 时，把待匹配串转成 CRLF 再试一次；
	// 写回时用的也是转换后的串，因此文件原来的换行风格保持不变。
	matchOld, matchNew := oldString, newString
	occurrences := strings.Count(text, matchOld)
	if occurrences == 0 && strings.Contains(text, "\r\n") {
		matchOld = toCRLF(oldString)
		matchNew = toCRLF(newString)
		occurrences = strings.Count(text, matchOld)
	}
	if occurrences == 0 {
		return ToolReturn{}, fmt.Errorf("未在 %s 中找到 old_string", displayPath(tc, path))
	}
	if occurrences > 1 && !replaceAll {
		return ToolReturn{}, fmt.Errorf(
			"old_string 在 %s 中出现 %d 次；请提供更长的上下文使其唯一，或设置 replace_all=true",
			displayPath(tc, path), occurrences)
	}
	if err := ctx.Err(); err != nil {
		return ToolReturn{}, err
	}

	replaced := 1
	updated := strings.Replace(text, matchOld, matchNew, 1)
	if replaceAll {
		replaced = occurrences
		updated = strings.ReplaceAll(text, matchOld, matchNew)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return ToolReturn{}, fmt.Errorf("写入失败 %s: %w", displayPath(tc, path), err)
	}

	line := strings.Count(text[:strings.Index(text, matchOld)], "\n") + 1
	return OutputOf(fmt.Sprintf("已在 %s 替换 %d 处（起始第 %d 行）：%q → %q",
		displayPath(tc, path), replaced, line, clip(oldString), clip(newString))), nil
}

// toCRLF 把 LF 换成 CRLF（已经是 CRLF 的不重复加 \r）。
func toCRLF(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
}

// clip 把多行文本压成单行短摘要（工具结果里回显改动内容）。
func clip(text string) string {
	runes := []rune(text)
	if len(runes) > 60 {
		text = string(runes[:60]) + "…"
	}
	return strings.ReplaceAll(text, "\n", "⏎")
}

func workingDirectoryTool() *ToolDef {
	return &ToolDef{
		Name: "working_directory",
		Description: "Read the current working directory, or change it with cd. Relative paths use the " +
			"current directory. Existing shells and running processes keep their own directories.",
		Parameters: schema(map[string]any{
			"cd": map[string]any{"type": "string", "description": "Existing directory to enter. Omit to read the current directory."},
		}),
		Run: workingDirectoryRun,
	}
}

func workingDirectoryRun(_ context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	target := args.OptStr("cd")
	if target == "" {
		return OutputOf(tc.Cwd()), nil
	}
	path, err := Resolve(tc, target)
	if err != nil {
		return ToolReturn{}, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ToolReturn{}, fmt.Errorf("不是目录或不存在：%s", displayPath(tc, path))
	}
	tc.setCwd(path)
	return OutputOf(path), nil
}
