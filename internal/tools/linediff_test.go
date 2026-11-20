package tools

import (
	"fmt"
	"strings"
	"testing"
)

// 行级 diff 要像常见 diff 工具那样：带上下文行、行号两列、+/- 标记。
//
// 用户给的参考例子（把一段函数中间的几个回调换掉）：
//
//	  24 function Foo()
//	  25     self:SetBarExpanded(false)
//	 -26
//	 -27     self.UI.btnExit.click = ...
//	 +26     self.UI.btnExpand.click = ...
//	  28 end
func TestBuildLineDiffContextAndLineNumbers(t *testing.T) {
	oldText := strings.Join([]string{
		"end",
		"",
		"function UI_SubSceneBackgroundPreviewPanel:InitPreviewBar()",
		"    self:SetBarExpanded(false)",
		"",
		"    self.UI.btnExit.click = function()",
		"        self:OnClickExitPreview()",
		"    end",
		"end",
	}, "\n")
	newText := strings.Join([]string{
		"end",
		"",
		"function UI_SubSceneBackgroundPreviewPanel:InitPreviewBar()",
		"    self:SetBarExpanded(false)",
		"",
		"    self.UI.btnExpand.click = function()",
		"        self:OnClickExpandPreview()",
		"    end",
		"end",
	}, "\n")

	diff := BuildLineDiff("panel.lua", oldText, newText, LineDiffOptions{})
	if diff.Added != 2 || diff.Removed != 2 {
		t.Fatalf("应 +2 -2，实际 +%d -%d", diff.Added, diff.Removed)
	}
	if len(diff.Hunks) == 0 {
		t.Fatal("应有 hunk")
	}
	hunk := diff.Hunks[0]

	// 上下文行要带**两个**行号（旧/新），改动行只有一个
	var ctx, del, add int
	for _, line := range hunk.Lines {
		switch line.Kind {
		case "ctx":
			ctx++
			if line.OldLine == 0 || line.NewLine == 0 {
				t.Fatalf("上下文行应有两个行号：%+v", line)
			}
		case "del":
			del++
			if line.OldLine == 0 || line.NewLine != 0 {
				t.Fatalf("删除行应只有旧行号：%+v", line)
			}
		case "add":
			add++
			if line.OldLine != 0 || line.NewLine == 0 {
				t.Fatalf("新增行应只有新行号：%+v", line)
			}
		}
	}
	if ctx == 0 {
		t.Fatal("应有上下文行（否则看不出改在哪）")
	}
	if del != 2 || add != 2 {
		t.Fatalf("应各 2 行改动，实际 del=%d add=%d", del, add)
	}

	// 打印出来人工核对格式
	for _, line := range hunk.Lines {
		mark := " "
		switch line.Kind {
		case "del":
			mark = "-"
		case "add":
			mark = "+"
		}
		t.Logf("%s %3d %3d %s", mark, line.OldLine, line.NewLine, line.Text)
	}
	_ = fmt.Sprint()
}

// 改动相距很远时，应分成多个 hunk（不把中间一大段都当上下文）。
func TestBuildLineDiffSeparateHunks(t *testing.T) {
	var oldLines, newLines []string
	for i := 0; i < 40; i++ {
		oldLines = append(oldLines, fmt.Sprintf("line %d", i))
		newLines = append(newLines, fmt.Sprintf("line %d", i))
	}
	newLines[2] = "changed early"
	newLines[35] = "changed late"

	diff := BuildLineDiff("f.txt", strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"),
		LineDiffOptions{})
	if len(diff.Hunks) != 2 {
		t.Fatalf("相距很远的改动应分成 2 个 hunk，实际 %d", len(diff.Hunks))
	}
}

// 没有改动时不产生 hunk。
func TestBuildLineDiffNoChange(t *testing.T) {
	text := "a\nb\nc"
	diff := BuildLineDiff("f.txt", text, text, LineDiffOptions{})
	if len(diff.Hunks) != 0 || diff.Added != 0 || diff.Removed != 0 {
		t.Fatalf("无改动时应为空，实际 %+v", diff)
	}
}

// 纯新增（原位置为空）：只有 add 行。
func TestBuildLineDiffPureInsert(t *testing.T) {
	diff := BuildLineDiff("f.txt", "a\nc", "a\nb\nc", LineDiffOptions{})
	if diff.Added != 1 || diff.Removed != 0 {
		t.Fatalf("应 +1 -0，实际 +%d -%d", diff.Added, diff.Removed)
	}
}