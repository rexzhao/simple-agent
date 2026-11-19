package tools_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple-agent/internal/tools"
)

// writePNG 在 dir 下写一张 w×h 的纯色 PNG，返回完整路径。
func writePNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 120, B: 60, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// display_image：把图片交给界面显示，且**不发给模型**。
//
// 与 read_image 的关键区别：
//   - 不需要视觉模型（Vision=false 也要能跑）
//   - 图片标记 DisplayOnly，组装请求时会被跳过
//   - 不缩放，用户拿到的是原图
func TestDisplayImageWorksWithoutVision(t *testing.T) {
	tc := newCtx(t, false)
	// tc.Vision 默认 false —— read_image 这种模型上会直接报错，
	// display_image 必须照样能跑。
	writePNG(t, tc.Workspace(), "chart.png", 400, 300)
	path := "chart.png"

	result, err := runTool(t, tc, "display_image", tools.Args{
		"file_path": path,
		"caption":   "构建耗时图表",
	})
	if err != nil {
		t.Fatalf("display_image 不该要求视觉模型：%v", err)
	}

	if len(result.Images) != 1 {
		t.Fatalf("应返回 1 张图，实际 %d 张", len(result.Images))
	}
	img := result.Images[0]
	if !img.DisplayOnly {
		t.Fatal("display_image 的图片必须标记 DisplayOnly（否则会发给模型、白烧 token）")
	}
	if img.Caption != "构建耗时图表" {
		t.Fatalf("caption 应是「构建耗时图表」，实际 %q", img.Caption)
	}
	// 不缩放：原图 400x300 原样返回
	if img.Width != 400 || img.Height != 300 {
		t.Fatalf("应保留原尺寸 400x300，实际 %dx%d", img.Width, img.Height)
	}
	if len(img.Data) == 0 {
		t.Fatal("应带上图片字节")
	}
	// 输出文字要明确"模型看不到"，否则模型会去编造图里的细节
	if !strings.Contains(result.Output, "显示给用户") {
		t.Fatalf("输出应说明已显示给用户，实际：%q", result.Output)
	}
	if !strings.Contains(result.Output, "read_image") {
		t.Fatalf("输出应提示「要看内容用 read_image」，实际：%q", result.Output)
	}
}

// read_image 的图片**不带** DisplayOnly：它本来就是给模型看的。
func TestReadImageIsNotDisplayOnly(t *testing.T) {
	tc := newCtx(t, false)
	tc.Vision = true // read_image 要求视觉模型
	writePNG(t, tc.Workspace(), "photo.png", 100, 100)
	path := "photo.png"

	result, err := runTool(t, tc, "read_image", tools.Args{"file_path": path})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("应返回 1 张图，实际 %d 张", len(result.Images))
	}
	if result.Images[0].DisplayOnly {
		t.Fatal("read_image 的图片不能标 DisplayOnly —— 那正是要给模型看的")
	}
}

// 非图片文件要报错，且错误信息里带上工具名（模型才知道该换哪个工具）。
func TestDisplayImageRejectsNonImage(t *testing.T) {
	tc := newCtx(t, false)
	if err := os.WriteFile(filepath.Join(tc.Workspace(), "notes.txt"), []byte("纯文本"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := "notes.txt"
	_, err := runTool(t, tc, "display_image", tools.Args{"file_path": path})
	if err == nil {
		t.Fatal("非图片文件应报错")
	}
	if !strings.Contains(err.Error(), "display_image") {
		t.Fatalf("错误信息应提到 display_image，实际：%v", err)
	}
}

// 两个工具都要出现在工具表里（模型才能选）。
func TestBothImageToolsRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range tools.Builtin(false) {
		names[tool.Name] = true
	}
	for _, want := range []string{"read_image", "display_image"} {
		if !names[want] {
			t.Fatalf("工具表里缺少 %s", want)
		}
	}
}