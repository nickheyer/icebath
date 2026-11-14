package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Resets the configured preview length around a display case
func resetPreviewLen(t *testing.T) {
	t.Helper()
	SetToolPreviewMaxLen(0)
	t.Cleanup(func() { SetToolPreviewMaxLen(0) })
}

func TestBuildToolPreviewNoneArgsReturnsNone(t *testing.T) {
	resetPreviewLen(t)
	if got := BuildToolPreview(nil, "terminal", nil, nil); got != "" {
		t.Fatalf("preview %q", got)
	}
}

func TestBuildToolPreviewEmptyDictReturnsNone(t *testing.T) {
	resetPreviewLen(t)
	if got := BuildToolPreview(nil, "terminal", map[string]any{}, nil); got != "" {
		t.Fatalf("preview %q", got)
	}
}

func TestBuildToolPreviewBrowserTypePreviewRedactsApiKey(t *testing.T) {
	resetPreviewLen(t)
	secret := "sk-proj-ABCD1234567890EFGH"
	got := BuildToolPreview(nil, "browser_type", map[string]any{"ref": "@e3", "text": secret}, nil)
	if got == "" || strings.Contains(got, secret) || !strings.Contains(got, "sk-pro") || !strings.Contains(got, "...") {
		t.Fatalf("preview %q", got)
	}
}

func TestBuildToolPreviewBrowserTypePreviewKeepsNormalText(t *testing.T) {
	resetPreviewLen(t)
	text := "hello world search query"
	if got := BuildToolPreview(nil, "browser_type", map[string]any{"ref": "@e3", "text": text}, nil); !strings.Contains(got, text) {
		t.Fatalf("preview %q", got)
	}
}

func TestBuildToolPreviewBrowserTypeDisplayArgsRedactApiKey(t *testing.T) {
	resetPreviewLen(t)
	secret := "ghp_ABCDEFGHIJ1234567890"
	safe := RedactToolArgsForDisplay(nil, "browser_type", map[string]any{"ref": "@e3", "text": secret})
	if strings.Contains(fmt.Sprint(safe), secret) || safe["ref"] != "@e3" || !strings.HasPrefix(safe["text"].(string), "ghp_AB") {
		t.Fatalf("display args %v", safe)
	}
}

func TestBuildToolPreviewDelegateTaskBatchPreviewRespectsMaxLen(t *testing.T) {
	resetPreviewLen(t)
	thirty := 30
	got := BuildToolPreview(nil, "delegate_task", map[string]any{"tasks": []any{
		map[string]any{"goal": strings.Repeat("A", 80)}, map[string]any{"goal": strings.Repeat("B", 80)}}}, &thirty)
	if got == "" || pystr.Len(got) > 30 {
		t.Fatalf("preview %q", got)
	}
}

func TestBuildToolPreviewFalseLikeArgsZero(t *testing.T) {
	resetPreviewLen(t)
	if got := BuildToolPreview(nil, "terminal", nil, nil); got != "" {
		t.Fatalf("preview of absent args %q", got)
	}
}

func TestBuildToolPreviewTinyMaxLenNeverExceeded(t *testing.T) {
	resetPreviewLen(t)
	long := "abcdefghijklmnopqrstuvwxyz"
	for maxLen := 1; maxLen <= 4; maxLen++ {
		limit := maxLen
		for tool, args := range map[string]map[string]any{"terminal": {"command": long}, "web_search": {"query": long}} {
			preview := BuildToolPreview(nil, tool, args, &limit)
			if preview == "" || pystr.Len(preview) > maxLen {
				t.Fatalf("%s at %d gave %q", tool, maxLen, preview)
			}
		}
		SetToolPreviewMaxLen(maxLen)
		if got := cutePath("/" + long + "/file.py"); pystr.Len(got) > maxLen {
			t.Fatalf("cute path at %d gave %q", maxLen, got)
		}
		SetToolPreviewMaxLen(0)
	}
}

func TestPrepareToolPreviewRecoversAndDescribesTruncatedUrl(t *testing.T) {
	resetPreviewLen(t)
	url := "https://example.com/a/very/long/path/to/a/page"
	SetToolPreviewMaxLen(20)
	preview := PrepareToolPreview(nil, "web_extract", map[string]any{"urls": []any{url}}, url[:17]+"...", 20)
	if preview.Text != url[:17]+"..." || !preview.Truncated || preview.URL != url {
		t.Fatalf("preview %+v", preview)
	}
}

func TestPrepareToolPreviewUntruncatedUrlHasNoLinkTarget(t *testing.T) {
	resetPreviewLen(t)
	url := "https://example.com/page"
	preview := PrepareToolPreview(nil, "browser_navigate", nil, url, 40)
	if preview.Text != url || preview.Truncated || preview.URL != "" {
		t.Fatalf("preview %+v", preview)
	}
}

