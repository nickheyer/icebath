package yuanbao

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestDownloadURLMetadataEndpointBlocked(t *testing.T) {
	_, _, err := DownloadURL(context.Background(), &tools.Env{}, "http://169.254.169.254/latest/meta-data/", DefaultMaxSizeMB)
	if err == nil || !strings.Contains(err.Error(), "SSRF protection") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadURLLoopbackBlocked(t *testing.T) {
	_, _, err := DownloadURL(context.Background(), &tools.Env{}, "http://127.0.0.1:8080/secret", DefaultMaxSizeMB)
	if err == nil || !strings.Contains(err.Error(), "SSRF protection") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageHelpersAndMsgBodies(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), 0, 0, 1, 0, 0, 0, 0, 0x80)
	if s := ParseImageSize(png); s == nil || s.Width != 256 || s.Height != 128 {
		t.Fatalf("png size %+v", s)
	}
	gif := append([]byte("GIF89a"), 10, 0, 20, 0)
	if s := ParseImageSize(gif); s == nil || s.Width != 10 || s.Height != 20 {
		t.Fatalf("gif size %+v", s)
	}
	if GuessMIMEType("A.JPG") != "image/jpeg" || !IsImage("x.png", "") || GetImageFormat("IMAGE/PNG") != 3 || GetImageFormat("x") != 255 {
		t.Fatal("mime helpers")
	}
	got, _ := pyjson.Dumps(BuildImageMsgBody("https://h/p/pic.png?x=1", "", "", 5, 2, 3, "image/png"))
	if got != `[{"msg_type": "TIMImageElem", "msg_content": {"uuid": "pic.png", "image_format": 3, "image_info_array": [{"type": 1, "size": 5, "width": 2, "height": 3, "url": "https://h/p/pic.png?x=1"}]}}]` {
		t.Fatalf("image body %s", got)
	}
	got, _ = pyjson.Dumps(BuildFileMsgBody("u", "f.pdf", "", 7))
	if got != `[{"msg_type": "TIMFileElem", "msg_content": {"uuid": "f.pdf", "file_name": "f.pdf", "file_size": 7, "url": "u"}}]` {
		t.Fatalf("file body %s", got)
	}
}

func TestCosSignLayout(t *testing.T) {
	auth := CosSign("PUT", "/a%20b", nil, [][2]string{{"Host", "b.cos"}, {"content-type", "image/png"}}, "sid", "skey", big.NewInt(100), big.NewInt(50))
	if !strings.HasPrefix(auth, "q-sign-algorithm=sha1&q-ak=sid&q-sign-time=100;150&q-key-time=100;150&q-header-list=content-type;host&q-url-param-list=&q-signature=") {
		t.Fatalf("auth %s", auth)
	}
	if got := sortedKV([][2]string{{"X", "a b/c"}}); got[0] != [2]string{"x", "a%20b%2Fc"} {
		t.Fatalf("sorted kv %v", got)
	}
}
