package pm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home plus an empty checkout, returning the checkout
func svRepo(t *testing.T) string {
	t.Helper()
	tmp := isolateHome(t)
	return sttMkdir(t, filepath.Join(tmp, "repo"))
}

// Generation venv with pyvenv.cfg and site packages under the install state, returning the venv
func svGeneration(t *testing.T, ctx context.Context, repo, name string) string {
	t.Helper()
	venv := filepath.Join(InstallStateDir(ctx, repo), "environments", name, "venv")
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("version = 3.12.4\n"), 0o666)
	site, err := SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	sttMkdir(t, site)
	sttTouch(t, filepath.Join(filepath.Dir(venv), ".lease-managed"))
	return venv
}

// Writes facts selecting a venv for the checkout
func svSelect(t *testing.T, ctx context.Context, repo string, environment any) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": environment}}})
	writeFile(t, EnvironmentRuntimeFactsPath(ctx, repo), data, 0o666)
}

// Restores this process's import path and dependency env vars when the test ends
func svKeepProcess(t *testing.T) {
	t.Helper()
	saved := importPathEntries()
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("PYTHONPATH", os.Getenv("PYTHONPATH"))
	t.Cleanup(func() {
		importPath.Lock()
		importPath.entries = saved
		importPath.Unlock()
		NoteRestartPending("")
	})
}

func svWantError(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %v, want %q", err, fragment)
	}
}

func TestSVSelectedVenvReadsTheRecordedGeneration(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	if got, err := SelectedVenv(ctx, repo); err != nil || got != filepath.Join(resolvePath(repo), "venv") {
		t.Fatalf("fresh selection %q %v", got, err)
	}
	sttMkdir(t, filepath.Join(repo, ".venv"))
	if got, _ := SelectedVenv(ctx, repo); got != filepath.Join(resolvePath(repo), ".venv") {
		t.Fatalf("in tree selection %q", got)
	}
	if got, err := CommittedVenv(ctx, repo); err != nil || got != "" {
		t.Fatalf("nothing committed answered %q %v", got, err)
	}
	venv := svGeneration(t, ctx, repo, "a")
	svSelect(t, ctx, repo, venv)
	if got, _ := SelectedVenv(ctx, repo); got != resolvePath(venv) {
		t.Fatalf("recorded selection %q", got)
	}
	svSelect(t, ctx, repo, filepath.Join(repo, "elsewhere"))
	_, err := SelectedVenv(ctx, repo)
	svWantError(t, err, "missing or outside this install")
	svSelect(t, ctx, repo, 7)
	_, err = SelectedVenv(ctx, repo)
	svWantError(t, err, "invalid dependency environment path")
	writeFile(t, EnvironmentRuntimeFactsPath(ctx, repo), []byte(`{"packages": []}`), 0o666)
	_, err = SelectedVenv(ctx, repo)
	svWantError(t, err, "invalid dependency environment record")
	writeFile(t, EnvironmentRuntimeFactsPath(ctx, repo), []byte(`{`), 0o666)
	_, err = SelectedVenv(ctx, repo)
	svWantError(t, err, "cannot read dependency environment")
	var py *PyError
	if !errors.As(err, &py) || py.Class != "RuntimeError" {
		t.Fatalf("class of %v", err)
	}
}

func TestSVVenvPythonVersionReadsTheTree(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "a")
	writeFile(t, filepath.Join(cfg, "pyvenv.cfg"), []byte("home = /x\nversion_info = 3.13.1.final.0\n"), 0o666)
	if major, minor, ok, err := VenvPythonVersion(cfg); err != nil || !ok || major != "3" || minor != "13" {
		t.Fatalf("pyvenv version %s %s %v %v", major, minor, ok, err)
	}
	lib := filepath.Join(tmp, "b")
	sttMkdir(t, filepath.Join(lib, "lib", "python3.11"))
	if major, minor, ok, _ := VenvPythonVersion(lib); !ok || major+"."+minor != "3.11" {
		t.Fatalf("lib version %s.%s", major, minor)
	}
	odd := filepath.Join(tmp, "c")
	writeFile(t, filepath.Join(odd, "pyvenv.cfg"), []byte("version = ³.12\n"), 0o666)
	if _, _, _, err := VenvPythonVersion(odd); err == nil || errorClass(err) != "ValueError" {
		t.Fatalf("superscript digit gave %v", err)
	}
	if _, _, ok, err := VenvPythonVersion(filepath.Join(tmp, "missing")); ok || err != nil {
		t.Fatal("a missing venv has no version")
	}
}

