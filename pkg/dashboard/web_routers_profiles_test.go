package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

const soulText = "# Persona\n\nYou are a careful, terse assistant.\n"

// Application client and a real demo profile directory under the test home
func soulClient(t *testing.T) (*testClient, string) {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	dir, err := profiles.GetProfileDir("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// Text of a file the case expects to exist
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSoulWriteDurabilityPutReplacesSoul(t *testing.T) {
	c, dir := soulClient(t)
	soul := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(soul, []byte(soulText), 0o644); err != nil {
		t.Fatal(err)
	}
	r := appJSON(t, c, "PUT", "/api/profiles/demo/soul", pyjson.ObjectOf("content", "# New\n"))
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := mustRead(t, soul); got != "# New\n" {
		t.Fatalf("SOUL.md %q", got)
	}
}

func TestSoulWriteDurabilityPutCreatesSoulWhenAbsent(t *testing.T) {
	c, dir := soulClient(t)
	soul := filepath.Join(dir, "SOUL.md")
	if pathExistsFollow(soul) {
		t.Fatal("SOUL.md exists before the first save")
	}
	r := appJSON(t, c, "PUT", "/api/profiles/demo/soul", pyjson.ObjectOf("content", soulText))
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := mustRead(t, soul); got != soulText {
		t.Fatalf("SOUL.md %q", got)
	}
}

func TestSoulWriteDurabilityExistingSoulSurvivesAnInterruptedSave(t *testing.T) {
	c, dir := soulClient(t)
	soul := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(soul, []byte(soulText), 0o644); err != nil {
		t.Fatal(err)
	}
	original := mustRead(t, soul)
	prev := pyfs.Fsync
	pyfs.Fsync = func(*os.File) error { return &os.SyscallError{Syscall: "fsync", Err: syscall.EIO} }
	r := appJSON(t, c, "PUT", "/api/profiles/demo/soul", pyjson.ObjectOf("content", "# clobbered\n"))
	pyfs.Fsync = prev
	if r.Status != 500 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := mustRead(t, soul); got != original {
		t.Fatalf("SOUL.md %q", got)
	}
	g := c.get("/api/profiles/demo/soul", sessionHeaders()...)
	if g.Status != 200 {
		t.Fatalf("status %d %s", g.Status, g.Body)
	}
	body := appObject(t, g)
	if body.GetOr("exists", nil) != true || body.GetOr("content", nil) != soulText {
		t.Fatalf("GET %s", g.Body)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temp files %v %v", leftovers, err)
	}
}

func TestSoulWriteDurabilityExistingFileModeIsPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	c, dir := soulClient(t)
	soul := filepath.Join(dir, "SOUL.md")
	if err := os.WriteFile(soul, []byte(soulText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(soul, 0o644); err != nil {
		t.Fatal(err)
	}
	r := appJSON(t, c, "PUT", "/api/profiles/demo/soul", pyjson.ObjectOf("content", "# New\n"))
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	st, err := os.Stat(soul)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %v %v", st.Mode().Perm(), err)
	}
}

func TestSoulWriteDurabilityCreatedFileModeIsNotTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	c, dir := soulClient(t)
	soul := filepath.Join(dir, "SOUL.md")
	r := appJSON(t, c, "PUT", "/api/profiles/demo/soul", pyjson.ObjectOf("content", soulText))
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	st, err := os.Stat(soul)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("first save created SOUL.md as %v %v", st.Mode().Perm(), err)
	}
}

func TestSoulReadReportsPresenceMissingSoulIsStillReportedAbsent(t *testing.T) {
	c, _ := soulClient(t)
	c.get("/api/profiles/demo/soul", sessionHeaders()...).expect(t, 200, `{"content":"","exists":false}`)
}

func TestSoulReadReportsPresenceEmptySoulIsStillReportedPresent(t *testing.T) {
	c, dir := soulClient(t)
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c.get("/api/profiles/demo/soul", sessionHeaders()...).expect(t, 200, `{"content":"","exists":true}`)
}

