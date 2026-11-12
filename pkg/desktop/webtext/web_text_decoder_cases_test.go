package webtext

import (
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func join(parts ...any) []byte {
	var out []byte
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			out = append(out, v...)
		case []byte:
			out = append(out, v...)
		}
	}
	return out
}

func TestHonoursAQuotedBig5CharsetInTheResponseHeader(t *testing.T) {
	data := join("<title>", []byte{0xb4, 0xa3, 0xa5, 0xdc, 0xab, 0x48, 0xae, 0xa7}, "</title>")
	if got := DecodeWebText(data, `text/html; charset="big5"`); got != "<title>提示信息</title>" {
		t.Fatalf("got %q", got)
	}
}

func TestSniffsAShiftJisMetaCharsetBeforeDecodingTheDocument(t *testing.T) {
	data := join("<meta charset=shift_jis><title>", []byte{0x93, 0xfa, 0x96, 0x7b, 0x8c, 0xea}, "</title>")
	if got := DecodeWebText(data, ""); got != "<meta charset=shift_jis><title>日本語</title>" {
		t.Fatalf("got %q", got)
	}
}

func TestSniffsCharsetFromAnHttpEquivContentAttribute(t *testing.T) {
	data := join(`<meta http-equiv="Content-Type" content="text/html; charset=gbk"><title>`, []byte{0xd6, 0xd0, 0xce, 0xc4}, "</title>")
	if got := DecodeWebText(data, ""); !strings.Contains(got, "<title>中文</title>") {
		t.Fatalf("got %q", got)
	}
}

func TestFallsBackToUtf8WhenAServerDeclaresAnUnknownCharset(t *testing.T) {
	if got := DecodeWebText([]byte("<title>Résumé</title>"), "text/html; charset=not-a-real-encoding"); got != "<title>Résumé</title>" {
		t.Fatalf("got %q", got)
	}
}

func TestTheHttpCharsetTakesPrecedenceOverAConflictingMetaDeclaration(t *testing.T) {
	data := join("<meta charset=utf-8><title>", []byte{0xd6, 0xd0, 0xce, 0xc4})
	if got := DecodeWebText(data, "text/html; charset=gbk"); got != "<meta charset=utf-8><title>中文" {
		t.Fatalf("got %q", got)
	}
}
