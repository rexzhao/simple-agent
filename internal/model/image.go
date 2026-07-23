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
