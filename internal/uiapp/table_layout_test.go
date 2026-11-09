package uiapp_test

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/conversation"
	"simple-agent/internal/message"
)

// 表格的**几何**布局：同一行里的单元格必须横向并排、各占一列；
// 不同行的同一列必须纵向对齐成一条直线。
//
// 之前只断言"文字存在"，所以"每个单元格各占一整行"的错位渲染能一路通过。
func TestTableCellsAreLaidOutInColumns(t *testing.T) {
	app, _, p := newApp(t)
	// 三列两行，单元格文字各不相同便于定位
	msg := message.Message{MessageID: "tbl-1", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("| 层 | 模块 | 说明 |\n|---|---|---|\n| 容器与寻址 | src/casc/* | 第一行 |\n| 压缩 | casc/bite.rs | 第二行 |"),
	}}
	if _, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	type cell struct{ name string }
	header := []cell{{"层"}, {"模块"}, {"说明"}}
	row1 := []cell{{"容器与寻址"}, {"src/casc/*"}, {"第一行"}}
	row2 := []cell{{"压缩"}, {"casc/bite.rs"}, {"第二行"}}

	// 每一行内部：X 必须递增（横向并排，而不是各自换行）
	checkRow := func(label string, cells []cell) []ui.Rect {
		rects := make([]ui.Rect, 0, len(cells))
		for _, c := range cells {
			rect, ok := tester.Find(c.name)
			if !ok {
				t.Fatalf("%s：找不到单元格 %q：%v", label, c.name, tester.Texts())
			}
			rects = append(rects, rect)
		}
		for i := 1; i < len(rects); i++ {
			if rects[i].X <= rects[i-1].X {
				t.Fatalf("%s：第 %d 列应在第 %d 列右侧（实际 x=%.1f vs %.1f）——表格被渲染成每个单元格一行了",
					label, i+1, i, rects[i].X, rects[i-1].X)
			}
			// 同一行的纵向位置应一致（同一行）
			if diff := rects[i].Y - rects[i-1].Y; diff > 2 || diff < -2 {
				t.Fatalf("%s：第 %d 列与第 %d 列不在同一行（y=%.1f vs %.1f）",
					label, i+1, i, rects[i].Y, rects[i-1].Y)
			}
		}
		return rects
	}
	head := checkRow("表头", header)
	r1 := checkRow("第 1 行", row1)
	r2 := checkRow("第 2 行", row2)

	// 跨行：同一列的 X 要对齐（列宽一致、起点一致）
	for i := range head {
		if diff := r1[i].X - head[i].X; diff > 2 || diff < -2 {
			t.Fatalf("第 %d 列：第 1 行与表头的 x 不一致（%.1f vs %.1f）——列没有对齐",
				i+1, r1[i].X, head[i].X)
		}
		if diff := r2[i].X - head[i].X; diff > 2 || diff < -2 {
			t.Fatalf("第 %d 列：第 2 行与表头的 x 不一致（%.1f vs %.1f）——列没有对齐",
				i+1, r2[i].X, head[i].X)
		}
	}
}

// 两列表格（用户截图里的那种）：数据行必须同样并排，不能各占一行。
func TestTwoColumnTableLayout(t *testing.T) {
	app, _, p := newApp(t)
	msg := message.Message{MessageID: "tbl-2", Role: message.RoleAgent, Parts: []message.Part{
		message.NewTextPart("| 层 | 模块 |\n|---|---|\n| 容器与寻址 | src/casc/* |\n| 压缩 | casc/bite.rs |\n| 数据表 | db2.rs |"),
	}}
	if _, err := app.Current().Conversation().AddMessage(msg, conversation.StatusComplete); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(app.View, 1200, 800)
	pump(tester, p)

	// 每一行的两个单元格都要横向并排
	rows := [][2]string{
		{"层", "模块"},
		{"容器与寻址", "src/casc/*"},
		{"压缩", "casc/bite.rs"},
		{"数据表", "db2.rs"},
	}
	firstX := float32(-1)
	for _, pair := range rows {
		left, ok := tester.Find(pair[0])
		if !ok {
			t.Fatalf("找不到 %q：%v", pair[0], tester.Texts())
		}
		right, ok := tester.Find(pair[1])
		if !ok {
			t.Fatalf("找不到 %q：%v", pair[1], tester.Texts())
		}
		t.Logf("%-12s x=%.1f   %-14s x=%.1f  （y %.1f / %.1f）",
			pair[0], left.X, pair[1], right.X, left.Y, right.Y)
		if right.X <= left.X {
			t.Fatalf("「%s」应在「%s」右侧：%.1f vs %.1f", pair[1], pair[0], right.X, left.X)
		}
		if diff := left.Y - right.Y; diff > 2 || diff < -2 {
			t.Fatalf("「%s」与「%s」不在同一行：y=%.1f vs %.1f", pair[0], pair[1], left.Y, right.Y)
		}
		// 第一列的 X 跨行一致
		if firstX < 0 {
			firstX = left.X
		} else if diff := left.X - firstX; diff > 2 || diff < -2 {
			t.Fatalf("第一列的 X 跨行不一致：%.1f vs %.1f", left.X, firstX)
		}
	}

	// 表格不能撑破正文列：第二列的**右边缘**应在正文列右边界之内。
	//
	// 正文列最宽 chatColumnWidth=748 且居中；窗口 1200 时右边界约 974。
	// ScrollHorizontal 是 Row，子元素按内容宽度排；Grid 的 Fr(1) 在无约束宽度下
	// 无法均分，所以这里顺手守住"没有把布局撑出去"。
	right, _ := tester.Find("模块")
	if right.X+right.W > 980 {
		t.Fatalf("表格超出正文列右边界：%.1f > 980", right.X+right.W)
	}
}
