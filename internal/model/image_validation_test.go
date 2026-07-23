package model

import (
	"strings"
	"testing"
)

func TestValidateImageInputBlocksRejectsUntrustedImageURLs(t *testing.T) {
	validPNG := ImageDataURL("image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	tests := []struct {
		name   string
		blocks []InputContentBlock
		want   string
	}{
		{
			name:   "remote URL",
			blocks: []InputContentBlock{{Type: "input_image", ImageURL: "https://example.com/image.png"}},
			want:   "base64 data URL",
		},
		{
			name:   "image payload on text block",
			blocks: []InputContentBlock{{Type: "input_text", ImageURL: validPNG}},
			want:   "not input_image",
		},
		{
			name:   "untrusted blob reference",
			blocks: []InputContentBlock{{Type: "input_image", ImageBlob: &BlobRef{Hash: "hash", SizeBytes: 8, MediaType: "image/png"}}},
			want:   "inline base64 data URL",
		},
		{
			name:   "unsupported detail",
			blocks: []InputContentBlock{{Type: "input_image", ImageURL: validPNG, Detail: "full"}},
			want:   "unsupported detail",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImageInputBlocks(tt.blocks, false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateImageInputBlocks() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateImageInputBlocksAllowsPersistedBlobReferences(t *testing.T) {
	blocks := []InputContentBlock{{
		Type:      "input_image",
		ImageBlob: &BlobRef{Hash: "abc", SizeBytes: 8, Encoding: "binary", MediaType: "image/png"},
	}}
	if err := ValidateImageInputBlocks(blocks, true); err != nil {
		t.Fatalf("ValidateImageInputBlocks() error = %v", err)
	}
}