func TestSVUnsealedTreesRunTheVenvInterpreter(t *testing.T) {
	tmp := isolateHome(t)
	venv := filepath.Join(tmp, "venv")
	got, err := VenvCommand(context.Background(), sttMkdir(t, filepath.Join(tmp, "checkout")), venv, []string{"-I", "-u"})
	if err != nil || strings.Join(got, " ") != VenvPython(venv)+" -I -u" {
		t.Fatalf("argv %v %v", got, err)
	}
}

func TestSVSealedPayloadRunsCodeWithoutTheRedirector(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("the sealed payload case runs a real interpreter: python3 is not on PATH")
	}
	tmp := isolateHome(t)
	ctx := context.Background()
	venv := filepath.Join(tmp, "venv")
	out, _ := exec.Command(python, "-c", "import sys; print('%d.%d' % sys.version_info[:2])").Output()
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("home = /nonexistent\nversion = "+strings.TrimSpace(string(out))+".0\n"), 0o666)
	writeFile(t, VenvPython(venv), []byte("#!/bin/sh\nexit 99\n"), 0o755)
	site, _ := SitePackages(venv)
	writeFile(t, filepath.Join(site, "venv_only_dep.py"), []byte("VALUE = 'from the venv'\n"), 0o666)
	payload := filepath.Join(tmp, "payload")
	repo := sttMkdir(t, filepath.Join(payload, "icebath-agent"))
	writeFile(t, filepath.Join(payload, "tools", "python", "python"), []byte("#!/bin/sh\nexec "+python+" \"$@\"\n"), 0o755)
	manifest, _ := json.Marshal(map[string]any{"schema": 1, "target": "test", "repo": "icebath-agent", "venv": "venv", "store": "tools",
		"runtime": map[string]any{"storePython": "tools/python/python", "commands": map[string]any{}}})
	writeFile(t, filepath.Join(payload, "manifest.json"), manifest, 0o666)
	prefix, err := VenvCommand(ctx, repo, venv, []string{"-I", "-u"})
	if err != nil {
		t.Fatal(err)
	}
	if prefix[0] != resolvePath(filepath.Join(payload, "tools", "python", "python")) || !containsString(prefix, "-S") || containsString(prefix, VenvPython(venv)) {
		t.Fatalf("prefix %v", prefix)
	}
	cmd := exec.Command(prefix[0], append(prefix[1:], "-c", "import venv_only_dep; print(venv_only_dep.VALUE)")...)
	cmd.Env = append(os.Environ(), "PYTHONPATH=", "PYTHONHOME=")
	got, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(got)) != "from the venv" {
		t.Fatalf("payload run %q %v", got, err)
	}
}

func TestSVFlushRunsBeforeASelectionIsWritten(t *testing.T) {
	calls := 0
	saved := filesystemSync
	filesystemSync = func() error { calls++; return nil }
	t.Cleanup(func() { filesystemSync = saved })
	if err := flushBeforeSelecting(); err != nil || calls != 1 {
		t.Fatalf("flush calls %d %v", calls, err)
	}
}

// Writes a config publication journal for the checkout and returns its path
func svJournal(t *testing.T, ctx context.Context, repo string, row map[string]any) string {
	t.Helper()
	journal := filepath.Join(InstallStateDir(ctx, repo), "publication.json")
	data, _ := json.Marshal(row)
	writeFile(t, journal, data, 0o666)
	return journal
}