func TestProfilesBurstLeavesThreadpoolStatusResponsive(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	release := make(chan struct{})
	var calls atomic.Int32
	prevList, prevDict := listProfilesForRouter, profileToDict
	listProfilesForRouter = func(ctx context.Context, lazy bool) ([]manage.ProfileInfo, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-time.After(3 * time.Second):
			t.Error("release never came")
		}
		return []manage.ProfileInfo{{Name: "example"}}, nil
	}
	profileToDict = func(p manage.ProfileInfo) *pyjson.Object { return pyjson.ObjectOf("name", p.Name) }
	t.Cleanup(func() { listProfilesForRouter, profileToDict = prevList, prevDict })
	app := NewFastAPI("", "", nil, nil)
	app.IncludeRouter(profilesRouter(), "")
	app.AddAPIRoute("/api/status", []string{"GET"}, &Endpoint{Name: "status",
		Handle: func(ctx context.Context, c *Call) (any, error) { return pyjson.ObjectOf("ok", true), nil }})
	c := newTestClient(t, app)
	responses := make([]*testResponse, 12)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i] = c.get("/api/profiles")
		}()
	}
	released := false
	defer func() {
		if !released {
			close(release)
			wg.Wait()
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("list_profiles never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	statusDone := make(chan *testResponse, 1)
	go func() { statusDone <- c.get("/api/status") }()
	select {
	case r := <-statusDone:
		r.expect(t, 200, `{"ok":true}`)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("status starved behind the profiles burst")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	if n := len(profilesRead.admission); n != 1 {
		t.Fatalf("borrowed tokens %d", n)
	}
	released = true
	close(release)
	wg.Wait()
	for _, r := range responses {
		r.expect(t, 200, `{"profiles":[{"name":"example"}]}`)
	}
}

// Application client with the user home and the icebath home inside one temp dir, the profile_env fixture
func profileEnvClient(t *testing.T) *testClient {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	app := dashboardTestApp(t)
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(manage.SwapCleanupGatewayService(func(context.Context, string, string) bool { return false }))
	return newTestClient(t, app)
}

func TestSettlePendingDeleteReportsPartialSuccess(t *testing.T) {
	c := profileEnvClient(t)
	t.Cleanup(manage.SwapLiveDefaultMultiplexer(func() bool { return true }))
	if _, err := manage.CreateProfile(t.Context(), "gone", manage.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	goneDir, err := profiles.GetProfileDir("gone")
	if err != nil {
		t.Fatal(err)
	}
	resp := c.request("DELETE", "/api/profiles/gone", nil, sessionHeaders()...)
	body := appObject(t, resp)
	if body.GetOr("ok", nil) != true || body.GetOr("settlement_pending", nil) != true || body.GetOr("identity_settled", nil) != false {
		t.Fatalf("body %s", resp.Body)
	}
	if got := body.GetOr("retry_command", nil); got != "icebath profile purge-identity gone" {
		t.Fatalf("retry_command %v", got)
	}
	if got := body.GetOr("path", nil); got != goneDir {
		t.Fatalf("path %v want %s", got, goneDir)
	}
	if pathExistsFollow(goneDir) {
		t.Fatal("the profile directory survived the delete")
	}
}

func TestFilesystemRemoveFailureStillReports500(t *testing.T) {
	c := profileEnvClient(t)
	t.Cleanup(manage.SwapRmtreeForDelete(func(string, func(pyfs.RmTreeOp, string, error) error) error {
		return &os.PathError{Op: "rmtree", Path: "gone", Err: syscall.EBUSY}
	}))
	if _, err := manage.CreateProfile(t.Context(), "gone", manage.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if resp := c.request("DELETE", "/api/profiles/gone", nil, sessionHeaders()...); resp.Status != 500 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestArchivedProfilesPagesDoNotLoseRowsToNonArchivedPins(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(home.Root(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"archive-a", "archive-b"} {
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SetSessionArchived(ctx, sid, true, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateSession(state.NewSession{ID: "live-pin", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "live-pin", map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionPinned(ctx, "live-pin", true); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, route := range []string{"/api/profiles/sessions?profile=default", "/api/sessions?profile=default"} {
		archivedIDs := map[string]bool{}
		for _, offset := range []string{"0", "1"} {
			page := appObject(t, c.get(route+"&limit=1&offset="+offset+"&archived=only", sessionHeaders()...))
			if total := page.GetOr("total", nil); fmt.Sprint(total) != "2" {
				t.Fatalf("%s total %v", route, total)
			}
			rows := objectRows(page.GetOr("sessions", nil))
			if len(rows) == 0 || rows[0].GetOr("archived", nil) != true {
				t.Fatalf("%s offset %s first row is not archived", route, offset)
			}
			pinned := false
			for _, row := range rows {
				id, _ := row.GetOr("id", nil).(string)
				if row.GetOr("archived", nil) == true {
					archivedIDs[id] = true
				}
				pinned = pinned || id == "live-pin"
			}
			if !pinned {
				t.Fatalf("%s offset %s lost the live pin", route, offset)
			}
		}
		if len(archivedIDs) != 2 || !archivedIDs["archive-a"] || !archivedIDs["archive-b"] {
			t.Fatalf("%s archived rows %v", route, archivedIDs)
		}
	}
}
