package tools

import (
	"strings"
)

// 行级 diff：把"旧文本 → 新文本"算成带上下文行的 hunks。
//
// 为什么要 LCS 而不是"旧的算删、新的算加"：edit 常常只在函数中间插几行，
// 朴素做法会把整个函数标成删除 + 新增，用户根本看不出改了什么。
// LCS 能把未改动的行识别成上下文，只标出真正变化的行。
//
// 输出的形态对齐常见 diff 工具的显示：
//
//	  24 function Foo()
//	  25     self:SetBarExpanded(false)
//	 -26
//	 -27     self.UI.btnExit.click = function()
//	 +26     self.UI.btnExpand.click = function()
//	  28 end
//
// 行号两列（旧/新）由 DiffLine.OldLine/NewLine 携带，界面据此对齐。

// diffContextLines 是每个改动块前后保留的上下文行数（与 git diff 默认一致）。
const diffContextLines = 3

// maxDiffLines 是输出行数上限（含上下文）。超出时截断并标记。
const maxDiffLines = 200

// LineDiffOptions 控制 diff 的生成。
type LineDiffOptions struct {
	// Context 是上下文行数（0 用默认 3）。
	Context int
	// MaxLines 是输出上限（0 用默认 200）。
	MaxLines int
}

// BuildLineDiff 计算 oldText → newText 的行级差异。
//
// path 只用于展示（取末级文件名）。
func BuildLineDiff(path, oldText, newText string, opts LineDiffOptions) *FileDiff {
	ctxLines := opts.Context
	if ctxLines <= 0 {
		ctxLines = diffContextLines
	}
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = maxDiffLines
	}

	oldLines := splitLines(oldText)
	newLines := splitLines(newText)

	// 逐行比较，得到操作序列
	ops := lcsOps(oldLines, newLines)

	diff := &FileDiff{Path: pathOnly(path)}
	for _, op := range ops {
		switch op.kind {
		case opEqual:
		case opDelete:
			diff.Removed++
		case opInsert:
			diff.Added++
		}
	}

	diff.Hunks = buildHunks(ops, oldLines, newLines, ctxLines, maxLines, diff)
	return diff
}

// diffOp 是一次行级操作。
type diffOp struct {
	kind int
	// oldIndex/newIndex 是这一行在各自文件里的下标（0 基）。
	oldIndex int
	newIndex int
}

const (
	opEqual = iota
	opDelete
	opInsert
)

// lcsOps 用 LCS 求出把 old 变成 new 的行级操作序列。
//
// 内存 O(n*m) 的经典实现：edit 的输入是"一小段 old_string / new_string"或
// 整个文件，但文件通常只有几百到几千行，二维表可以接受。
// 真遇到超大文件时 maxDiffLines 会先截断输出，不至于卡死界面。
func lcsOps(oldLines, newLines []string) []diffOp {
	n, m := len(oldLines), len(newLines)
	// dp[i][j] = oldLines[i:] 与 newLines[j:] 的 LCS 长度
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = maxInt(dp[i+1][j], dp[i][j+1])
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldLines[i] == newLines[j]:
			ops = append(ops, diffOp{kind: opEqual, oldIndex: i, newIndex: j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{kind: opDelete, oldIndex: i})
			i++
		default:
			ops = append(ops, diffOp{kind: opInsert, newIndex: j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: opDelete, oldIndex: i})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: opInsert, newIndex: j})
	}
	return ops
}

// buildHunks 把操作序列切成"改动 ± 上下文"的块，并填好行号。
func buildHunks(ops []diffOp, oldLines, newLines []string,
	ctxLines, maxLines int, diff *FileDiff) []DiffHunk {
	// 先找出所有改动的位置
	var changeIdx []int
	for index, op := range ops {
		if op.kind != opEqual {
			changeIdx = append(changeIdx, index)
		}
	}
	if len(changeIdx) == 0 {
		return nil
	}

	// 把相邻（间隔 ≤ 2*ctx）的改动并进同一块
	type span struct{ start, end int } // [start, end)
	var spans []span
	for _, index := range changeIdx {
		start := index - ctxLines
		if start < 0 {
			start = 0
		}
		end := index + ctxLines + 1
		if end > len(ops) {
			end = len(ops)
		}
		if n := len(spans); n > 0 && start <= spans[n-1].end {
			if end > spans[n-1].end {
				spans[n-1].end = end
			}
			continue
		}
		spans = append(spans, span{start: start, end: end})
	}

	oldLineAt := make([]int, len(ops)+1) // 每个操作位置对应的 1 基旧行号
	newLineAt := make([]int, len(ops)+1)
	oldNo, newNo := 1, 1
	for index, op := range ops {
		oldLineAt[index] = oldNo
		newLineAt[index] = newNo
		switch op.kind {
		case opEqual:
			oldNo++
			newNo++
		case opDelete:
			oldNo++
		case opInsert:
			newNo++
		}
	}
	oldLineAt[len(ops)] = oldNo
	newLineAt[len(ops)] = newNo

	total := 0
	for _, sp := range spans {
		hunk := DiffHunk{OldStart: oldLineAt[sp.start], NewStart: newLineAt[sp.start]}
		for index := sp.start; index < sp.end; index++ {
			op := ops[index]
			line := DiffLine{OldLine: 0, NewLine: 0}
			switch op.kind {
			case opEqual:
				line.Kind = "ctx"
				line.Text = oldLines[op.oldIndex]
				line.OldLine = oldLineAt[index]
				line.NewLine = newLineAt[index]
			case opDelete:
				line.Kind = "del"
				line.Text = oldLines[op.oldIndex]
				line.OldLine = oldLineAt[index]
			case opInsert:
				line.Kind = "add"
				line.Text = newLines[op.newIndex]
				line.NewLine = newLineAt[index]
			}
			hunk.Lines = append(hunk.Lines, line)
			total++
			if total >= maxLines {
				diff.Truncated = true
				break
			}
		}
		diff.Hunks = append(diff.Hunks, hunk)
		if diff.Truncated {
			break
		}
	}
	return diff.Hunks
}

// pathOnly 取路径末级（展示用）。
func pathOnly(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