func TestSVInvalidJournalCannotWriteOutsideHome(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(repo), "outside", "config.yaml")
	writeFile(t, outside, []byte("untouched"), 0o666)
	journal := svJournal(t, ctx, repo, map[string]any{"config": outside, "previous": "eA==", "facts_before": nil})
	err := recoverPublication(ctx, repo)
	svWantError(t, err, "outside icebath state")
	if data, _ := os.ReadFile(outside); string(data) != "untouched" || !sttExists(journal) {
		t.Fatal("recovery touched state it must preserve")
	}
}

func TestSVRecoveryRefusesToReplaceANewerConfig(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	config := filepath.Join(home.Root(), "config.yaml")
	original := []byte("plugins:\n  enabled: [old]\n")
	published := []byte("plugins:\n  enabled: [new]\n")
	writeFile(t, config, append(published, []byte("model: user-choice\n")...), 0o666)
	row := map[string]any{"config": config, "previous": base64.StdEncoding.EncodeToString(original), "config_after": hexSHA256(published), "facts_before": nil}
	journal := svJournal(t, ctx, repo, row)
	svWantError(t, recoverPublication(ctx, repo), "config changed")
	if !sttExists(journal) {
		t.Fatal("journal dropped")
	}
	writeFile(t, config, published, 0o666)
	if err := recoverPublication(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(config); !bytes.Equal(data, original) || sttExists(journal) {
		t.Fatalf("rollback left %q", data)
	}
	writeFile(t, config, published, 0o666)
	svJournal(t, ctx, repo, row)
	if err := finishPublication(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(config); !bytes.Equal(data, published) || sttExists(journal) {
		t.Fatal("a committed publication must keep its config")
	}
}

func TestSVContextHomeRecoversFromItsOwnState(t *testing.T) {
	tmp := isolateHome(t)
	project := sttMkdir(t, filepath.Join(tmp, "repo"))
	contextHome := sttMkdir(t, filepath.Join(tmp, "context-home"))
	processState := InstallStateDir(context.Background(), project)
	ctx := home.WithOverride(context.Background(), contextHome)
	if InstallStateDir(ctx, project) == processState {
		t.Fatal("a context home must own its dependency state")
	}
	config := filepath.Join(contextHome, "config.yaml")
	writeFile(t, config, []byte("changed"), 0o666)
	svJournal(t, ctx, project, map[string]any{"config": config, "previous": base64.StdEncoding.EncodeToString([]byte("original")), "config_after": hexSHA256([]byte("changed")), "facts_before": nil})
	if err := recoverPublication(ctx, project); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(config); string(data) != "original" || sttExists(processState) {
		t.Fatalf("context recovery left %q", data)
	}
	profile := sttMkdir(t, filepath.Join(home.Root(), "profiles", "worker"))
	if InstallStateDir(home.WithOverride(context.Background(), profile), project) != processState {
		t.Fatal("a named profile shares its root's install state")
	}
}

func TestSVPluginJournalRestoresTheBackup(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	plugins := filepath.Join(home.Root(), "plugins")
	target, backup := filepath.Join(plugins, "demo"), filepath.Join(plugins, ".previous-demo")
	writeFile(t, filepath.Join(target, "new.py"), []byte("new"), 0o666)
	writeFile(t, filepath.Join(backup, "old.py"), []byte("old"), 0o666)
	metadata := filepath.Join(plugins, ".install-metadata.json")
	writeFile(t, metadata, []byte("after"), 0o666)
	journal := svJournal(t, ctx, repo, map[string]any{"kind": "plugin", "target": target, "backup": backup, "metadata": metadata,
		"facts_before": nil, "metadata_before": base64.StdEncoding.EncodeToString([]byte("before")),
		"metadata_after": base64.StdEncoding.EncodeToString([]byte("after")), "target_existed": true})
	if err := recoverPublication(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if !sttExists(filepath.Join(target, "old.py")) || sttExists(backup) || sttExists(journal) {
		t.Fatal("plugin rollback incomplete")
	}
	if data, _ := os.ReadFile(metadata); string(data) != "before" {
		t.Fatalf("metadata %q", data)
	}
}

func TestSVCollectsOnlyUnselectedUnleasedOldGenerations(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	var venvs []string
	for _, name := range []string{"selected", "stale", "young", "leased"} {
		venv := svGeneration(t, ctx, repo, name)
		if name != "young" {
			os.Chtimes(filepath.Join(filepath.Dir(venv), ".lease-managed"), old, old)
		}
		venvs = append(venvs, venv)
	}
	svSelect(t, ctx, repo, venvs[0])
	release, err := LeaseGeneration(venvs[3])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := CollectSupersededGenerations(ctx, repo); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, true, true} {
		if sttExists(filepath.Dir(venvs[i])) != want {
			t.Fatalf("generation %d kept=%v", i, !want)
		}
	}
}

func TestSVActivationAdoptAndRestart(t *testing.T) {
	repo := svRepo(t)
	svKeepProcess(t)
	ctx := context.Background()
	first := svGeneration(t, ctx, repo, "first")
	svSelect(t, ctx, repo, first)
	if reason, _ := RestartNeeded(ctx, repo); reason != "" {
		t.Fatalf("a process that never booted from the install needs no restart: %q", reason)
	}
	if err := ActivateDependencies(ctx, repo); err != nil {
		t.Fatal(err)
	}
	running, _, err := runningFromSelectedEnvironment(ctx, repo)
	if err != nil || !running {
		t.Fatal("activation must put the selection on the import path")
	}
	site, _ := SitePackages(resolvePath(first))
	if !strings.Contains(os.Getenv("PYTHONPATH"), site) {
		t.Fatalf("PYTHONPATH %q", os.Getenv("PYTHONPATH"))
	}
	second := svGeneration(t, ctx, repo, "second")
	svSelect(t, ctx, repo, second)
	reason, err := RestartNeeded(ctx, repo)
	if err != nil || !strings.Contains(reason, "second") {
		t.Fatalf("reason %q %v", reason, err)
	}
	if ok, err := AdoptSelected(ctx, repo); err != nil || !ok {
		t.Fatalf("adopt %v %v", ok, err)
	}
	if reason, _ := RestartNeeded(ctx, repo); reason != "" {
		t.Fatalf("adopted process still owes %q", reason)
	}
	secondSite, _ := SitePackages(resolvePath(second))
	tool := filepath.Join(secondSite, "demo.py")
	writeFile(t, tool, []byte("x"), 0o666)
	writeFile(t, filepath.Join(secondSite, "demo-1.0.dist-info", "METADATA"), []byte("Name: demo\nVersion: 1.0\n"), 0o666)
	writeFile(t, filepath.Join(secondSite, "demo-1.0.dist-info", "RECORD"), []byte("demo.py,,\n"), 0o666)
	third := svGeneration(t, ctx, repo, "third")
	thirdSite, _ := SitePackages(resolvePath(third))
	writeFile(t, filepath.Join(thirdSite, "demo-2.0.dist-info", "METADATA"), []byte("Name: demo\nVersion: 2.0\n"), 0o666)
	svSelect(t, ctx, repo, third)
	releaseHold := HoldGenerationFiles([]string{tool})
	if ok, _ := AdoptSelected(ctx, repo); ok {
		t.Fatal("a live child of a changed distribution forbids adoption")
	}
	releaseHold()
	if ok, _ := AdoptSelected(ctx, repo); !ok {
		t.Fatal("released holds leave the generation adoptable")
	}
}

func TestSVActivationEnvironmentCarriesTheSentinel(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	t.Setenv("VIRTUAL_ENV", "/somewhere")
	venv := svGeneration(t, ctx, repo, "a")
	svSelect(t, ctx, repo, venv)
	env, err := ActivationEnvironment(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		values[k] = v
	}
	site, _ := SitePackages(resolvePath(venv))
	if _, has := values["VIRTUAL_ENV"]; has || values["PYTHONPATH"] != resolvePath(repo)+string(os.PathListSeparator)+site ||
		values["__ICEBATH_ACTIVATED"] != EnvironmentRuntimeFactsPath(ctx, repo) {
		t.Fatalf("activation env %v", values)
	}
}

func TestSVShellExportsQuoteForEachDialect(t *testing.T) {
	env := []string{"PLAIN=a b", "QUOTE=it's", "PWD=/x", "ProgramFiles(ARM)=c", "SLASH=back\\slash"}
	sh, _ := ShellExports(env, "sh")
	if sh != "export PLAIN='a b'\nexport QUOTE='it'\"'\"'s'\nexport PWD=/x\nexport SLASH='back\\slash'" {
		t.Fatalf("sh %q", sh)
	}
	fish, _ := ShellExports(env, "fish")
	if fish != "set -gx PLAIN 'a b'\nset -gx QUOTE 'it\\'s'\nset -gx SLASH 'back\\\\slash'" {
		t.Fatalf("fish %q", fish)
	}
	if _, err := ShellExports(env, "csh"); errorClass(err) != "KeyError" {
		t.Fatal("unknown dialect must be a KeyError")
	}
	if bash, err := exec.LookPath("bash"); err == nil && runtime.GOOS != "windows" {
		out, err := exec.Command(bash, "-c", sh+"\nprintf '%s|%s|%s' \"$PLAIN\" \"$QUOTE\" \"$SLASH\"").Output()
		if err != nil || string(out) != "a b|it's|back\\slash" {
			t.Fatalf("bash evaluated %q %v", out, err)
		}
	}
}

func TestSVEnvironmentsMainParsesLikeArgparse(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := EnvironmentsMain(context.Background(), []string{"--format", "csh"}, "env", &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "argument --format: invalid choice: 'csh' (choose from json, sh, fish)") {
		t.Fatalf("code %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := EnvironmentsMain(context.Background(), []string{"stray"}, "env", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unrecognized arguments: stray") {
		t.Fatalf("stray %q", errOut.String())
	}
	if code := EnvironmentsMain(context.Background(), []string{"-h"}, "env", &out, &errOut); code != 0 || !strings.Contains(out.String(), "Print the composed activation environment.") {
		t.Fatalf("help %q", out.String())
	}
}

func TestSVDeclinedExtrasRecordAndReasons(t *testing.T) {
	repo := svRepo(t)
	ctx := context.Background()
	got, err := recordDeclinedExtras(ctx, []string{"Foo_Bar", "fal"}, nil, map[string]string{"fal": "  it  no\nlonger builds "}, repo)
	if err != nil || !got["foo-bar"] || !got["fal"] {
		t.Fatalf("record %v %v", got, err)
	}
	if reasons := declinedReasons(ctx, repo); reasons["fal"] != "it no longer builds" || len(reasons) != 1 {
		t.Fatalf("reasons %v", reasons)
	}
	path := DeclinedExtrasPath(ctx, repo)
	before, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	recordDeclinedExtras(ctx, []string{"foo-bar"}, nil, nil, repo)
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged record must not be rewritten")
	}
	got, _ = recordDeclinedExtras(ctx, nil, []string{"fal"}, nil, repo)
	if got["fal"] || len(declinedReasons(ctx, repo)) != 0 {
		t.Fatal("removing an extra drops its reason")
	}
	writeFile(t, path, []byte("{"), 0o666)
	var install *InstallError
	if _, err := declinedExtras(ctx, repo); !errors.As(err, &install) {
		t.Fatalf("damaged record gave %v", err)
	}
}

func TestSVFeaturesFileAndDeclarations(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	if got, err := readFeatures(ctx); got != nil || err != nil {
		t.Fatal("no file reads as None")
	}
	if _, err := WriteFeatures(ctx, []string{"b", "a", "b"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := readFeatures(ctx); strings.Join(got, ",") != "a,b" {
		t.Fatalf("features %v", got)
	}
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "pyproject.toml"), []byte("[project]\nname = \"icebath-agent\"\n[project.optional-dependencies]\nall = [\"icebath-agent[web]\"]\nweb = []\nfal = []\n[tool.icebath]\nopt-in-extras = [\"fal\"]\n"), 0o666)
	if got, _ := declaredExtras(repo); strings.Join(got, ",") != "all,fal,web" {
		t.Fatalf("declared %v", got)
	}
	if got, err := optInExtras(repo); err != nil || strings.Join(got, ",") != "fal" {
		t.Fatalf("opt in %v %v", got, err)
	}
	if got, _ := DroppableExtras(repo); strings.Join(got, ",") != "fal" {
		t.Fatalf("droppable %v", got)
	}
	writeFile(t, filepath.Join(repo, "pyproject.toml"), []byte("[project.optional-dependencies]\nweb = []\n[tool.icebath]\nopt-in-extras = [\"ghost\"]\n"), 0o666)
	if _, err := optInExtras(repo); errorClass(err) != "ValueError" || !strings.Contains(err.Error(), "['ghost']") {
		t.Fatalf("undeclared opt in gave %v", err)
	}
}

func TestSVLegacySelectionCarriesWhatTheOldVenvHeld(t *testing.T) {
	repo := svRepo(t)
	saved := platformGatesCache.gates
	platformGatesCache.gates = map[string]string{"matrix": "sys_platform == 'nowhere'"}
	savedGate := evaluateGate
	evaluateGate = func(context.Context, string, map[string]string) bool { return false }
	t.Cleanup(func() { platformGatesCache.gates, evaluateGate = saved, savedGate })
	site := filepath.Join(repo, "venv", "lib", "python3.11", "site-packages")
	sttMkdir(t, filepath.Join(site, "telegram"))
	writeFile(t, filepath.Join(site, "fal_client.py"), nil, 0o666)
	for _, anchor := range Anchors["matrix"] {
		sttMkdir(t, filepath.Join(site, anchor))
	}
	got, err := LegacySelection(context.Background(), repo)
	if err != nil || strings.Join(got, ",") != "all,fal,telegram" {
		t.Fatalf("legacy %v %v", got, err)
	}
	recordDeclinedExtras(context.Background(), []string{"fal"}, nil, nil, repo)
	if got, _ := LegacySelection(context.Background(), repo); strings.Join(got, ",") != "all,telegram" {
		t.Fatalf("declined extra carried %v", got)
	}
}

func TestSVCarriedTransientRefusalAndLeftOutRecord(t *testing.T) {
	repo := svRepo(t)
	failing := map[string]CarriedFailure{"fal": {Reason: "download reset", Verdict: false}, "web": {Reason: "no wheel", Verdict: true}}
	err := refuseTransient(failing, errors.New("cause"))
	svWantError(t, err, "the extras ['fal'] icebath carried over from the old venv could not be prepared: download reset")
	svWantError(t, err, "`icebath pm install --without fal`")
	if refuseTransient(map[string]CarriedFailure{"web": failing["web"]}, nil) != nil {
		t.Fatal("verdicts alone are not transient")
	}
	if !isBuildVerdict(&BuildFailure{InstallError: NewInstallError("venv", "x", "")}) || isBuildVerdict(errors.New("x")) {
		t.Fatal("build verdict classes")
	}
	venv := newVenv(repo)
	if err := recordLeftOut(context.Background(), venv, map[string]CarriedFailure{"web": failing["web"]}); err != nil {
		t.Fatal(err)
	}
	if reasons := declinedReasons(context.Background(), repo); reasons["web"] != noBuildReason {
		t.Fatalf("left out reason %v", reasons)
	}
}

func TestSVRetiredEntryPoints(t *testing.T) {
	var out bytes.Buffer
	if ScanVenvBlockersMain(nil, &out) != 0 || !strings.Contains(out.String(), `"retired": true`) {
		t.Fatalf("scan %q", out.String())
	}
	out.Reset()
	if ScanVenvBlockersMain([]string{"--terminate-safe"}, &out) != 1 || !strings.Contains(out.String(), `"ok": false`) {
		t.Fatalf("terminate %q", out.String())
	}
	if isPausableGateway("icebath gateway run") {
		t.Fatal("no matcher fails closed")
	}
	saved := signingPlatform
	signingPlatform = func() string { return "linux" }
	defer func() { signingPlatform = saved }()
	if SignManagedPython("/x/python") {
		t.Fatal("signing happens only on macOS")
	}
}
