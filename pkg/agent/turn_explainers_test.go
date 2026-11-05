package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func explainerHome(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_LANGUAGE", "en")
	return context.Background()
}

func TestNeutralizeFooterPathsBackticksBarePathsOnce(t *testing.T) {
	cases := map[string]string{
		"wrote /etc/passwd.txt and ~/notes/a.md": "wrote `/etc/passwd.txt` and `~/notes/a.md`",
		"already `/tmp/x.log` here":              "already `/tmp/x.log` here",
		`see C:\Users\me\file.txt`:               "see `C:\\Users\\me\\file.txt`",
		"no path here":                           "no path here",
		"url https://example.com/a.html":         "url https://example.com/a.html",
		"":                                       "",
	}
	for in, want := range cases {
		if got := neutralizeFooterPaths(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestFileMutationFooterListsTenThenOverflow(t *testing.T) {
	ctx := explainerHome(t)
	if got := formatFileMutationFailureFooter(ctx, NewFileMutationFailures()); got != "" {
		t.Fatalf("empty: %q", got)
	}
	failed := NewFileMutationFailures()
	for i := 0; i < 12; i++ {
		failed.SetDefault("/repo/f"+string(rune('a'+i))+".py", &FileMutationFailure{Tool: "write_file", ErrorPreview: " denied /etc/x.conf "})
	}
	failed.SetDefault("/repo/fa.py", &FileMutationFailure{Tool: "patch", ErrorPreview: "second error is ignored"})
	got := formatFileMutationFailureFooter(ctx, failed)
	lines := strings.Split(got, "\n")
	if len(lines) != 12 || !strings.Contains(lines[0], "12 file edit(s) FAILED") {
		t.Fatalf("footer lines: %d %q", len(lines), lines[0])
	}
	if lines[1] != "  • `/repo/fa.py` — [write_file] denied `/etc/x.conf`" {
		t.Fatalf("entry: %q", lines[1])
	}
	if lines[11] != "  • … and 2 more" {
		t.Fatalf("overflow: %q", lines[11])
	}
	blank := NewFileMutationFailures()
	blank.SetDefault("rel.txt", &FileMutationFailure{})
	if got := formatFileMutationFailureFooter(ctx, blank); !strings.HasSuffix(got, "  • `rel.txt` — [patch] failed") {
		t.Fatalf("defaults: %q", got)
	}
}

func TestTurnCompletionExplanationByExitReason(t *testing.T) {
	ctx := explainerHome(t)
	for _, reason := range []any{nil, "", "text_response(stop)", "guardrail_halt"} {
		if got := formatTurnCompletionExplanation(ctx, reason, "", "", ""); got != "" {
			t.Fatalf("%v: %q", reason, got)
		}
	}
	got := formatTurnCompletionExplanation(ctx, "empty_response_exhausted", "", "", "")
	if got != "⚠️ No reply: The model didn't produce a reply this time, even after retries. Send `continue` to try again, or switch models with /model." {
		t.Fatalf("empty: %q", got)
	}
	if got := formatTurnCompletionExplanation(ctx, "max_iterations_reached(3/3)", "", "", "m"); !strings.HasPrefix(got, "⚠️ No reply: the maximum tool-iteration limit") {
		t.Fatalf("prefix: %q", got)
	}
	got = formatTurnCompletionExplanation(ctx, "session_persistence_failed", "corrupt", "", "")
	if !strings.HasPrefix(got, "⚠️ No reply: the turn was stopped because the state database reported structural corruption") {
		t.Fatalf("corrupt: %q", got)
	}
	if got := formatTurnCompletionExplanation(ctx, "session_persistence_failed", "weird", "", ""); !strings.Contains(got, "Icebath couldn't save this conversation, so it stopped") {
		t.Fatalf("default cause: %q", got)
	}
	if got := EmptyResponseExplanationFor(ctx, "gpt-x"); !strings.HasPrefix(got, "gpt-x didn't produce a reply") {
		t.Fatalf("named model: %q", got)
	}
}

func TestDisplayFlagEnvOverridesAndConfigIsCached(t *testing.T) {
	ctx := explainerHome(t)
	h := os.Getenv("ICEBATH_HOME")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display:\n  file_mutation_verifier: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{}
	if a.fileMutationVerifierEnabled(ctx) {
		t.Fatal("config false disables the verifier")
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display:\n  file_mutation_verifier: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a.fileMutationVerifierEnabled(ctx) {
		t.Fatal("the config answer is cached per agent")
	}
	t.Setenv("ICEBATH_FILE_MUTATION_VERIFIER", " On ")
	if !a.fileMutationVerifierEnabled(ctx) {
		t.Fatal("the env var wins on every call")
	}
	t.Setenv("ICEBATH_TURN_COMPLETION_EXPLAINER", "off")
	if a.turnCompletionExplainerEnabled(ctx) {
		t.Fatal("off disables the explainer")
	}
}

func TestRecordFileMutationResultKeepsFirstFailureUntilALaterSuccess(t *testing.T) {
	explainerHome(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.md")
	a := &AIAgent{Env: &tools.Env{Home: os.Getenv("ICEBATH_HOME")}}
	if err := a.recordFileMutationResult("write_file", map[string]any{"path": target}, `{"error": "denied"}`, true, ""); err != nil {
		t.Fatal(err)
	}
	a.turnFailedFileMutations = NewFileMutationFailures()
	a.turnFileMutationPaths = map[string]bool{}
	if err := a.recordFileMutationResult("read_file", map[string]any{"path": target}, "x", true, ""); err != nil || a.turnFailedFileMutations.Len() != 0 {
		t.Fatalf("non mutating tools are ignored: %v", err)
	}
	if err := a.recordFileMutationResult("write_file", map[string]any{"path": target}, `{"error": "denied"}`, true, ""); err != nil {
		t.Fatal(err)
	}
	info, ok := a.turnFailedFileMutations.Get(target)
	if !ok || info.Tool != "write_file" || !info.HasStat || info.Stat != nil || info.Identity != filepath.Clean(target) {
		t.Fatalf("failure: %+v", info)
	}
	if still := fileMutationsStillFailed(a.turnFailedFileMutations); still.Len() != 1 {
		t.Fatal("an untouched target stays failed")
	}
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if still := fileMutationsStillFailed(a.turnFailedFileMutations); still.Len() != 0 {
		t.Fatal("a target created later this turn drops out")
	}
	ok2 := `{"success": true, "bytes_written": 1}`
	if err := a.recordFileMutationResult("write_file", map[string]any{"path": target}, ok2, false, ""); err != nil {
		t.Fatal(err)
	}
	if a.turnFailedFileMutations.Len() != 0 {
		t.Fatalf("a later success clears the failure: %v", a.turnFailedFileMutations.Paths())
	}
}
