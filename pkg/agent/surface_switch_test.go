package agent

import (
	"context"
	"strings"
	"testing"
)

func surfacePrompt(platform string) string {
	return "identity\n\nmemory\nPlatform: memory-says-cron\n\nConversation started: x\nModel: m\nPlatform: " + platform +
		"\n\n" + RuntimeEnvironmentHeading + "\n\nUser home directory: /home/u\nCurrent working directory: /work\n\nembedder prose\nPlatform: prose\n\n" + RuntimeEnvironmentEnd
}

func TestSurfaceSplitRuntimeBoundary(t *testing.T) {
	p := surfacePrompt("cli")
	identity, marker, runtime := SplitRuntimeBoundary(p)
	if !strings.HasSuffix(identity, "Platform: cli") || marker == "" || !strings.HasPrefix(runtime, "User home directory:") {
		t.Fatalf("split %q %q %q", identity, marker, runtime)
	}
	legacy := "identity\n\n" + RuntimeEnvironmentHeading + "\n\nquoted"
	if a, b, c := SplitRuntimeBoundary(legacy); a != legacy || b != "" || c != "" {
		t.Fatal("a prompt not ending in the boundary is all identity")
	}
	if a, b, c := SplitRuntimeBoundary("no marker " + RuntimeEnvironmentEnd); a != "" || b != "" || c != "no marker "+RuntimeEnvironmentEnd {
		t.Fatal("rpartition without the marker keeps the whole prompt as runtime")
	}
}

func TestSurfaceRuntimeHostValue(t *testing.T) {
	if got := runtimeHostValue(surfacePrompt("cli"), "Current working directory"); got != "/work" {
		t.Fatalf("new prompt %q", got)
	}
	legacy := "identity\nUser home directory: /h\nOS: linux\nCurrent working directory:  /legacy \n\nmore"
	if got := runtimeHostValue(legacy, "Current working directory"); got != "/legacy" {
		t.Fatalf("legacy prompt %q", got)
	}
	far := "User home directory: /h\na\nb\nc\nCurrent working directory: /far"
	if runtimeHostValue(far, "Current working directory") != "" || runtimeHostValue("nothing", "X") != "" {
		t.Fatal("only the three lines after the home line are host lines")
	}
}

func TestSurfaceIdentityLineValueReadsOnlyTheTrailer(t *testing.T) {
	if got := IdentityLineValue(surfacePrompt("desktop"), "Platform"); got != "desktop" {
		t.Fatalf("platform %q", got)
	}
	noTrailerLabel := "memory\nPlatform: memory-says-cron\n\nConversation started: x\nModel: m"
	if IdentityLineValue(noTrailerLabel, "Platform") != "" {
		t.Fatal("earlier paragraphs are never identity")
	}
	if IdentityLineValue("a\n\nPlatform: one\rPlatform: two", "Platform") != "two" {
		t.Fatal("the last trailer match wins over Python line boundaries")
	}
}

func TestSurfaceLastAnnouncedSurface(t *testing.T) {
	note := surfaceSwitchNotePrefix + "tui" + surfaceNameEnd + " in this conversation is superseded"
	history := []map[string]any{
		{"role": "user", "content": "hi", "api_content": "hi\n" + note},
		{"role": "assistant", "content": surfaceSwitchNotePrefix + "assistant" + surfaceNameEnd},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": surfaceSwitchNotePrefix + "my plugin" + surfaceNameEnd + " x"}}},
	}
	if got := lastAnnouncedSurface(history); got != "my plugin" {
		t.Fatalf("newest note %q", got)
	}
	if got := lastAnnouncedSurface(history[:2]); got != "tui" {
		t.Fatalf("sidecar note %q", got)
	}
	padded := append([]map[string]any{}, history[0])
	for range surfaceNoteScanTail {
		padded = append(padded, map[string]any{"role": "user", "content": "x"})
	}
	if lastAnnouncedSurface(padded) != "" || lastAnnouncedSurface(nil) != "" {
		t.Fatal("only the bounded tail is scanned")
	}
}

func TestSurfaceStageSwitchNote(t *testing.T) {
	ctx := context.Background()
	a := &AIAgent{platform: "cli", sessionID: "s"}
	if StageSurfaceSwitchNote(ctx, a, surfacePrompt("cli"), nil) || a.surfaceSwitchNote != "" {
		t.Fatal("an unchanged surface stages nothing")
	}
	if !StageSurfaceSwitchNote(ctx, a, surfacePrompt("desktop"), nil) {
		t.Fatal("a switched surface stages a note")
	}
	if !strings.HasPrefix(a.surfaceSwitchNote, surfaceSwitchNotePrefix+"cli"+surfaceNameEnd) || !strings.Contains(a.surfaceSwitchNote, "the guidance below") ||
		!strings.HasSuffix(a.surfaceSwitchNote, "\n"+PlatformHint(ctx, a)) {
		t.Fatalf("note %q", a.surfaceSwitchNote)
	}
	back := &AIAgent{platform: "cli"}
	stale := []map[string]any{{"role": "user", "content": "x", "api_content": surfaceSwitchNotePrefix + "tui" + surfaceNameEnd}}
	if !StageSurfaceSwitchNote(ctx, back, surfacePrompt("cli"), stale) || !strings.Contains(back.surfaceSwitchNote, "the interface section in the system prompt above") ||
		strings.Contains(back.surfaceSwitchNote, "\n") {
		t.Fatalf("switch back note %q", back.surfaceSwitchNote)
	}
	for _, skip := range []*AIAgent{{platform: "tui", provider: "moa"}, {platform: "tui", apiMode: "codex_app_server"}, {platform: " "}} {
		if StageSurfaceSwitchNote(ctx, skip, surfacePrompt("cli"), nil) {
			t.Fatalf("staged for %+v", skip.provider)
		}
	}
}

func TestSurfaceNoteInertPinnedTools(t *testing.T) {
	prev := agentToolNamesFn
	t.Cleanup(func() { agentToolNamesFn = prev })
	agentToolNamesFn = func(*AIAgent) []string { return []string{"terminal", "focus_pane", "read_file"} }
	a := &AIAgent{}
	NoteInertPinnedTools(a, []string{"terminal"})
	if a.surfaceSwitchNote != "" {
		t.Fatal("no staged note means nothing to extend")
	}
	a.surfaceSwitchNote = "note"
	NoteInertPinnedTools(a, []string{"terminal", "read_file", "focus_pane"})
	if a.surfaceSwitchNote != "note" {
		t.Fatal("every tool built leaves the note alone")
	}
	NoteInertPinnedTools(a, []string{"terminal"})
	if !strings.HasSuffix(a.surfaceSwitchNote, "expect a call to one of them to fail: focus_pane, read_file.]") {
		t.Fatalf("note %q", a.surfaceSwitchNote)
	}
}
