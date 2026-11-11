package dashboard

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveChatCwdFailsClosedAndReachesTheTuiEnv(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	prev := TUILaunch
	t.Cleanup(func() { TUILaunch = prev })
	TUILaunch.MakeArgv = func(context.Context, string, bool) ([]string, string, error) {
		return []string{"node", "fake-tui.js"}, tmp, nil
	}
	TUILaunch.ApplyPythonEnv = func(map[string]string) error { return nil }

	empty := ""
	if got, err := resolveChatCwd(&empty); got != nil || err != nil {
		t.Fatalf("empty cwd: %v %v", got, err)
	}
	gone := filepath.Join(tmp, "gone")
	_, err := resolveChatCwd(&gone)
	var httpErr *HTTPException
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 400 {
		t.Fatalf("missing cwd err %v, want 400", err)
	}
	picked, err := resolveChatCwd(&tmp)
	if err != nil || picked == nil {
		t.Fatalf("picked %v err %v", picked, err)
	}
	ctx := context.Background()
	app := NewFastAPI("", "", nil, nil)
	_, _, env, err := resolveChatArgv(ctx, app, "", "", "", "", *picked)
	if err != nil {
		t.Fatal(err)
	}
	if env["ICEBATH_CWD"] != tmp || env["ICEBATH_TUI_CWD"] != tmp {
		t.Fatalf("env cwd %q tui cwd %q, want %q", env["ICEBATH_CWD"], env["ICEBATH_TUI_CWD"], tmp)
	}
	_, _, env, err = resolveChatArgv(ctx, app, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["ICEBATH_TUI_CWD"]; ok {
		t.Fatal("unset cwd still set ICEBATH_TUI_CWD")
	}
}
