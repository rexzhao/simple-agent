// 宽度把手的几何与拖动方向。
//
// 这一组是"拖动方向反了"和"把手左右不对称"两个 bug 的守门测试：
//   - 标签与位置必须一致（曾经 Left()/Right() 用反，拖右边走了左边的分支）；
//   - 两侧留白必须相等（曾经用窗口宽度当基准，容器在侧栏右边 → 不对称）；
//   - 两侧往外拖都必须变宽（只测一侧会漏掉符号错误）。
package uiapp_test

import (
	"testing"
)

// 左侧把手必须真的在左边、右侧把手在右边（标签与位置一致）。
//
// 拖动方向的符号依赖这个前提：一旦两边的标签与位置错位，
// 拖右侧就会走左侧的分支，表现为"方向反了"。
func TestWidthHandlePositionsMatchLabels(t *testing.T) {
	_, _, tester, _ := newWidthApp(t, 1600, 900, 0)
	left, okLeft := tester.Find("调整会话宽度（左）")
	right, okRight := tester.Find("调整会话宽度（右）")
	if !okLeft || !okRight {
		t.Fatalf("找不到把手 left=%v right=%v：%v", okLeft, okRight, tester.Texts())
	}
	t.Logf("left.X=%.1f left.W=%.1f right.X=%.1f right.W=%.1f", left.X, left.W, right.X, right.W)
	if left.X >= right.X {
		t.Fatalf("左侧把手应在右侧把手的左边：left.X=%.1f right.X=%.1f", left.X, right.X)
	}
	// 两条把手到**正文容器**两边的距离必须相等（= 正文居中，拖动只改宽度）。
	// 容器左边 = 侧栏宽度，右边 = 窗口宽度。
	sidebar := float32(280)
	window := float32(1600)
	leftGap := left.X - sidebar
	rightGap := window - (right.X + right.W)
	if diff := leftGap - rightGap; diff > 1 || diff < -1 {
		t.Fatalf("两侧留白应相等（正文居中）：左 %.1f 右 %.1f", leftGap, rightGap)
	}
}

// 两侧把手的拖动方向：往外拖都变宽，往里拖都变窄。
//
// 这是"方向反了"那个 bug 的守门测试：只测一侧会漏掉符号错误
// （曾经左右标签与位置对调，拖右边走了左边的分支）。
func TestWidthHandleBothSidesDragDirection(t *testing.T) {
	cases := []struct {
		name string
		side string
		// outward 是"把把手往远离正文的方向拖"的 dx
		outward float32
	}{
		{"左把手往外（向左）变宽", "调整会话宽度（左）", -60},
		{"右把手往外（向右）变宽", "调整会话宽度（右）", 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, p, tester, _ := newWidthApp(t, 1600, 900, 0)
			before := app.ContentWidthForTest()

			rect, ok := tester.Find(c.side)
			if !ok {
				t.Fatalf("找不到把手 %q", c.side)
			}
			x, y := rect.X+rect.W/2, rect.Y+rect.H/2
			tester.Press(x, y)
			tester.Move(x+c.outward, y)
			tester.Release(x+c.outward, y)
			pump(tester, p)

			after := app.ContentWidthForTest()
			if after <= before {
				t.Fatalf("%s：往外交拖应变宽，实际 %d → %d", c.side, before, after)
			}

			// 再往内拖回去：应变窄
			rect, _ = tester.Find(c.side)
			x, y = rect.X+rect.W/2, rect.Y+rect.H/2
			tester.Press(x, y)
			tester.Move(x-c.outward, y)
			tester.Release(x-c.outward, y)
			pump(tester, p)
			if got := app.ContentWidthForTest(); got >= after {
				t.Fatalf("%s：往内拖应变窄，实际 %d → %d", c.side, after, got)
			}
		})
	}
}
