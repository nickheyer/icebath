package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const sessionsHolderHelperEnv = "ICEBATH_SESSIONS_HOLDER_HELPER_DB"

// Child half of the held store tests, keeping a connection on the store until a line arrives on stdin
func TestSessionsStoreHolderHelperProcess(t *testing.T) {
	path := os.Getenv(sessionsHolderHelperEnv)
	if path == "" {
		return
	}
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		os.Exit(2)
	}
	var n int64
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		os.Exit(3)
	}
	fmt.Println("ready")
	bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// Fresh profile home the sessions command runs against
func sessionsTestHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	installSessionsStoreWiring(t)
	return h
}

// Installs the store hooks the process wiring installs for message media, spillover files and platform labels
func installSessionsStoreWiring(t *testing.T) {
	t.Helper()
	labels, source, same, agree := state.MessagingPlatformLabels, state.MessagingPlatformSource, state.SameOwner, state.OwnersAgree
	subject, media, hosted, release := state.ParseAuthenticatedSubject, state.AdmissionMediaReferences, state.HostedDocumentReferences, state.ReleaseUnheldMedia
	filter, names, referenced, namespace := state.SpilloverSQLFilter, state.SpilloverNamesInText, state.ReferencedSpilloverNames, state.SessionKeyNamespace
	state.MessagingPlatformLabels, state.MessagingPlatformSource = gateway.MessagingPlatformLabels, gateway.IsMessagingPlatformSource
	state.SameOwner, state.OwnersAgree, state.ParseAuthenticatedSubject = gateway.SameOwner, gateway.OwnersAgree, gateway.ParseAuthenticatedSubject
	state.AdmissionMediaReferences, state.HostedDocumentReferences = gateway.AdmissionMediaReferences, gateway.HostedDocumentReferences
	state.ReleaseUnheldMedia = gateway.ReleaseUnheldMediaRetainingHistory
	state.SpilloverSQLFilter, state.SpilloverNamesInText = tools.SpilloverSQLFilter, tools.SpilloverNamesInText
	state.ReferencedSpilloverNames, state.SessionKeyNamespace = tools.ReferencedSpilloverNames, gateway.SessionKeyNamespace
	t.Cleanup(func() {
		state.MessagingPlatformLabels, state.MessagingPlatformSource, state.SameOwner, state.OwnersAgree = labels, source, same, agree
		state.ParseAuthenticatedSubject, state.AdmissionMediaReferences, state.HostedDocumentReferences, state.ReleaseUnheldMedia = subject, media, hosted, release
		state.SpilloverSQLFilter, state.SpilloverNamesInText, state.ReferencedSpilloverNames, state.SessionKeyNamespace = filter, names, referenced, namespace
	})
}

// Run state writing to a buffer, prompts answered from the given stdin text
func sessionsTestRun(t *testing.T, h string, ns map[string]any, stdin string) (*sessionsRun, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	notTTY := func() bool { return false }
	restore := SetStreams(&cliui.Console{In: strings.NewReader(stdin), Out: out, StdinTTY: notTTY, StdoutTTY: notTTY}, &bytes.Buffer{})
	t.Cleanup(restore)
	help := func() error {
		fmt.Fprintln(out, "usage: icebath sessions")
		return nil
	}
	return &sessionsRun{ctx: context.Background(), h: h, ns: ns, w: out, printHelp: help}, out
}

// Runs icebath sessions over a namespace, a panic or an error failing the test with the output so far
func runSessionsCmd(t *testing.T, h string, ns map[string]any, stdin string) (int, string) {
	t.Helper()
	r, out := sessionsTestRun(t, h, ns, stdin)
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("sessions %v panicked: %v, output %s", ns["sessions_action"], p, out.String())
		}
	}()
	code, err := CmdSessions(r.ctx, h, ns, out, r.printHelp)
	if err != nil {
		t.Fatalf("sessions %v failed: %v, output %s", ns["sessions_action"], err, out.String())
	}
	return code, out.String()
}

