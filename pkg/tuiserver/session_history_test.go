package tuiserver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Writes a fake image under dir and returns its path
func writeFakeImage(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNeverCallsVisionOnSubmitPath(t *testing.T) {
	img := writeFakeImage(t, t.TempDir(), "photo.jpg", []byte("\xff\xd8\xff fake jpeg"))
	out := tuihost.BuildImageRefMessage("what is this?", []string{img})
	for _, want := range []string{"what is this?", img, "vision_analyze"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestEveryAttachedImageIsReferenced(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := range 20 {
		paths = append(paths, writeFakeImage(t, dir, fmt.Sprintf("img%d.png", i), []byte("\x89PNG\r\n\x1a\n fake")))
	}
	out := tuihost.BuildImageRefMessage("caption", paths)
	for _, p := range paths {
		if !strings.Contains(out, p) {
			t.Fatalf("missing %q", p)
		}
	}
}

func TestMissingPathsSkipped(t *testing.T) {
	dir := t.TempDir()
	img := writeFakeImage(t, dir, "photo.jpg", []byte("\xff\xd8\xff fake jpeg"))
	gone := filepath.Join(dir, "nope.png")
	out := tuihost.BuildImageRefMessage("hi", []string{gone, img})
	if strings.Contains(out, gone) || !strings.Contains(out, img) {
		t.Fatalf("unexpected %q", out)
	}
}

// Agent stand in reporting a fixed route
type routeFake struct{ provider, model string }

func (r routeFake) Provider() string { return r.provider }
func (r routeFake) Model() string    { return r.model }

// Exception with no message whose Python type is KeyError
type keyErrorValue struct{}

func (keyErrorValue) Error() string     { return "" }
func (keyErrorValue) ErrorKind() string { return "KeyError" }
