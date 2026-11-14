package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	out := tuigateway.BuildImageRefMessage("what is this?", []string{img})
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
	out := tuigateway.BuildImageRefMessage("caption", paths)
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
	out := tuigateway.BuildImageRefMessage("hi", []string{gone, img})
	if strings.Contains(out, gone) || !strings.Contains(out, img) {
		t.Fatalf("unexpected %q", out)
	}
}

// Agent stand in reporting a fixed route
type routeFake struct{ provider, model string }

func (r routeFake) Provider() string { return r.provider }
func (r routeFake) Model() string    { return r.model }

func TestLiveAgentIdentityWinsAfterModelSwitch(t *testing.T) {
	oldP, oldM := readMainProvider, readMainModel
	t.Cleanup(func() { readMainProvider, readMainModel = oldP, oldM })
	readMainProvider = func(context.Context) string { panic("stale provider fallback used") }
	readMainModel = func(context.Context) string { panic("stale model fallback used") }
	p, m := activeImageRoutingIdentity(context.Background(), routeFake{"alibaba", "qwen3.7-plus"})
	if p != "alibaba" || m != "qwen3.7-plus" {
		t.Fatalf("got %q %q", p, m)
	}
}

func TestMissingAgentIdentityUsesRuntimeFallback(t *testing.T) {
	oldP, oldM := readMainProvider, readMainModel
	t.Cleanup(func() { readMainProvider, readMainModel = oldP, oldM })
	readMainProvider = func(context.Context) string { return "openai-codex" }
	readMainModel = func(context.Context) string { return "gpt-5.5-codex" }
	p, m := activeImageRoutingIdentity(context.Background(), routeFake{"", ""})
	if p != "openai-codex" || m != "gpt-5.5-codex" {
		t.Fatalf("got %q %q", p, m)
	}
}

// Exception with no message whose Python type is KeyError
type keyErrorValue struct{}

func (keyErrorValue) Error() string      { return "" }
func (keyErrorValue) PyTypeName() string { return "KeyError" }

func TestNothingToSayProducesNothing(t *testing.T) {
	if got := turnFailureDetail("", nil, nil); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := turnFailureDetail(nil, nil, nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestAnExceptionWithNoMessageStillNamesItsType(t *testing.T) {
	if got := turnFailureDetail(keyErrorValue{}, nil, nil); !strings.Contains(got, "KeyError") {
		t.Fatalf("got %q", got)
	}
}

func TestThePromptArgumentIsOptional(t *testing.T) {
	if got := turnFailureDetail("Bearer sk-proj-supersecretvalue1234", nil, nil); strings.Contains(got, "supersecretvalue1234") {
		t.Fatalf("leaked %q", got)
	}
}

func TestABrokenRedactorFailsClosed(t *testing.T) {
	old := redactSensitiveForced
	t.Cleanup(func() { redactSensitiveForced = old })
	redactSensitiveForced = func(string) string { panic("redactor unavailable") }
	got := turnFailureDetail("Bearer sk-proj-supersecretvalue", nil, nil)
	if strings.Contains(got, "supersecretvalue") || !strings.Contains(got, "unredactable") {
		t.Fatalf("got %q", got)
	}
}

func TestAnOverlapBelowTheWindowIsNotAnEcho(t *testing.T) {
	if got := stripPromptEcho("400: invalid model", "invalid model"); got != "400: invalid model" {
		t.Fatalf("got %q", got)
	}
}

func TestAJSONEscapedEchoIsStrippedToo(t *testing.T) {
	prompt := "please summarise the Q3 marmalade inventory memo for me"
	message := `upstream body: {"messages": [{"content": "` + prompt + `"}]}`
	got := stripPromptEcho(message, prompt)
	if strings.Contains(got, "marmalade") || !strings.Contains(got, "<prompt>") {
		t.Fatalf("got %q", got)
	}
}

func TestAnEchoIsRemovedBeforeTheLengthCapApplies(t *testing.T) {
	prompt := "the confidential merger memorandum for the northern division"
	got := turnFailureDetail(strings.Repeat("x", 200)+" echoed request: "+prompt, nil, prompt)
	if strings.Contains(got, "merger memorandum") || strings.Contains(got, "confidential") {
		t.Fatalf("got %q", got)
	}
}

func TestAPromptShorterThanTheWindowCannotBlankTheMessage(t *testing.T) {
	if got := stripPromptEcho("provider said no", "hi"); got != "provider said no" {
		t.Fatalf("got %q", got)
	}
}

func TestWhitespaceShapeDoesNotHideAnEcho(t *testing.T) {
	prompt := "the marmalade inventory for Q3 was discontinued in March"
	got := turnFailureDetail("rejected:    the marmalade inventory\n for Q3 was discontinued in March", nil, prompt)
	if strings.Contains(got, "marmalade") {
		t.Fatalf("got %q", got)
	}
}
