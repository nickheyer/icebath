package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeMediaFile(t *testing.T, path string, data string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mediaPaths(t *testing.T, text string) ([]string, string) {
	t.Helper()
	media, cleaned, err := ExtractMedia(t.Context(), text)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range media {
		out = append(out, m.Path)
	}
	return out, cleaned
}

func TestMultipartExtensionNotTruncated(t *testing.T) {
	targz := writeMediaFile(t, filepath.Join(t.TempDir(), "archive.tar.gz"), "\x1f\x8b")
	if got, _ := mediaPaths(t, "MEDIA:"+targz); !reflect.DeepEqual(got, []string{targz}) {
		t.Fatalf("got %q", got)
	}
}

func TestRealPathInInlineCodeDelivers(t *testing.T) {
	file := writeMediaFile(t, filepath.Join(t.TempDir(), "data.csv"), "x,y\n1,2\n")
	got, cleaned := mediaPaths(t, "Here is your file `MEDIA:"+file+"`")
	if !reflect.DeepEqual(got, []string{file}) || strings.Contains(cleaned, "MEDIA:") {
		t.Fatalf("got %q cleaned %q", got, cleaned)
	}
}

func TestBoldWrappedTagDelivers(t *testing.T) {
	file := writeMediaFile(t, filepath.Join(t.TempDir(), "data.csv"), "x,y\n1,2\n")
	got, cleaned := mediaPaths(t, "**MEDIA:"+file+"**")
	if !reflect.DeepEqual(got, []string{file}) || strings.Contains(cleaned, "MEDIA:") {
		t.Fatalf("got %q cleaned %q", got, cleaned)
	}
}

func TestTwoTagsOneLineBothDeliver(t *testing.T) {
	dir := t.TempDir()
	a := writeMediaFile(t, filepath.Join(dir, "a.csv"), "1")
	b := writeMediaFile(t, filepath.Join(dir, "b.csv"), "2")
	if got, _ := mediaPaths(t, "MEDIA:"+a+" MEDIA:"+b); !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("got %q", got)
	}
}

func TestStripMediaDirectivesHandlesGluedKnownExtensionTags(t *testing.T) {
	dir := t.TempDir()
	png1 := writeMediaFile(t, filepath.Join(dir, "a.png"), "\x89PNG\r\n\x1a\n")
	png2 := writeMediaFile(t, filepath.Join(dir, "b.png"), "\x89PNG\r\n\x1a\n")
	cleaned, err := stripMediaTagDirectives(t.Context(), "MEDIA:"+png1+"MEDIA:"+png2)
	if err != nil || strings.Contains(cleaned, "MEDIA:") {
		t.Fatalf("Greedy merge leaked: %q %v", cleaned, err)
	}
}

func TestTerminalEOSSentinelLeavesExtractionAndCleanupUnchanged(t *testing.T) {
	for _, sentinel := range []string{"<|eos|>", "<|eos|><|eos|>"} {
		for _, filename := range []string{"chart.png", "payload.weirdext", "Caddyfile"} {
			root := filepath.Join(t.TempDir(), "media-cache")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			mediaFile := writeMediaFile(t, filepath.Join(root, filename), "media")
			saved := mediaDeliverySafeRoots
			mediaDeliverySafeRoots = func() []string { return []string{root} }
			clean := "Here is the file.\nMEDIA:" + mediaFile
			m1, c1, err1 := ExtractMedia(t.Context(), clean+sentinel)
			m2, c2, err2 := ExtractMedia(t.Context(), clean)
			display, err3 := StripMediaDirectivesForDisplay(t.Context(), clean+sentinel)
			mediaDeliverySafeRoots = saved
			if err1 != nil || err2 != nil || err3 != nil {
				t.Fatal(err1, err2, err3)
			}
			if !reflect.DeepEqual(m1, m2) || c1 != c2 || len(m2) != 1 {
				t.Errorf("%s %s: %v %q vs %v %q", filename, sentinel, m1, c1, m2, c2)
			}
			if display != "Here is the file." {
				t.Errorf("%s %s: display %q", filename, sentinel, display)
			}
		}
	}
}

func TestNonTerminalOrProtectedSentinelIsNotAMediaBoundary(t *testing.T) {
	for _, text := range []string{
		"MEDIA:/tmp/example.png<|EOS|>",
		"MEDIA:/tmp/example.png<|eos|> trailing prose",
		"```text\nMEDIA:/tmp/example.png<|eos|>\n```",
		`{"example":"MEDIA:/tmp/example.png<|eos|>"}`,
	} {
		media, cleaned, err := ExtractMedia(t.Context(), text)
		if err != nil || len(media) != 0 || cleaned != text {
			t.Errorf("%q: %v %q %v", text, media, cleaned, err)
		}
		if display, err := StripMediaDirectivesForDisplay(t.Context(), text); err != nil || display != text {
			t.Errorf("%q: display %q %v", text, display, err)
		}
	}
}
