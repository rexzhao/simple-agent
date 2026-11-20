package driver

import (
	"testing"

	"simple-agent/internal/tools"
)

// 所有结构化附加字段都要经得起重放。
//
// 这条守着一个反复踩过的坑：实时路径和重放路径是两处构造代码，
// 加字段时只改一边 → "刚做完能看到，重启后就没了"（diff 和 exec 都中过）。
// 现在两条路共用 attachToolExtras / attachRecordExtras，
// 这个测试确保每个字段都被带上。
//
// 放在**内部包**（package driver）里：要直接调这两个未导出的函数。
func TestAllToolExtrasSurviveReplay(t *testing.T) {
	outcome := tools.ToolOutcome{
		ID: "c1", Name: "grep", Output: "x",
		Diff:   &tools.FileDiff{Path: "f.go", Added: 1},
		Exec:   &tools.ExecInfo{ExitCode: 0, ElapsedMS: 12},
		Image:  &tools.ImageInfo{Path: "a.png", Width: 10, Height: 20},
		Web:    &tools.WebInfo{Kind: "search"},
		Search: &tools.SearchInfo{FileCount: 2, HitCount: 5},
		Read:   &tools.ReadInfo{StartLine: 1, EndLine: 9, TotalLines: 99},
		Todo:   &tools.TodoInfo{Total: 4, Completed: 1},
	}

	// 实时路径：outcome → part 数据
	live := map[string]any{"id": outcome.ID}
	attachToolExtras(live, outcome)

	// 重放路径：落盘记录 → part 数据
	record := resultRecords([]tools.ToolOutcome{outcome})[0]
	replay := map[string]any{"id": outcome.ID}
	attachRecordExtras(replay, record)

	keys := []string{
		extraDiff, extraExec, extraImageInfo, extraWeb, extraSearch, extraRead, extraTodo,
	}
	for _, key := range keys {
		if live[key] == nil {
			t.Fatalf("实时路径缺少字段 %q", key)
		}
		if replay[key] == nil {
			t.Fatalf("重放路径缺少字段 %q（实时有、重放没带上）", key)
		}
	}
}

// resultRecords 要把每个结构化字段从 outcome 搬到记录里。
func TestResultRecordsCarryAllExtras(t *testing.T) {
	outcome := tools.ToolOutcome{
		ID: "c1", Name: "grep",
		Web:    &tools.WebInfo{Kind: "search"},
		Search: &tools.SearchInfo{FileCount: 1},
		Read:   &tools.ReadInfo{TotalLines: 5},
		Todo:   &tools.TodoInfo{Total: 2},
	}
	record := resultRecords([]tools.ToolOutcome{outcome})[0]
	if record.Web == nil || record.Search == nil || record.Read == nil || record.Todo == nil {
		t.Fatalf("记录里缺字段：%+v", record)
	}
}