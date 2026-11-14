package mediacache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	ogg      = append([]byte("OggS"), make([]byte, 64)...)
	flac     = append([]byte("fLaC"), make([]byte, 64)...)
	wav      = append([]byte("RIFF\x24\x08\x00\x00WAVEfmt "), make([]byte, 64)...)
	webp     = append([]byte("RIFF\x24\x08\x00\x00WEBPVP8 "), make([]byte, 64)...)
	mp3ID3   = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 64)...)
	mp3Frame = append([]byte("\xff\xfb\x90\x00"), make([]byte, 64)...)
	aacADTS  = append([]byte("\xff\xf1\x50\x80"), make([]byte, 64)...)
	m4a      = append([]byte("\x00\x00\x00\x1cftypM4A "), make([]byte, 64)...)
	m4b      = append([]byte("\x00\x00\x00\x1cftypM4B "), make([]byte, 64)...)
	mp4ISOM  = append([]byte("\x00\x00\x00\x18ftypisom"), make([]byte, 64)...)
	webm     = append([]byte("\x1a\x45\xdf\xa3"), make([]byte, 64)...)
	unknown  = append([]byte("not-audio-at-all"), make([]byte, 64)...)
	png      = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 64)...)
)

func tempHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestSniffContainerMagicBytes(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{{ogg, "ogg"}, {flac, "flac"}, {wav, "wav"}, {mp3ID3, "mp3"}, {mp3Frame, "mp3"}, {aacADTS, "aac"}, {m4a, "m4a"}, {m4b, "m4a"}, {mp4ISOM, "mp4"}, {webm, "webm"}}
	for _, c := range cases {
		if got, ok := SniffContainer(c.data); !ok || got != c.want {
			t.Fatalf("%q sniffed %q %v, want %q", c.data[:12], got, ok, c.want)
		}
	}
	for _, data := range [][]byte{ogg, flac, wav, mp3ID3, aacADTS, m4a, mp4ISOM, webm} {
		got, _ := SniffContainer(data)
		if _, ok := ContainerToExt[got]; !ok {
			t.Fatalf("container %q has no extension", got)
		}
	}
	for _, data := range [][]byte{webp, png, unknown} {
		if got, ok := SniffContainer(data); ok {
			t.Fatalf("non audio bytes claimed as %q", got)
		}
	}
}

func TestSniffAudioExtContainerWinsOverClaimedExt(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{{ogg, ".ogg"}, {flac, ".flac"}, {wav, ".wav"}, {mp3ID3, ".mp3"}, {mp3Frame, ".mp3"}, {aacADTS, ".aac"}, {m4a, ".m4a"}, {webm, ".webm"}, {mp4ISOM, ".m4a"}}
	for _, c := range cases {
		claimed := ".ogg"
		if c.want == ".ogg" {
			claimed = ".mp3"
		}
		if got := SniffAudioExt(c.data, claimed); got != c.want {
			t.Fatalf("got %q want %q", got, c.want)
		}
	}
	if got := SniffAudioExt(unknown, "mp3"); got != ".mp3" {
		t.Fatalf("fallback without dot gave %q", got)
	}
}

func TestCacheAudioFromBytesRepairsWrongExt(t *testing.T) {
	h := tempHome(t)
	cases := []struct {
		data            []byte
		claimed, suffix string
	}{{mp3ID3, ".ogg", ".mp3"}, {wav, ".ogg", ".wav"}, {m4a, ".ogg", ".m4a"}, {ogg, ".mp3", ".ogg"}, {aacADTS, ".ogg", ".aac"}, {webm, ".ogg", ".webm"}, {flac, ".mp3", ".flac"}}
	for _, c := range cases {
		p, err := CacheAudioFromBytes(context.Background(), h, c.data, c.claimed)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(p) != c.suffix || filepath.Dir(p) != filepath.Join(h, "cache", "audio") {
			t.Fatalf("cached at %s, want suffix %s", p, c.suffix)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, c.data) {
			t.Fatal("bytes changed")
		}
	}
}

func TestCacheAudioFromBytesBasicCaching(t *testing.T) {
	h := tempHome(t)
	p, err := CacheAudioFromBytes(context.Background(), h, []byte("fake-ogg-bytes"), ".ogg")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "fake-ogg-bytes" || !strings.HasPrefix(filepath.Base(p), "audio_") || filepath.Ext(p) != ".ogg" {
		t.Fatalf("cached %s %q", p, got)
	}
	st, _ := os.Stat(p)
	dst, _ := os.Stat(filepath.Dir(p))
	if st.Mode().Perm() != 0o600 || dst.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", st.Mode().Perm(), dst.Mode().Perm())
	}
}

func agedWrite(t *testing.T, p string, ageHours float64) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Duration(ageHours * float64(time.Hour)))
	os.Chtimes(p, ts, ts)
}

