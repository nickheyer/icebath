package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Store the agent browser era left, both entries, the old chromium env and the recorded opt out
type rabStore struct {
	store, binary, retired, retiredBin string
}

// Records a fact for an entry with its identity and tree digest
func rabRecord(t *testing.T, facts *Facts, store, name, version, entry string, env EnvDiff, artifacts []string) {
	t.Helper()
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := TreeDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record(name, version, filepath.Base(entry), env, store, FactRecord{Target: &target, Artifacts: artifacts, Digest: &digest}); err != nil {
		t.Fatal(err)
	}
}

// Isolated home and runtime over a checkout of the upstream pyproject, the real registry and lockfile, the upgraded store built
func rabUpgradedStore(t *testing.T) rabStore {
	t.Helper()
	tmp := isolateHome(t)
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "runtime"))
	repo := filepath.Join(tmp, "checkout")
	peWrite(t, filepath.Join(repo, "pyproject.toml"), peRead(t, filepath.Join(peReferences, "pyproject.toml")))
	peSwap(t, &repoRoot, func() string { return repo })
	ctx := context.Background()
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	chromium, err := GetPackage("chromium")
	if err != nil {
		t.Fatal(err)
	}
	if reason := chromium.MissingReason(target); reason != "" {
		t.Skipf("pm builds no Chromium for %s", target)
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock := NewLockfile(LockfilePath())
	version, ok := lock.Version("chromium")
	if !ok {
		t.Fatal("the lockfile pins no chromium")
	}
	entry := filepath.Join(store, chromium.StoreEntry(version, target))
	exe := "chrome"
	if strings.HasPrefix(target, "win32") {
		exe = "chrome.exe"
	}
	s := rabStore{store: store, binary: filepath.Join(entry, "chrome-fixture", exe)}
	writeFile(t, s.binary, []byte("inert browser fixture"), 0o755)
	if err := os.Chmod(s.binary, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(entry, "INSTALLATION_COMPLETE"), nil, 0o644)
	factsPath, err := FactsPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for _, row := range lock.Artifacts("chromium", target) {
		shas = append(shas, row["sha256"].(string))
	}
	rabRecord(t, facts, store, "chromium", version, entry, EnvDiff{"PLAYWRIGHT_BROWSERS_PATH": store, "AGENT_BROWSER_EXECUTABLE_PATH": s.binary}, shas)
	s.retired = filepath.Join(store, "agent-browser-0.26.0")
	s.retiredBin = filepath.Join(s.retired, "bin")
	writeFile(t, filepath.Join(s.retiredBin, "agent-browser-"+target), []byte("retired"), 0o644)
	rabRecord(t, facts, store, "agent-browser", "0.26.0", s.retired, EnvDiff{"PATH": []string{s.retiredBin}}, []string{strings.Repeat("0", 64)})
	writeFile(t, DeclinedPath(ctx, ""), []byte(`{"schema": 1, "declined": ["agent-browser"]}`), 0o644)
	return s
}

// Installed chromium binary as an application caller reads it
func rabChromium(t *testing.T) string {
	t.Helper()
	installed, err := Installed(context.Background(), "chromium", false)
	if err != nil || installed == nil {
		t.Fatalf("chromium installed %v %v", installed, err)
	}
	return installed.Binary
}

// Install command steps recorded the way the closure tests spy on them, names accumulating over calls
type rabSpy struct{ names []string }

// Replaces the tool install, venv sync and activation steps of the install command with recorders
func rabInstallSpy(t *testing.T) *rabSpy {
	t.Helper()
	spy := &rabSpy{}
	peSwap(t, &cliInstallNames, func(_ context.Context, names []string, _ *string, _ bool) (int, error) {
		spy.names = append(append([]string{}, spy.names...), names...)
		return 0, nil
	})
	peSwap(t, &installSyncVenv, func(context.Context, SyncVenvOptions) error { return nil })
	peSwap(t, &installActivate, func(context.Context, bool) ([]string, error) { return []string{}, nil })
	return spy
}

// Names a bare install requests after the recorded names are cleared
func (s *rabSpy) bareInstall(t *testing.T, without []string) []string {
	t.Helper()
	s.names = nil
	if code := rabInstall(t, InstallArgs{Without: without}); code != 0 {
		t.Fatalf("bare install exited %d", code)
	}
	return s.names
}

// Runs the install command and fails the test on an error escaping it
func rabInstall(t *testing.T, args InstallArgs) int {
	t.Helper()
	var out bytes.Buffer
	code, err := CmdInstall(withSyncOutput(context.Background(), &out), args)
	if err != nil {
		t.Fatalf("install %v: %s", err, out.String())
	}
	return code
}

