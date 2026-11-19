package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"simple-agent/internal/render"
)

// display_image 把图片**显示给用户看**。
//
// 与 read_image 的区别（两者不能混用）：
//
//	read_image  给**模型**看：要求视觉模型，会压缩到 ~1568px 省 token
//	display_image  给**用户**看：不需要视觉模型，不发缩放，保留原始字节
//
// 为什么需要它：模型跑命令生成了一张图表/截图，想让用户看到，以前只能
// 用 read_image —— 而那个在非视觉模型上直接报错，在视觉模型上又会把图
// 压小（用户存下来是糊的）。display_image 只做"上屏"，与模型能力无关。
func displayImageTool(fullAccess bool) *ToolDef {
	return &ToolDef{
		Name: "display_image",
		Description: "Display an image file to the user in the conversation. " +
			"The image is shown at full quality; it is NOT sent to you, so you cannot see it. " +
			"Use this when the user should look at a picture (a chart you generated, a screenshot). " +
			"To inspect an image yourself, use read_image instead.",
		Guidance: "Use display_image to let the USER see an image (generated chart, screenshot). " +
			"It does not require a vision model and does not downscale. " +
			"Do not use it to look at images yourself — that is read_image.",
		Parameters: schema(map[string]any{
			"file_path": map[string]any{
				"type":        "string",
				"description": pathDescription(fullAccess, "Path to the image file"),
			},
			"caption": map[string]any{
				"type":        "string",
				"description": "Optional one-line caption shown above the image",
			},
		}, "file_path"),
		Run: displayImageRun,
	}
}

func displayImageRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	raw, err := args.Str("file_path")
	if err != nil {
		return ToolReturn{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return ToolReturn{}, fmt.Errorf("file_path 不能为空")
	}
	path, err := Resolve(tc, raw)
	if err != nil {
		return ToolReturn{}, err
	}
	if err := ctx.Err(); err != nil {
		return ToolReturn{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("读取失败 %s: %w", displayPath(tc, path), err)
	}
	if info.IsDir() {
		return ToolReturn{}, fmt.Errorf("%s 是目录，不是图片", displayPath(tc, path))
	}
	if info.Size() > maxImageBytes {
		return ToolReturn{}, fmt.Errorf("无法显示 %s：图片超过 %dMB",
			displayPath(tc, path), maxImageBytes/(1024*1024))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("读取失败 %s: %w", displayPath(tc, path), err)
	}
	mediaType := render.SniffImage(data)
	if mediaType == "" {
		return ToolReturn{}, fmt.Errorf(
			"无法显示 %s：不是受支持的图片格式（display_image 只接受 PNG/JPEG/WebP/GIF）%s",
			displayPath(tc, path), imageExtHint(raw))
	}
	// **不压缩**：用户要的是原图（可能存下来当结果）。
	// 只解码尺寸用于显示比例的提示，失败就当未知，不阻塞。
	width, height, _ := imageDimensions(data, mediaType)

	caption, _ := args.Str("caption")
	caption = strings.TrimSpace(caption)
	displayName := displayPath(tc, path)

	// 输出给模型的文字：**只说"已显示"**，不贴图片内容。
	// 模型看不到这张图，说得越像"我看到了"越容易误导它编造细节。
	output := fmt.Sprintf("已把 %s 显示给用户", displayName)
	if caption != "" {
		output += "（标题：" + caption + "）"
	}
	output += "。你自己看不到这张图的内容；如需查看请用 read_image。"

	return ToolReturn{
		Output: output,
		Images: []ImageOutput{{
			MediaType: mediaType,
			Data:      data,
			Width:     width,
			Height:    height,
			Caption:   caption,
			// **不要发给模型**：这是纯展示用途。
			// 一张图动辄几十万 token，模型也未必有视觉能力。
			DisplayOnly: true,
		}},
	}, nil
}