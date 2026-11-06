package state

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func msgObj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func TestBackgroundReviewHarnessDetectionAndStrip(t *testing.T) {
	for _, tc := range []struct {
		msg  *pyjson.Object
		want bool
	}{
		{msgObj("role", "user", "content", "Review the conversation above and update the skill library now."), true},
		{msgObj("role", "system", "content", "  Review the conversation above and consider saving to memory."), true},
		{msgObj("role", "user", "content", "Please review my PR and update the changelog."), false},
		{msgObj("role", "assistant", "content", "Review the conversation above and update the skill library"), false},
	} {
		if got, err := isBackgroundReviewHarnessMessage(tc.msg); got != tc.want || err != nil {
			t.Fatalf("%v %v %v", tc.msg.Vals, got, err)
		}
	}
	if _, err := isBackgroundReviewHarnessMessage(msgObj("role", []any{"user"})); err == nil || !strings.Contains(err.Error(), "unhashable type: 'list'") {
		t.Fatalf("unhashable role %v", err)
	}
	messages := []any{
		msgObj("role", "user", "content", "What's the weather?"),
		msgObj("role", "assistant", "content", "It's sunny."),
		msgObj("role", "user", "content", "Review the conversation above and update the skill library."),
		msgObj("role", "assistant", "content", "Nothing to save."),
		msgObj("role", "user", "content", "Thanks, now book a flight."),
	}
	out, err := stripBackgroundReviewHarness(messages)
	var contents []string
	for _, m := range out {
		c, _ := m.(*pyjson.Object).Get("content")
		contents = append(contents, c.(string))
	}
	if err != nil || strings.Join(contents, "|") != "What's the weather?|It's sunny.|Thanks, now book a flight." {
		t.Fatalf("%v %v", contents, err)
	}
	consecutive := []any{
		msgObj("role", "user", "content", "Review the conversation above and update the skill library."),
		msgObj("role", "user", "content", "Review the conversation above and consider saving to memory."),
		msgObj("role", "assistant", "content", "The user's real reply."),
	}
	if out, _ := stripBackgroundReviewHarness(consecutive); len(out) != 1 || out[0] != consecutive[2] {
		t.Fatalf("consecutive %v", out)
	}
	if out, _ := stripBackgroundReviewHarness(nil); out != nil {
		t.Fatal("empty input returned a new list")
	}
}

func TestStaleToolCallMarkersBlankedInPlace(t *testing.T) {
	buf := captureSlog(t)
	stale := msgObj("role", "assistant", "content", " [skill_manage] ", "tool_calls", []any{msgObj("id", "c1")})
	keep := []*pyjson.Object{
		msgObj("role", "assistant", "content", "[memory]", "tool_calls", []any{}),
		msgObj("role", "user", "content", "[memory]", "tool_calls", []any{1}),
		msgObj("role", "assistant", "content", "[not a marker!]", "tool_calls", []any{1}),
	}
	messages := []any{stale, keep[0], keep[1], keep[2], "raw"}
	stripStaleToolCallMarkers(messages)
	if c, _ := stale.Get("content"); c != "" {
		t.Fatalf("stale %v", c)
	}
	for _, k := range keep {
		if c, _ := k.Get("content"); c == "" {
			t.Fatalf("blanked %v", k.Vals)
		}
	}
	if !strings.Contains(buf.String(), "Cleared 1 stale tool-call marker message(s) while restoring session (#78148)") {
		t.Fatal(buf.String())
	}
}

func TestCompressionLockHolderProcessIsDead(t *testing.T) {
	if compressionLockHolderProcessIsDead(fmt.Sprintf("pid=%d", os.Getpid())) {
		t.Fatal("own pid reported dead")
	}
	token := HolderNamespaceToken()
	for _, holder := range []string{"", "thread-7", "pid=0", "pid=99999999999999999999", "pidx=4"} {
		if compressionLockHolderProcessIsDead(holder + token) {
			t.Fatalf("%q reported dead", holder)
		}
	}
	if compressionLockHolderProcessIsDead(fmt.Sprintf("host:pid=%d%s", os.Getppid(), token)) {
		t.Fatal("live parent reported dead")
	}
	if HolderPidCheckable("pid=4194303"+token) && !compressionLockHolderProcessIsDead("host:pid=4194303"+token) {
		t.Fatal("absent pid kept")
	}
}

func TestSecureStateDBFilesTightensRegularFilesOnly(t *testing.T) {
	if os.PathSeparator == '\\' {
		return
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := secureStateDBFiles(db, true); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(db); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("created %v %v", st, err)
	}
	os.WriteFile(db+"-wal", nil, 0o644)
	target := filepath.Join(dir, "elsewhere")
	os.WriteFile(target, nil, 0o644)
	os.Symlink(target, db+"-shm")
	os.Chmod(db, 0o644)
	if err := secureStateDBFiles(db, true); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{db: 0o600, db + "-wal": 0o600, target: 0o644} {
		if st, _ := os.Stat(path); st.Mode().Perm() != want {
			t.Fatalf("%s %v", path, st.Mode())
		}
	}
	if err := secureStateDBFiles(dir, true); err != nil {
		t.Fatalf("directory main %v", err)
	}
}

func TestTranscriptLimitsAndRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		yaml       string
		resume, ex int
	}{
		{"", 20000, 20000},
		{"sessions:\n  max_resume_messages: 0\n  max_export_messages: '500'\n", 0, 500},
		{"sessions:\n  max_resume_messages: -3\n  max_export_messages: 7.9\n", 20000, 7},
		{"sessions: [1]\n", 20000, 20000},
		{"sessions:\n  max_resume_messages: lots\n", 20000, 20000},
	} {
		writeHomeConfig(t, tc.yaml)
		if r, e := ResolvedMaxResumeMessages(ctx), ResolvedMaxExportMessages(ctx); r != tc.resume || e != tc.ex {
			t.Fatalf("%q %d %d", tc.yaml, r, e)
		}
	}
	if got := NewSessionResumeTooLargeError(25000).Error(); !strings.HasPrefix(got, "This session is too long to reload safely (25000 messages; limit 20000).") {
		t.Fatal(got)
	}
	if got := (&SessionExportTooLargeError{SessionID: "s1", MessageCount: 30000, Limit: 20000}).Error(); !strings.HasPrefix(got, "Session 's1' has more than 20,000 exportable messages;") {
		t.Fatal(got)
	}
}

func TestEnsureTestIsolationRefusesDeniedRoots(t *testing.T) {
	root := t.TempDir()
	pinVar(t, &stateDBGuardExtraDenyRoots, []string{root})
	t.Setenv(stateDBGuardBypassEnv, "")
	err := ensureTestIsolation(filepath.Join(root, "state.db"))
	if err == nil || !strings.Contains(err.Error(), "live-system guard: test attempted to open production state.db at") {
		t.Fatalf("%v", err)
	}
	if err := ensureTestIsolation(filepath.Join(t.TempDir(), "state.db")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stateDBGuardBypassEnv, "1")
	if err := ensureTestIsolation(filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stateDBGuardBypassEnv, "")
	pinVar(t, &stateDBGuardBypass, true)
	if err := ensureTestIsolation(filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
}