func TestPrepareToolPreviewTruncatedNonUrlHasNoLinkTarget(t *testing.T) {
	resetPreviewLen(t)
	preview := PrepareToolPreview(nil, "web_search", map[string]any{"query": "how to parse a URL"}, "how to parse a URL", 12)
	if !preview.Truncated || preview.URL != "" {
		t.Fatalf("preview %+v", preview)
	}
}

func TestEditDiffPreviewExtractEditDiffUsesLocalSnapshotForWriteFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := CaptureLocalEditSnapshot(nil, "write_file", map[string]any{"path": target}, nil)
	if err := os.WriteFile(target, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := ExtractEditDiff("write_file", `{"bytes_written": 4}`, map[string]any{"path": target}, snapshot)
	for _, want := range []string{"--- a/", "+++ b/", "-old", "+new"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff lacks %q:\n%s", want, diff)
		}
	}
}

func TestEditDiffPreviewRenderEditDiffWithDeltaHandlesRendererErrors(t *testing.T) {
	saved := summarizeDiffSectionsFn
	summarizeDiffSectionsFn = func(context.Context, string) ([]string, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { summarizeDiffSectionsFn = saved })
	printed := 0
	rendered := RenderEditDiffWithDelta(context.Background(), "patch", `{"diff": "--- a/x\n+++ b/x\n"}`, nil, nil, func(string) { printed++ })
	if rendered || printed != 0 {
		t.Fatalf("rendered %v printed %d", rendered, printed)
	}
}

func TestEditDiffPreviewSummarizeRenderedDiffSectionsLimitsFileCount(t *testing.T) {
	var diff strings.Builder
	for i := range 8 {
		fmt.Fprintf(&diff, "--- a/file%d.py\n+++ b/file%d.py\n+line%d\n", i, i, i)
	}
	rendered := summarizeRenderedDiffSections(context.Background(), diff.String(), 3, 50)
	joined := strings.Join(rendered, "\n")
	for _, want := range []string{"a/file0.py", "a/file1.py", "a/file2.py"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rendered lacks %s: %q", want, rendered)
		}
	}
	if strings.Contains(joined, "a/file7.py") || !strings.Contains(rendered[len(rendered)-1], "additional file") {
		t.Fatalf("rendered %q", rendered)
	}
}

func TestBuildToolLabelDisabledFallsBackToPreview(t *testing.T) {
	resetPreviewLen(t)
	SetFriendlyToolLabels(false)
	t.Cleanup(func() { SetFriendlyToolLabels(true) })
	args := map[string]any{"query": "weather in NYC"}
	label := BuildToolLabel(nil, "web_search", args, nil)
	if label != BuildToolPreview(nil, "web_search", args, nil) || strings.Contains(label, "Searching the web") {
		t.Fatalf("label %q", label)
	}
}

func TestBuildStatusPhraseCapsLengthForSlackStatusLine(t *testing.T) {
	SetFriendlyToolLabels(true)
	phrase := BuildStatusPhrase(nil, "terminal", map[string]any{"command": strings.Repeat("x", 300)}, 49)
	if phrase == "" || pystr.Len(phrase) > 49 || !strings.HasSuffix(phrase, "\u2026") {
		t.Fatalf("phrase %q", phrase)
	}
}

func TestBuildStatusPhraseRespectsFriendlyLabelsToggle(t *testing.T) {
	SetFriendlyToolLabels(false)
	t.Cleanup(func() { SetFriendlyToolLabels(true) })
	if phrase := BuildStatusPhrase(nil, "terminal", map[string]any{"command": "ls"}, 49); phrase != "" {
		t.Fatalf("phrase %q", phrase)
	}
}

func TestGetToolEmojiCustomDefault(t *testing.T) {
	SetSkinToolEmojis(nil)
	if got := GetToolEmoji(&Env{Home: t.TempDir()}, "x", "\u2699\ufe0f"); got != "\u2699\ufe0f" {
		t.Fatalf("emoji %q", got)
	}
}

func TestGetToolEmojiSkinOverrideOnlyForMatchingTool(t *testing.T) {
	env := &Env{Home: t.TempDir()}
	if err := Default.RegisterScoped(ScopeKey(envHome(env)), Def{Name: "web_search", Toolset: "web", Emoji: "\U0001f50d", Handler: func(context.Context, *Call) string { return "" }, Schema: map[string]any{"name": "web_search", "description": "search", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}); err != nil {
		t.Fatal(err)
	}
	SetSkinToolEmojis(func() map[string]string { return map[string]string{"terminal": "\u2694"} })
	t.Cleanup(func() { SetSkinToolEmojis(nil) })
	if got := GetToolEmoji(env, "terminal", "\u26a1"); got != "\u2694" {
		t.Fatalf("skin override %q", got)
	}
	if got := GetToolEmoji(env, "web_search", "\u26a1"); got != "\U0001f50d" {
		t.Fatalf("registry fallback %q", got)
	}
}
