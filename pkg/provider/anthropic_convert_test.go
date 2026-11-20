package provider

import (
	"encoding/binary"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestImageDimensionsFromBytesReadsPNGAndJPEGHeaders(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	png = binary.BigEndian.AppendUint32(png, 640)
	png = binary.BigEndian.AppendUint32(png, 480)
	if w, h, ok := ImageDimensionsFromBytes(png); !ok || w != 640 || h != 480 {
		t.Fatalf("png got %d %d %v", w, h, ok)
	}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08, 0x01, 0x2C, 0x02, 0x58, 0x03}
	w, h, ok := ImageDimensionsFromBytes(jpeg)
	ew, eh, eok := imageDimensionsFromBytes(jpeg)
	if ok != eok || w != ew || h != eh {
		t.Fatalf("jpeg export %d %d %v differs from %d %d %v", w, h, ok, ew, eh, eok)
	}
	if _, _, ok := ImageDimensionsFromBytes([]byte("not an image")); ok {
		t.Fatal("unreadable bytes reported dimensions")
	}
}

func TestSetAnthropicLiveBlocksReplacesTheSidecar(t *testing.T) {
	m := &state.Message{Role: "assistant"}
	SetAnthropicLiveBlocks(m, []any{map[string]any{"type": "text", "text": "a"}})
	SetAnthropicLiveBlocks(m, []any{map[string]any{"type": "text", "text": "b"}})
	blocks, ok := AnthropicLiveBlocks(m)
	if !ok || len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "b" {
		t.Fatalf("blocks %v %v", blocks, ok)
	}
}

func TestUnsupportedInlineImageMediaType(t *testing.T) {
	cases := map[string]string{
		"data:image/png;base64,AAAA":     "",
		"data:image/jpg;base64,AAAA":     "",
		"data:image/webp;base64,AAAA":    "",
		"data:image/svg+xml;base64,AAAA": "image/svg+xml",
		"DATA:IMAGE/BMP;base64,AAAA":     "image/bmp",
		"data:image/;base64,AAAA":        "image/unknown",
		"https://example.com/a.bmp":      "",
	}
	for in, want := range cases {
		if got := unsupportedInlineImageMediaType(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
