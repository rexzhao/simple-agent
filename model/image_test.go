package model

import (
	"bytes"
	"testing"
)

func TestParseImageDataURL(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G'}
	url := ImageDataURL("IMAGE/PNG", raw)
	mediaType, got, err := ParseImageDataURL(url)
	if err != nil {
		t.Fatalf("ParseImageDataURL() error = %v", err)
	}
	if mediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", mediaType)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("decoded bytes = %x, want %x", got, raw)
	}
}

func TestParseImageDataURLRejectsNonBase64URLs(t *testing.T) {
	for _, value := range []string{
		"https://example.com/image.png",
		"data:image/png,raw",
		"data:image/png;base64,not-base64",
		"data:;base64,AA==",
	} {
		if _, _, err := ParseImageDataURL(value); err == nil {
			t.Errorf("ParseImageDataURL(%q) error = nil, want error", value)
		}
	}
}

func TestImageBytesMatchMediaType(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		data      []byte
		want      bool
	}{
		{name: "png", mediaType: "image/png", data: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, want: true},
		{name: "jpeg", mediaType: "image/jpeg", data: []byte{0xff, 0xd8, 0xff}, want: true},
		{name: "gif", mediaType: "image/gif", data: []byte("GIF89a"), want: true},
		{name: "webp", mediaType: "image/webp", data: []byte("RIFF\x00\x00\x00\x00WEBP"), want: true},
		{name: "mismatched", mediaType: "image/png", data: []byte("GIF89a"), want: false},
		{name: "unsupported", mediaType: "image/bmp", data: []byte("BM"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImageBytesMatchMediaType(tt.mediaType, tt.data); got != tt.want {
				t.Fatalf("ImageBytesMatchMediaType(%q, %x) = %t, want %t", tt.mediaType, tt.data, got, tt.want)
			}
		})
	}
}