// Declined default packages of the checkout
func rabDeclined(t *testing.T) map[string]bool {
	t.Helper()
	declined, err := Declined(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return declined
}

func TestEveryReaderSkipsTheRetiredFact(t *testing.T) {
	s := rabUpgradedStore(t)
	ctx := context.Background()
	if got := rabChromium(t); got != s.binary {
		t.Fatalf("chromium binary %q, want %q", got, s.binary)
	}
	report, err := Drift(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, flagged := report.Reasons["agent-browser"]; flagged {
		t.Fatalf("drift reports the retired fact %v", report.Reasons)
	}
	env := EnvFor(AllPackages(), map[string]string{})
	if slices.Contains(strings.Split(env["PATH"], string(os.PathListSeparator)), s.retiredBin) {
		t.Fatalf("env PATH carries the retired bin %q", env["PATH"])
	}
	if env["PLAYWRIGHT_BROWSERS_PATH"] != s.store {
		t.Fatalf("env %v", env)
	}
	t.Setenv("PATH", "")
	if _, err := Activate(ctx, true); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)), s.retiredBin) {
		t.Fatalf("activation put the retired bin on PATH %q", os.Getenv("PATH"))
	}
}

func TestEnsureRerecordsTheOldChromiumEnvWithoutInstalling(t *testing.T) {
	s := rabUpgradedStore(t)
	ctx := context.Background()
	peSwap(t, &realizePin, func(context.Context, Package, *Lockfile, *Facts, *Store, string, InstallPackageOptions) (string, error) {
		t.Fatal("an installed package was installed")
		return "", nil
	})
	if _, err := ensure(ctx, "chromium", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	env, err := envFor(ctx, []string{"chromium"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env, map[string]string{"PLAYWRIGHT_BROWSERS_PATH": s.store}) {
		t.Fatalf("env %v", env)
	}
	facts, err := storeFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := facts.EnvFor("chromium", store)["AGENT_BROWSER_EXECUTABLE_PATH"]; kept {
		t.Fatal("the retired variable stayed recorded")
	}
	if got := rabChromium(t); got != s.binary {
		t.Fatalf("chromium binary %q, want %q", got, s.binary)
	}
}

func TestGcReclaimsTheRetiredEntryAndKeepsChromium(t *testing.T) {
	s := rabUpgradedStore(t)
	ctx := context.Background()
	facts, err := storeFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, _, err := gcStore(withSyncOutput(ctx, &out), NewStore(s.store), facts); err != nil {
		t.Fatal(err)
	}
	reread, err := storeFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := reread.Get("agent-browser"); kept {
		t.Fatal("gc kept the retired fact")
	}
	if lexists(s.retired) {
		t.Fatal("gc kept the retired entry")
	}
	if _, kept := reread.Get("chromium"); !kept {
		t.Fatal("gc dropped chromium")
	}
	if got := rabChromium(t); got != s.binary {
		t.Fatalf("chromium binary %q, want %q", got, s.binary)
	}
}

func TestARecordedAgentBrowserOptOutStillSkipsTheBrowser(t *testing.T) {
	rabUpgradedStore(t)
	spy := rabInstallSpy(t)
	if got := rabDeclined(t); !reflect.DeepEqual(got, map[string]bool{"chromium": true}) {
		t.Fatalf("declined %v", got)
	}
	if names := spy.bareInstall(t, nil); slices.Contains(names, "chromium") {
		t.Fatalf("a bare install requested chromium %v", names)
	}
	spy.names = nil
	if code := rabInstall(t, InstallArgs{Names: []string{"agent-browser"}}); code != 0 {
		t.Fatalf("install agent-browser exited %d", code)
	}
	if !reflect.DeepEqual(spy.names, []string{"chromium"}) {
		t.Fatalf("names %v", spy.names)
	}
	if got := rabDeclined(t); len(got) != 0 {
		t.Fatalf("declined %v", got)
	}
	if names := spy.bareInstall(t, nil); !slices.Contains(names, "chromium") {
		t.Fatalf("a bare install after the opt back in skipped chromium %v", names)
	}
}

func TestAnOldInstallerWithoutAgentBrowserDeclinesChromium(t *testing.T) {
	rabUpgradedStore(t)
	spy := rabInstallSpy(t)
	if err := os.Remove(DeclinedPath(context.Background(), "")); err != nil {
		t.Fatal(err)
	}
	if names := spy.bareInstall(t, []string{"agent-browser"}); slices.Contains(names, "chromium") {
		t.Fatalf("an install without agent-browser requested chromium %v", names)
	}
	if got := rabDeclined(t); !reflect.DeepEqual(got, map[string]bool{"chromium": true}) {
		t.Fatalf("declined %v", got)
	}
	var record map[string]any
	if err := json.Unmarshal(readFile(t, DeclinedPath(context.Background(), "")), &record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record["declined"], []any{"chromium"}) {
		t.Fatalf("declined record %v", record)
	}
}
