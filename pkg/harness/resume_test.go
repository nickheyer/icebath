package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Saves a session of the project with one message and the labels
func saved(t *testing.T, st *session.Store, project string, labels map[string]string) string {
	t.Helper()
	s := session.New()
	s.Label(session.ProjectLabel, project)
	for k, v := range labels {
		s.Label(k, v)
	}
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}})
	if err := st.Save(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

func TestNewestSessionSkipsBusyAndSubAgents(t *testing.T) {
	dir := t.TempDir()
	st := &session.Store{Dir: dir}
	ctx := context.Background()
	if _, err := NewestSession(ctx, st, "/p/a"); err == nil {
		t.Fatal("no session should be an error")
	}
	want := saved(t, st, "/p/a", nil)
	saved(t, st, "/p/a", map[string]string{parentLabel: want})
	busy := saved(t, st, "/p/a", nil)
	saved(t, st, "/p/b", nil)
	other := &session.Store{Dir: dir}
	if err := other.Lease(busy); err != nil {
		t.Fatal(err)
	}
	defer other.Unlease(busy)
	got, err := NewestSession(ctx, st, "/p/a")
	if err != nil || got != want {
		t.Fatalf("newest session %q %v, want %q", got, err, want)
	}
}

func TestSystemPromptHoldsForRun(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	project := t.TempDir()
	sub := filepath.Join(project, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "ICEBATH.md"), []byte("sub rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := Build(ctx, cfg, func(any) {}, BuildOptions{Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	h := &systemHook{app: a, ep: a.Primary}
	if _, err := h.BeforeRun(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var first, second protocol.Request
	if err := h.BeforeTurn(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if r := a.Shell.RunWith(ctx, "cd sub", os.Stderr, os.Stderr, time.Minute); r.ExitCode != 0 {
		t.Fatalf("cd exited %d", r.ExitCode)
	}
	moved, err := a.PromptFor(ctx, a.Primary, Kind{})
	if err != nil {
		t.Fatal(err)
	}
	if moved == first.System {
		t.Fatal("moving the shell should change the prompt a new run builds")
	}
	if err := h.BeforeTurn(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if second.System != first.System {
		t.Fatal("prompt changed inside one run")
	}
	if _, err := h.BeforeRun(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.BeforeTurn(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if second.System != moved {
		t.Fatal("next run kept the old prompt")
	}
}

func TestStripThinkingRunsLast(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	project := t.TempDir()
	cfg, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profile, cfg.Model = "ollama", "test"
	ctx := context.Background()
	a, err := Build(ctx, cfg, func(any) {}, BuildOptions{Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	ag, err := a.NewAgent("", a.Session)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.Hooks[len(ag.Hooks)-1].(*agent.StripThinking); !ok {
		t.Fatalf("last hook is %T, the request it digests is not the one sent", ag.Hooks[len(ag.Hooks)-1])
	}
}