// Opens the store of a home for writing, closed when the test ends
func sessionsTestDB(t *testing.T, h string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Creates a session with the given user messages
func sessionsTestSeed(t *testing.T, db *state.SessionDB, n state.NewSession, contents ...string) {
	t.Helper()
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
	for _, content := range contents {
		if _, err := db.AppendMessage(context.Background(), n.ID, map[string]any{"role": "user", "content": content}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

// Namespace of the error exit code cases with the given overrides
func exitCodeArgs(action string, overrides map[string]any) map[string]any {
	ns := map[string]any{"sessions_action": action, "session_id": nil, "title": nil, "yes": true, "source": nil, "path": nil,
		"from_source": nil, "dry_run": false, "older_than": nil, "newer_than": nil, "before": nil, "after": nil, "limit": 50}
	for k, v := range overrides {
		ns[k] = v
	}
	return ns
}

func TestGrowthReportsGrewByNotNegativeReclaimed(t *testing.T) {
	label := sizeDeltaLabel(-163.0)
	if !strings.Contains(label, "163.0") || strings.Contains(label, "reclaimed") || strings.Contains(label, "-") {
		t.Fatalf("label %q", label)
	}
}

func TestNoNegativeNumberEverRendered(t *testing.T) {
	for _, value := range []float64{-9999.9, -0.1, 0.0, 0.1, 9999.9} {
		if label := sizeDeltaLabel(value); strings.Contains(label, "-") {
			t.Fatalf("%v rendered %q", value, label)
		}
	}
}

func TestDeleteMissingReturns1(t *testing.T) {
	h := sessionsTestHome(t)
	sessionsTestDB(t, h).Close()
	code, out := runSessionsCmd(t, h, exitCodeArgs("delete", map[string]any{"session_id": "nope_xyz"}), "")
	if code != 1 || !strings.Contains(out, "No session 'nope_xyz'") || !strings.Contains(out, "icebath sessions list") {
		t.Fatalf("code %d output %s", code, out)
	}
}

func TestRenameMissingReturns1(t *testing.T) {
	h := sessionsTestHome(t)
	sessionsTestDB(t, h).Close()
	if code, out := runSessionsCmd(t, h, exitCodeArgs("rename", map[string]any{"session_id": "nope_xyz", "title": []string{"New"}}), ""); code != 1 {
		t.Fatalf("code %d output %s", code, out)
	}
}

func TestImportMissingFileReturns1(t *testing.T) {
	h := sessionsTestHome(t)
	code, out := runSessionsCmd(t, h, exitCodeArgs("import", map[string]any{"path": filepath.Join(h, "nope.jsonl")}), "")
	if code != 1 || !strings.Contains(strings.ToLower(out), "file not found") {
		t.Fatalf("code %d output %s", code, out)
	}
}

// Output of the list handler over a store of six one message sessions
func listSixSessions(t *testing.T, limit int) string {
	t.Helper()
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	for i := range 6 {
		sessionsTestSeed(t, db, state.NewSession{ID: fmt.Sprintf("sess_%d", i), Source: "cli"}, fmt.Sprintf("hello %d", i))
	}
	r, out := sessionsTestRun(t, h, map[string]any{"limit": limit, "source": nil, "workspace": nil}, "")
	if code, err := cmdList(r, db); err != nil || code != 0 {
		t.Fatalf("list code %d %v", code, err)
	}
	return out.String()
}

func TestFooterWhenMoreSessionsThanLimit(t *testing.T) {
	out := listSixSessions(t, 4)
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "hello") {
			fields := strings.Fields(line)
			ids = append(ids, fields[len(fields)-1])
		}
	}
	if len(ids) != 4 {
		t.Fatalf("rendered %v in %s", ids, out)
	}
	if !strings.Contains(out, "--limit 8") {
		t.Fatalf("no footer in %s", out)
	}
}

func TestNoFooterWhenListingFits(t *testing.T) {
	for _, limit := range []int{6, 20} {
		if out := listSixSessions(t, limit); strings.Contains(out, "more") {
			t.Fatalf("limit %d printed a footer: %s", limit, out)
		}
	}
}

func TestSessionsObservationalCommandsOnMissingStoreStayEmpty(t *testing.T) {
	h := sessionsTestHome(t)
	dbPath := filepath.Join(h, "state.db")
	code, out := runSessionsCmd(t, h, map[string]any{"sessions_action": "list", "source": nil, "limit": 20, "workspace": nil}, "")
	if code != 0 || !strings.Contains(out, "No sessions found.") {
		t.Fatalf("list code %d output %s", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "stats"}, "")
	if code != 0 || !strings.Contains(out, "Total sessions: 0") || !strings.Contains(out, "Total messages: 0") {
		t.Fatalf("stats code %d output %s", code, out)
	}
	code, out = runSessionsCmd(t, h, map[string]any{"sessions_action": "pinned", "source": nil, "json": false}, "")
	if code != 0 || !strings.Contains(out, "No pinned sessions") {
		t.Fatalf("pinned code %d output %s", code, out)
	}
	if _, err := os.Lstat(dbPath); err == nil {
		t.Fatal("an observational command created state.db")
	}
}

func TestSessionObservationPreservesStorage(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprintf("populated=%v", populated), func(t *testing.T) {
			h := sessionsTestHome(t)
			path := filepath.Join(h, "state.db")
			if populated {
				db := sessionsTestDB(t, h)
				sessionsTestSeed(t, db, state.NewSession{ID: "observation-fixture", Source: "cli"}, "fixture")
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.ReadFile(path)
			for _, action := range []string{"list", "stats", "pinned", "export"} {
				code, text := runSessionsCmd(t, h, map[string]any{"sessions_action": action, "source": nil, "limit": 20, "json": true,
					"session_id": nil, "redact": false, "format": "jsonl", "output": "-", "dry_run": false}, "")
				if code != 0 || strings.Contains(text, "Error:") {
					t.Fatalf("%s code %d output %s", action, code, text)
				}
				want := map[string]string{"list": "No sessions found.", "stats": "Total sessions: 0", "export": "No sessions found."}
				if populated {
					want = map[string]string{"list": "observation-fixture", "stats": "Total sessions: 1", "export": "observation-fixture"}
				}
				if w, ok := want[action]; ok && !strings.Contains(text, w) {
					t.Fatalf("%s output lacks %q: %s", action, w, text)
				}
				if action == "pinned" && strings.TrimSpace(text) != "[]" {
					t.Fatalf("pinned output %q", text)
				}
				after, afterErr := os.ReadFile(path)
				if (afterErr == nil) != (beforeErr == nil) || !bytes.Equal(after, before) {
					t.Fatalf("%s created or mutated state.db", action)
				}
			}
		})
	}
}

// Home with a one session store and a second process holding it, the returned function releasing the holder
func heldStore(t *testing.T) (h, dbPath string, pid int, release func()) {
	t.Helper()
	h = sessionsTestHome(t)
	dbPath = filepath.Join(h, "state.db")
	db := sessionsTestDB(t, h)
	sessionsTestSeed(t, db, state.NewSession{ID: "seed", Source: "cli"}, "hello")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(os.Args[0], "-test.run=^TestSessionsStoreHolderHelperProcess$")
	holder.Env = append(os.Environ(), sessionsHolderHelperEnv+"="+dbPath)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			stdin.Close()
			holder.Wait()
		}
	}
	t.Cleanup(release)
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("holder said %q %v", line, err)
	}
	return h, dbPath, holder.Process.Pid, release
}