func TestCleanupCachesRemoveOldFilesAndKeepRecent(t *testing.T) {
	h := tempHome(t)
	type pair struct {
		dir     func(context.Context, string) (string, error)
		cleanup func(context.Context, string, float64) (int, error)
	}
	for _, p := range []pair{{AudioCacheDir, CleanupAudioCache}, {ScreenshotCacheDir, CleanupScreenshotCache}, {DocumentCacheDir, CleanupDocumentCache}, {ImageCacheDir, CleanupImageCache}, {VideoCacheDir, CleanupVideoCache}} {
		d, err := p.dir(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		agedWrite(t, filepath.Join(d, "old.bin"), 48)
		agedWrite(t, filepath.Join(d, "fresh.bin"), 0)
		n, err := p.cleanup(context.Background(), h, MediaCacheMaxAgeHours)
		if err != nil || n != 1 {
			t.Fatalf("%s removed %d %v", d, n, err)
		}
		if _, err := os.Stat(filepath.Join(d, "fresh.bin")); err != nil {
			t.Fatalf("recent file removed in %s", d)
		}
	}
	if d, _ := ScreenshotCacheDir(context.Background(), h); d != filepath.Join(h, "cache", "screenshots") {
		t.Fatalf("screenshot dir %s", d)
	}
}

func TestCacheDirPrefersPopulatedLegacyDir(t *testing.T) {
	h := tempHome(t)
	legacy := filepath.Join(h, "document_cache")
	os.MkdirAll(legacy, 0o755)
	os.WriteFile(filepath.Join(legacy, "keep.txt"), []byte("x"), 0o644)
	d, err := DocumentCacheDir(context.Background(), h)
	if err != nil || d != legacy {
		t.Fatalf("dir %s %v", d, err)
	}
	os.MkdirAll(filepath.Join(h, "image_cache"), 0o755)
	if d, _ := ImageCacheDir(context.Background(), h); d != filepath.Join(h, "cache", "images") {
		t.Fatalf("empty legacy dir shadowed the new path: %s", d)
	}
}

func TestCacheDocumentFromBytes(t *testing.T) {
	h := tempHome(t)
	d, err := DocumentCacheDir(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		t.Fatal("document cache dir not created")
	}
	p, err := CacheDocumentFromBytes(context.Background(), h, []byte("hello world"), "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "hello world" {
		t.Fatal("content mismatch")
	}
	p, _ = CacheDocumentFromBytes(context.Background(), h, []byte("data"), "report.pdf")
	if !strings.Contains(filepath.Base(p), "report.pdf") || !strings.HasPrefix(filepath.Base(p), "doc_") {
		t.Fatalf("name %s", p)
	}
	for _, name := range []string{"", "..", "../../etc/passwd", " \x00 ", "dir/"} {
		p, err := CacheDocumentFromBytes(context.Background(), h, []byte("data"), name)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(p) != d {
			t.Fatalf("%q escaped to %s", name, p)
		}
		if got, _ := os.ReadFile(p); string(got) != "data" {
			t.Fatal("content mismatch")
		}
	}
	if p, _ := CacheDocumentFromBytes(context.Background(), h, []byte("x"), "../../etc/passwd"); !strings.HasSuffix(p, "_passwd") {
		t.Fatalf("directory components kept: %s", p)
	}
	if p, _ := CacheDocumentFromBytes(context.Background(), h, []byte("x"), ""); !strings.HasSuffix(p, "_document") {
		t.Fatalf("empty name fallback: %s", p)
	}
}

func TestSupportedDocumentTypesHaveMimeTypes(t *testing.T) {
	for ext, mime := range SupportedDocumentTypes {
		if !strings.HasPrefix(ext, ".") || !strings.Contains(mime, "/") {
			t.Fatalf("%s %s", ext, mime)
		}
	}
	for ext, mime := range SupportedVideoTypes {
		if !strings.HasPrefix(ext, ".") || !strings.HasPrefix(mime, "video/") {
			t.Fatalf("%s %s", ext, mime)
		}
	}
}

func TestCacheImageFromBytesValidatesImageAndSize(t *testing.T) {
	h := tempHome(t)
	ctx := context.Background()
	p, err := CacheImageFromBytes(ctx, h, png, ".png")
	if err != nil || !strings.HasPrefix(filepath.Base(p), "img_") || filepath.Dir(p) != filepath.Join(h, "cache", "images") {
		t.Fatalf("cached %s %v", p, err)
	}
	_, err = CacheImageFromBytes(ctx, h, []byte("<html>Error</html>"), ".jpg")
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Msg != "Refusing to cache non-image data as .jpg (starts with: '<html>Error</html>')" {
		t.Fatalf("non image error %v", err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  max_inbound_media_bytes: 16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InboundMediaMaxBytes(ctx, h); got != 16 {
		t.Fatalf("cap %d", got)
	}
	for _, call := range []func() error{
		func() error { _, err := CacheImageFromBytes(ctx, h, png, ".png"); return err },
		func() error { _, err := CacheAudioFromBytes(ctx, h, ogg, ".ogg"); return err },
		func() error { _, err := CacheVideoFromBytes(ctx, h, ogg, ".mp4"); return err },
	} {
		if err := call(); !errors.As(err, &ve) || !strings.Contains(ve.Msg, "payload is too large") {
			t.Fatalf("oversized payload accepted: %v", err)
		}
	}
	if _, err := CacheImageFromBytes(ctx, h, png, ".png"); err == nil || err.Error() != "Inbound image payload is too large (72 bytes > 16 bytes)" {
		t.Fatalf("message %v", err)
	}
	zero := int64(0)
	if err := ValidateInboundMediaSize(ctx, h, 1<<40, "media", &zero); err != nil {
		t.Fatal("zero cap must disable the check")
	}
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  max_inbound_media_bytes: lots\n"), 0o600)
	if got := InboundMediaMaxBytes(ctx, h); got != DefaultInboundMediaMaxBytes {
		t.Fatalf("unparseable cap %d", got)
	}
}

func TestCacheFunctionsUseTheGivenProfileHome(t *testing.T) {
	tempHome(t)
	other := t.TempDir()
	p, err := CacheVideoFromBytes(context.Background(), other, []byte("video"), ".mp4")
	if err != nil || !strings.HasPrefix(p, filepath.Join(other, "cache", "videos")+string(filepath.Separator)) || !strings.HasPrefix(filepath.Base(p), "video_") {
		t.Fatalf("cached %s %v", p, err)
	}
}
