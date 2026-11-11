package tuiserver

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Writes data to name under dir, creating dir
func writeImageAt(t *testing.T, dir, name string, data string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildPersistMessageWithImageRefsWithoutImagesReturnsText(t *testing.T) {
	if got := tuigateway.BuildPersistMessageWithImageRefs("what is this?", nil); got != "what is this?" {
		t.Fatalf("got %q", got)
	}
	if got := tuigateway.BuildPersistMessageWithImageRefs("", nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildPersistMessageWithImageRefsAppendsExistingPaths(t *testing.T) {
	img := writeImageAt(t, t.TempDir(), "cat.png", "\x89PNG")
	if got := tuigateway.BuildPersistMessageWithImageRefs("what is in this photo?", []string{img}); got != "what is in this photo?\n@image:"+img {
		t.Fatalf("got %q", got)
	}
}

func TestBuildPersistMessageWithImageRefsSkipsMissingPaths(t *testing.T) {
	dir := t.TempDir()
	existing := writeImageAt(t, dir, "a.png", "png")
	missing := filepath.Join(dir, "gone.png")
	if got := tuigateway.BuildPersistMessageWithImageRefs("compare them", []string{existing, missing}); got != "compare them\n@image:"+existing {
		t.Fatalf("got %q", got)
	}
}

func TestBuildPersistMessageWithImageRefsWithoutTextIsRefsOnly(t *testing.T) {
	img := writeImageAt(t, t.TempDir(), "only.png", "png")
	if got := tuigateway.BuildPersistMessageWithImageRefs("", []string{img}); got != "@image:"+img {
		t.Fatalf("got %q", got)
	}
}

func TestBuildPersistMessageQuotesPathsContainingSpaces(t *testing.T) {
	img := writeImageAt(t, filepath.Join(t.TempDir(), "Application Support", "Hermes", "composer-images"), "cat.png", "png")
	if got := tuigateway.BuildPersistMessageWithImageRefs("what is this?", []string{img}); got != "what is this?\n@image:`"+img+"`" {
		t.Fatalf("got %q", got)
	}
}

func TestPersistUserMessageMirrorsTheShapeSentToTheModel(t *testing.T) {
	img := writeImageAt(t, t.TempDir(), "cat.png", "png")
	imagePart := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}
	native := []any{map[string]any{"type": "text", "text": "api-only text"}, imagePart}
	want := []any{map[string]any{"type": "text", "text": "what is this?\n@image:" + img}, imagePart}
	if got := tuigateway.BuildPersistUserMessage("what is this?", []string{img}, native); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestPersistUserMessageStaysAStringForTextMode(t *testing.T) {
	img := writeImageAt(t, t.TempDir(), "cat.png", "png")
	if got := tuigateway.BuildPersistUserMessage("what is this?", []string{img}, "enriched api-only text"); got != "what is this?\n@image:"+img {
		t.Fatalf("got %v", got)
	}
}
