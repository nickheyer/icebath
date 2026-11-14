package tuiserver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Status report hooks assigned the way cmd/icebath wiring assigns them
func useStatusReport(t *testing.T) {
	t.Helper()
	saved := tuiserver.StatusReport
	tuiserver.StatusReport.Fields, tuiserver.StatusReport.Lines = app.TUIStatusFields, app.TUIStatusLines
	t.Cleanup(func() { tuiserver.StatusReport = saved })
}

// Store at dir/state.db holding the status row under the title
func statusStore(t *testing.T, dir, title string) *state.DB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "status-row", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(context.Background(), "status-row", title); err != nil {
		t.Fatal(err)
	}
	return db
}

// Live status record, bound to the profile home when one is given
func statusRecord(t *testing.T, sid, profileHome string) {
	t.Helper()
	fields := map[string]any{"session_key": "status-row", "history": []any{}, "history_lock": &sync.Mutex{}, "running": false,
		"agent": nil, "created_at": 1.0, "last_active": 1.0}
	if profileHome != "" {
		fields["profile_home"] = profileHome
	}
	tuigateway.SetSessionRecord(sid, tuigateway.NewSession(fields))
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
}

func statusOutput(t *testing.T, sid string) string {
	t.Helper()
	resp := tuiserver.LookupMethod("session.status")(context.Background(), "1", map[string]any{"session_id": sid})
	result, isResult := resp["result"].(map[string]any)
	if !isResult {
		t.Fatalf("resp %v", resp)
	}
	return result["output"].(string)
}

// test_status_profile_home.py test_session_status_path_uses_owning_profile_home
func TestSessionStatusPathUsesOwningProfileHome(t *testing.T) {
	root := t.TempDir()
	launchHome, profileHome := filepath.Join(root, "profiles", "launch"), filepath.Join(root, "profiles", "ember")
	for _, dir := range []string{launchHome, profileHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", launchHome)
	useStatusReport(t)
	tuiserver.UseSharedDB(t, statusStore(t, launchHome, "launch"))
	statusStore(t, profileHome, "owned").Close()
	statusRecord(t, "status-profile-home", profileHome)
	output := statusOutput(t, "status-profile-home")
	if !strings.Contains(output, "Path: "+profileHome) || strings.Contains(output, "Path: "+launchHome) {
		t.Fatalf("output %q", output)
	}
}

// test_status_profile_home.py test_session_status_path_without_profile_home_keeps_launch_home
func TestSessionStatusPathWithoutProfileHomeKeepsLaunchHome(t *testing.T) {
	launchHome := filepath.Join(t.TempDir(), "launch")
	if err := os.MkdirAll(launchHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launchHome)
	useStatusReport(t)
	tuiserver.UseSharedDB(t, statusStore(t, launchHome, "launch"))
	statusRecord(t, "status-launch-home", "")
	if output := statusOutput(t, "status-launch-home"); !strings.Contains(output, "Path: "+launchHome) {
		t.Fatalf("output %q", output)
	}
}