// Namespace of a held store action with its force flag
func heldStoreArgs(action string, force bool) map[string]any {
	ns := map[string]any{"sessions_action": action, "force": force}
	switch action {
	case "prune":
		for k, v := range map[string]any{"dry_run": false, "yes": true, "never_active": false, "include_archived": false, "include_pinned": false} {
			ns[k] = v
		}
		for _, name := range filterArgs {
			ns[name] = nil
		}
	case "optimize-storage":
		ns["no_vacuum"], ns["yes"] = false, true
	}
	return ns
}

func TestStoreRewritesRefuseAndNameTheHolderUntilForced(t *testing.T) {
	for _, action := range []string{"optimize", "optimize-storage", "prune"} {
		t.Run(action, func(t *testing.T) {
			if !heldStoreActions[action] || len(heldStoreActions) != 3 {
				t.Fatalf("held store actions %v", heldStoreActions)
			}
			h, dbPath, pid, _ := heldStore(t)
			code, out := runSessionsCmd(t, h, heldStoreArgs(action, false), "")
			if code != 1 || !strings.Contains(out, fmt.Sprintf("PID %d (", pid)) ||
				!strings.Contains(out, "Refusing `icebath sessions "+action+"`") || !strings.Contains(out, "--force") {
				t.Fatalf("code %d output %s", code, out)
			}
			if !strings.Contains(out, dbPath) {
				t.Fatalf("refusal does not name %s: %s", dbPath, out)
			}
			sub, _, err := commands.NewCLI().Root.Find([]string{"sessions", action})
			if err != nil || sub.Name() != action {
				t.Fatalf("sessions %s parser %v %v", action, sub, err)
			}
			for _, flag := range []string{"db", "db-path", "database"} {
				if sub.Flags().Lookup(flag) != nil {
					t.Fatalf("sessions %s takes --%s", action, flag)
				}
			}
			code, out = runSessionsCmd(t, h, heldStoreArgs(action, true), "")
			if code == 1 || strings.Contains(out, "Refusing") {
				t.Fatalf("forced code %d output %s", code, out)
			}
		})
	}
}

func TestPrunePreviewPassesTheDeleteWaitsForAQuietStore(t *testing.T) {
	h, _, _, release := heldStore(t)
	preview := heldStoreArgs("prune", false)
	preview["dry_run"], preview["yes"] = true, false
	code, out := runSessionsCmd(t, h, preview, "")
	if code != 0 || strings.Contains(out, "Refusing") {
		t.Fatalf("preview code %d output %s", code, out)
	}
	preview["dry_run"], preview["yes"] = false, true
	preview["older_than"] = nil
	code, out = runSessionsCmd(t, h, preview, "")
	if code != 1 || !strings.Contains(out, "Refusing `icebath sessions prune`") {
		t.Fatalf("held delete code %d output %s", code, out)
	}
	release()
	code, out = runSessionsCmd(t, h, preview, "")
	if code != 0 || strings.Contains(out, "Refusing") {
		t.Fatalf("quiet delete code %d output %s", code, out)
	}
}
