package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"simple-agent/internal/render"
)

// read_image 的限额（与 dsh 的 attachment 默认值对齐：20MB / 8192px / 64MP），
// 再加上一条自己的缩放上限——视觉模型对超过 ~1600px 的长边没有更多细节收益，
// 却要付 token 与带宽。
const (
	maxImageBytes     = 20 * 1024 * 1024
	maxImageDimension = 8192
	maxImagePixels    = 64_000_000
	downscaleLongEdge = 1568
	downscalePixels   = 4_000_000
	// reencodeJPEGQuality 是缩放后重新编码的质量（有透明通道时改用 PNG）。
	reencodeJPEGQuality = 88
)

// ImageOutput 是工具返回的一张图片（由调用方写入 blob 存储并挂进工具结果）。
type ImageOutput struct {
	// MediaType 是规范化后的 MIME（image/png 等）。
	MediaType string
	// Data 是图片字节（可能已被缩放/重新编码）。
	Data []byte
	// Width/Height 是**返回给模型**的尺寸；原图尺寸在 Output 的文字里说明。
	Width  int
	Height int
}

func readImageTool() *ToolDef {
	return &ToolDef{
		Name: "read_image",
		Description: "Read a PNG/JPEG/WebP/GIF file and return the image itself. " +
			"Large images are downscaled automatically; do not create thumbnails to inspect an image. " +
			"Requires a model with image input.",
		Guidance: "Use the read_image tool to look at an image file directly; " +
			"do not transcode or resize it with shell commands first.",
		Parameters: schema(map[string]any{
			"file_path": map[string]any{"type": "string", "description": "Path to the image file, relative to the session working directory."},
		}, "file_path"),
		Run: readImageRun,
	}
}

func readImageRun(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) {
	raw, err := args.Str("file_path")
	if err != nil {
		return ToolReturn{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return ToolReturn{}, fmt.Errorf("file_path 不能为空")
	}
	// 路由能力先判：模型收不到图片时，读文件本身没有意义（与 dsh 的
	// assertImageCapableRoute 同一思路）。
	if !tc.Vision {
		return ToolReturn{}, fmt.Errorf(
			"无法读取 %s 为图片：当前模型不支持图片输入，请切换到支持视觉的模型", raw)
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
		return ToolReturn{}, fmt.Errorf("无法读取 %s：这是一个目录，不是图片文件", displayPath(tc, path))
	}
	if info.Size() > maxImageBytes {
		return ToolReturn{}, fmt.Errorf("无法读取 %s：图片超过 %dMB，请先缩小后重试",
			displayPath(tc, path), maxImageBytes/(1024*1024))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ToolReturn{}, fmt.Errorf("读取失败 %s: %w", displayPath(tc, path), err)
	}
	mediaType := render.SniffImage(data)
	if mediaType == "" {
		return ToolReturn{}, fmt.Errorf(
			"无法读取 %s：不是受支持的图片格式（read_image 只接受 PNG/JPEG/WebP/GIF）%s",
			displayPath(tc, path), imageExtHint(raw))
	}

	width, height, decodeErr := imageDimensions(data, mediaType)
	if decodeErr != nil {
		return ToolReturn{}, fmt.Errorf("无法读取 %s：%w", displayPath(tc, path), decodeErr)
	}
	if width > maxImageDimension || height > maxImageDimension {
		return ToolReturn{}, fmt.Errorf(
			"无法读取 %s：图片尺寸 %dx%d 超过单边 %dpx 上限，请先缩小后重试",
			displayPath(tc, path), width, height, maxImageDimension)
	}
	if width*height > maxImagePixels {
		return ToolReturn{}, fmt.Errorf(
			"无法读取 %s：图片像素数 %dx%d 超过 %d 万像素上限，请先缩小后重试",
			displayPath(tc, path), width, height, maxImagePixels/10_000)
	}

	out := ImageOutput{MediaType: mediaType, Data: data, Width: width, Height: height}
	scaled := false
	if mediaType != "image/webp" && shouldDownscale(width, height) {
		if small, newWidth, newHeight, scaleErr := downscaleImage(data, mediaType); scaleErr == nil {
			out = ImageOutput{MediaType: small.mediaType, Data: small.data, Width: newWidth, Height: newHeight}
			scaled = true
		}
		// 缩放失败就发原图：内容正确比省 token 重要。
	}
	return ToolReturn{
		Output: formatImageOutput(displayPath(tc, path), out, width, height, scaled),
		Images: []ImageOutput{out},
	}, nil
}

// shouldDownscale 报告是否值得把图片缩小后再发给模型。
func shouldDownscale(width, height int) bool {
	long := width
	if height > long {
		long = height
	}
	return long > downscaleLongEdge || width*height > downscalePixels
}

// imageExtHint 是错误信息里的扩展名提示（真正判定看文件签名）。
func imageExtHint(path string) string {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", "":
		return ""
	default:
		return fmt.Sprintf("（%s 不是图片扩展名）", ext)
	}
}

