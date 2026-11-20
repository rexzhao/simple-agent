package tools_test

import (
	"testing"

	"simple-agent/internal/tools"
)

// read_image 的长边上限是 1568：**超过才缩，小于则原样返回**。
//
// 为什么不是"永远缩到 1568"：小图被拉大既浪费 token（按面积算）
// 又不会多出任何细节。用户的原话："如果图片本身小于 1568，不应该强制拉到"。
func TestReadImageDownscalesOnlyWhenLarger(t *testing.T) {
	tc := newCtx(t, false)
	tc.Vision = true

	// 大于 1568 → 缩到 1568
	writePNG(t, tc.Workspace(), "big.png", 2000, 1200)
	result, err := runTool(t, tc, "read_image", tools.Args{"file_path": "big.png"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Images[0].Width; got != 1568 {
		t.Fatalf("2000 宽应缩到 1568，实际 %d", got)
	}

	// 正好 1568 → 不动
	writePNG(t, tc.Workspace(), "edge.png", 1568, 900)
	result, err = runTool(t, tc, "read_image", tools.Args{"file_path": "edge.png"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Images[0].Width; got != 1568 {
		t.Fatalf("正好 1568 应保持原样，实际 %d", got)
	}

	// 小于 1568 → **原样返回，不拉大**
	writePNG(t, tc.Workspace(), "small.png", 300, 200)
	result, err = runTool(t, tc, "read_image", tools.Args{"file_path": "small.png"})
	if err != nil {
		t.Fatal(err)
	}
	img := result.Images[0]
	if img.Width != 300 || img.Height != 200 {
		t.Fatalf("小图应原样返回 300x200，实际 %dx%d（不该拉大）", img.Width, img.Height)
	}
}

// max_size 只让模型要**更小**的副本；传比 1568 大的按 1568 算。
func TestReadImageMaxSize(t *testing.T) {
	tc := newCtx(t, false)
	tc.Vision = true
	writePNG(t, tc.Workspace(), "big.png", 2000, 1200)

	// 要 768 → 缩到 768
	result, err := runTool(t, tc, "read_image", tools.Args{
		"file_path": "big.png", "max_size": float64(768),
	})
	if err != nil {
		t.Fatal(err)
	}
	img := result.Images[0]
	if img.Width != 768 {
		t.Fatalf("max_size=768 时应缩到 768，实际 %d", img.Width)
	}
	// 2000:1200 = 5:3 → 768 宽对应高 460.8 → 460
	if img.Height < 455 || img.Height > 465 {
		t.Fatalf("高度应等比缩到约 460，实际 %d", img.Height)
	}

	// 要 4000（比默认上限大）→ 按 1568 算，不会被放大
	result, err = runTool(t, tc, "read_image", tools.Args{
		"file_path": "big.png", "max_size": float64(4000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Images[0].Width; got != 1568 {
		t.Fatalf("max_size 超过默认上限时按 1568 算，实际 %d", got)
	}
}

// 小图 + max_size 大于原图：原样返回（全局「不放大」规则的又一入口）。
func TestReadImageNeverUpscales(t *testing.T) {
	tc := newCtx(t, false)
	tc.Vision = true
	writePNG(t, tc.Workspace(), "small.png", 300, 200)

	for _, maxSize := range []float64{0, 800, 4000} {
		args := tools.Args{"file_path": "small.png"}
		if maxSize > 0 {
			args["max_size"] = maxSize
		}
		result, err := runTool(t, tc, "read_image", args)
		if err != nil {
			t.Fatalf("max_size=%v 不该报错：%v", maxSize, err)
		}
		img := result.Images[0]
		if img.Width != 300 || img.Height != 200 {
			t.Fatalf("max_size=%v 时小图应保持 300x200，实际 %dx%d",
				maxSize, img.Width, img.Height)
		}
	}
}

// 负数 max_size 当作没传，走默认上限。
func TestReadImageNegativeMaxSizeIgnored(t *testing.T) {
	tc := newCtx(t, false)
	tc.Vision = true
	writePNG(t, tc.Workspace(), "big.png", 2000, 1200)

	result, err := runTool(t, tc, "read_image", tools.Args{
		"file_path": "big.png", "max_size": float64(-100),
	})
	if err != nil {
		t.Fatalf("负数 max_size 不该报错：%v", err)
	}
	if got := result.Images[0].Width; got != 1568 {
		t.Fatalf("负数应被忽略、按默认 1568，实际 %d", got)
	}
}