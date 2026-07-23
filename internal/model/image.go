package model

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
)

// BlobRef identifies immutable binary data retained outside a session JSONL
// record. It is shared with session storage so large image attachments do not
// inflate the durable event log.
type BlobRef struct {
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"size_bytes"`
	Encoding  string `json:"encoding"`
	MediaType string `json:"media_type,omitempty"`
}

const (
	// MaxImageInputAttachments, MaxImageInputBytes, and
	// MaxImageInputTotalBytes bound every user image input path, including
	// callers that do not pass through the Web API.
	MaxImageInputAttachments = 5
	MaxImageInputBytes       = 4 * 1024 * 1024
	MaxImageInputTotalBytes  = 12 * 1024 * 1024
)

// NormalizeImageMediaType returns the canonical MIME type for image formats
// accepted by every built-in multimodal provider.
func NormalizeImageMediaType(mediaType string) (string, bool) {
	switch normalized := strings.ToLower(strings.TrimSpace(mediaType)); normalized {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return normalized, true
	default:
		return "", false
	}
}

// ImageBytesMatchMediaType verifies a format signature before the bytes are
// persisted as an image attachment. It is intentionally lightweight: provider
// APIs remain responsible for full image decoding and dimension handling.
func ImageBytesMatchMediaType(mediaType string, data []byte) bool {
	normalized, supported := NormalizeImageMediaType(mediaType)
	if !supported {
		return false
	}
	switch normalized {
	case "image/png":
		return bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/jpeg":
		return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
	case "image/gif":
		return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
	case "image/webp":
		return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	default:
		return false
	}
}

// ImageDataURL encodes image bytes in the data-URL representation accepted by
// the supported model providers.
func ImageDataURL(mediaType string, data []byte) string {
	return "data:" + strings.ToLower(strings.TrimSpace(mediaType)) + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ParseSupportedImageDataURL decodes an inline data URL and validates the
// image format accepted by every built-in multimodal provider.
func ParseSupportedImageDataURL(value string) (string, []byte, error) {
	mediaType, data, err := ParseImageDataURL(value)
	if err != nil {
		return "", nil, err
	}
	normalizedMediaType, supported := NormalizeImageMediaType(mediaType)
	if !supported {
		return "", nil, fmt.Errorf("unsupported image media type %q", mediaType)
	}
	if len(data) == 0 {
		return "", nil, fmt.Errorf("image data is empty")
	}
	if !ImageBytesMatchMediaType(normalizedMediaType, data) {
		return "", nil, fmt.Errorf("image data does not match media type %q", normalizedMediaType)
	}
	return normalizedMediaType, data, nil
}

// ValidateImageInputBlocks checks image-bearing content blocks before they
// reach durable session storage or a provider request. Blob references are
// permitted only while reading or rewriting an already persisted message.
func ValidateImageInputBlocks(blocks []InputContentBlock, allowBlobRefs bool) error {
	imageCount := 0
	totalBytes := int64(0)
	for index, block := range blocks {
		typeName := strings.TrimSpace(block.Type)
		hasImage := typeName == "input_image" || strings.TrimSpace(block.ImageURL) != "" || block.ImageBlob != nil
		if !hasImage {
			continue
		}
		if typeName != "input_image" {
			return fmt.Errorf("content block %d has image data but is not input_image", index+1)
		}
		imageCount++
		if imageCount > MaxImageInputAttachments {
			return fmt.Errorf("at most %d images may be attached", MaxImageInputAttachments)
		}

		detail := strings.ToLower(strings.TrimSpace(block.Detail))
		switch detail {
		case "", "auto", "low", "high":
		default:
			return fmt.Errorf("image %d has unsupported detail %q", imageCount, block.Detail)
		}

		var imageBytes int64
		if block.ImageBlob != nil {
			if !allowBlobRefs {
				return fmt.Errorf("image %d must be an inline base64 data URL", imageCount)
			}
			if strings.TrimSpace(block.ImageURL) != "" {
				return fmt.Errorf("image %d cannot set both image_url and image_blob", imageCount)
			}
			if _, supported := NormalizeImageMediaType(block.ImageBlob.MediaType); !supported {
				return fmt.Errorf("image %d has unsupported media type %q", imageCount, block.ImageBlob.MediaType)
			}
			if block.ImageBlob.SizeBytes <= 0 {
				return fmt.Errorf("image %d has invalid size", imageCount)
			}
			imageBytes = block.ImageBlob.SizeBytes
		} else {
			_, data, err := ParseSupportedImageDataURL(block.ImageURL)
			if err != nil {
				return fmt.Errorf("image %d: %w", imageCount, err)
			}
			imageBytes = int64(len(data))
		}
		if imageBytes > MaxImageInputBytes {
			return fmt.Errorf("image %d exceeds the %d MiB limit", imageCount, MaxImageInputBytes/(1024*1024))
		}
		if imageBytes > MaxImageInputTotalBytes-totalBytes {
			return fmt.Errorf("attached images exceed the %d MiB total limit", MaxImageInputTotalBytes/(1024*1024))
		}
		totalBytes += imageBytes
	}
	return nil
}

// ParseImageDataURL decodes a base64 data URL. It deliberately accepts only
// inline data URLs: callers must not cause the local agent to fetch arbitrary
// remote image URLs while restoring durable session history.
func ParseImageDataURL(value string) (string, []byte, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "data:") {
		return "", nil, fmt.Errorf("image URL must be a base64 data URL")
	}
	separator := strings.IndexByte(value, ',')
	if separator < 0 {
		return "", nil, fmt.Errorf("image data URL is missing data")
	}
	header := value[len("data:"):separator]
	parts := strings.Split(header, ";")
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[1]), "base64") {
		return "", nil, fmt.Errorf("image data URL must use base64 encoding")
	}
	mediaType := strings.ToLower(strings.TrimSpace(parts[0]))
	if mediaType == "" {
		return "", nil, fmt.Errorf("image data URL is missing media type")
	}
	data, err := base64.StdEncoding.DecodeString(value[separator+1:])
	if err != nil {
		return "", nil, fmt.Errorf("decode image data URL: %w", err)
	}
	return mediaType, data, nil
}