// formatImageOutput 构造与 dsh 同构的工具结果信封（路径 + 类型 + 内容摘要）。
func formatImageOutput(path string, image ImageOutput, originalWidth, originalHeight int, scaled bool) string {
	detail := fmt.Sprintf("%s image, %dx%d px, %d bytes",
		image.MediaType, image.Width, image.Height, len(image.Data))
	if scaled {
		detail += fmt.Sprintf(" (downscaled from %dx%d px)", originalWidth, originalHeight)
	}
	return fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\n%s\n</content>", path, detail)
}

// imageDimensions 返回图片的像素尺寸。
//
// PNG/JPEG/GIF 走标准库解码；WebP 标准库不支持，按容器头解析（VP8X/VP8/VP8L）。
func imageDimensions(data []byte, mediaType string) (int, int, error) {
	switch mediaType {
	case "image/png":
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, fmt.Errorf("PNG 解码失败：%w", err)
		}
		return cfg.Width, cfg.Height, nil
	case "image/jpeg":
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, fmt.Errorf("JPEG 解码失败：%w", err)
		}
		return cfg.Width, cfg.Height, nil
	case "image/gif":
		cfg, err := gif.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, fmt.Errorf("GIF 解码失败：%w", err)
		}
		return cfg.Width, cfg.Height, nil
	case "image/webp":
		width, height, err := webpDimensions(data)
		if err != nil {
			return 0, 0, err
		}
		return width, height, nil
	default:
		return 0, 0, fmt.Errorf("不支持的图片类型 %s", mediaType)
	}
}

// webpDimensions 从 WebP 容器头读尺寸（不需要解码像素）。
func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 30 {
		return 0, 0, errors.New("WebP 文件头被截断")
	}
	switch string(data[12:16]) {
	case "VP8X":
		width := 1 + int(data[24]) | int(data[25])<<8 | int(data[26])<<16
		height := 1 + int(data[27]) | int(data[28])<<8 | int(data[29])<<16
		return width, height, nil
	case "VP8L":
		if data[20] != 0x2f {
			return 0, 0, errors.New("WebP（VP8L）文件头无效")
		}
		bits := uint32(data[21]) | uint32(data[22])<<8 | uint32(data[23])<<16 | uint32(data[24])<<24
		width := int(bits&0x3FFF) + 1
		height := int((bits>>14)&0x3FFF) + 1
		return width, height, nil
	case "VP8 ":
		for i := 20; i+9 < len(data); i++ {
			if data[i] == 0x9d && data[i+1] == 0x01 && data[i+2] == 0x2a {
				width := int(data[i+3]) | int(data[i+4])<<8
				height := int(data[i+5]) | int(data[i+6])<<8
				return width & 0x3FFF, height & 0x3FFF, nil
			}
		}
		return 0, 0, errors.New("WebP（VP8）文件头无效")
	default:
		return 0, 0, errors.New("不支持的 WebP 编码（只支持 VP8/VP8L/VP8X）")
	}
}

type encodedImage struct {
	mediaType string
	data      []byte
}

// downscaleImage 把长边超过上限的图片按比例缩小，并重新编码。
//
// 用**区域平均**（box filter）而不是最近邻：缩小时最近邻会丢细节、产生锯齿，
// 而视觉模型看的正是细节。透明通道存在时统一编码成 PNG，否则用 JPEG 控制体积。
func downscaleImage(data []byte, mediaType string) (encodedImage, int, int, error) {
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return encodedImage{}, 0, 0, err
	}
	if format == "gif" {
		// 动图只保留第一帧会误导模型，直接发原图（体积通常不大）。
		return encodedImage{}, 0, 0, errors.New("GIF 不缩放")
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	newWidth, newHeight := scaledDimensions(width, height)
	if newWidth >= width && newHeight >= height {
		return encodedImage{}, 0, 0, errors.New("无需缩放")
	}
	dst := resizeBox(src, newWidth, newHeight)

	var buf bytes.Buffer
	outType := "image/jpeg"
	if hasAlpha(src) || mediaType == "image/png" {
		outType = "image/png"
		err = png.Encode(&buf, dst)
	} else {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: reencodeJPEGQuality})
	}
	if err != nil {
		return encodedImage{}, 0, 0, err
	}
	return encodedImage{mediaType: outType, data: buf.Bytes()}, newWidth, newHeight, nil
}

// scaledDimensions 按长边与像素预算算出缩放后的尺寸（保持比例）。
func scaledDimensions(width, height int) (int, int) {
	ratio := 1.0
	if long := maxInt(width, height); long > downscaleLongEdge {
		ratio = float64(downscaleLongEdge) / float64(long)
	}
	if pixels := float64(width) * float64(height); pixels*ratio*ratio > downscalePixels {
		ratio = math.Sqrt(downscalePixels / pixels)
	}
	if ratio >= 1 {
		return width, height
	}
	return maxInt(1, int(float64(width)*ratio)), maxInt(1, int(float64(height)*ratio))
}

// hasAlpha 报告图片是否带透明通道。
func hasAlpha(src image.Image) bool {
	switch src.(type) {
	case *image.NRGBA, *image.RGBA, *image.NRGBA64, *image.RGBA64:
		return true
	}
	return false
}

// resizeBox 用区域平均把 src 缩到 width×height。
//
// 每个目标像素覆盖源图的一块矩形，取其中所有像素的平均值——缩小时比最近邻平滑，
// 且没有双线性在强缩小时的高频混叠。
func resizeBox(src image.Image, width, height int) *image.NRGBA {
	bounds := src.Bounds()
	srcWidth, srcHeight := bounds.Dx(), bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for dy := 0; dy < height; dy++ {
		y0 := bounds.Min.Y + dy*srcHeight/height
		y1 := bounds.Min.Y + (dy+1)*srcHeight/height
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < width; dx++ {
			x0 := bounds.Min.X + dx*srcWidth/width
			x1 := bounds.Min.X + (dx+1)*srcWidth/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sumR, sumG, sumB, sumA, count uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, b, a := src.At(x, y).RGBA()
					sumR += uint64(r >> 8)
					sumG += uint64(g >> 8)
					sumB += uint64(b >> 8)
					sumA += uint64(a >> 8)
					count++
				}
			}
			if count == 0 {
				continue
			}
			index := dst.PixOffset(dx, dy)
			dst.Pix[index+0] = uint8(sumR / count)
			dst.Pix[index+1] = uint8(sumG / count)
			dst.Pix[index+2] = uint8(sumB / count)
			dst.Pix[index+3] = uint8(sumA / count)
		}
	}
	return dst
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
