package manage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Captures lifecycle prints for the test duration
func captureOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &buf, &buf
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return &buf
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCreateProfileSeedsPlaceholderEnvAndSoul(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	dir, err := CreateProfile(context.Background(), "coder", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, ".env")
	for _, line := range strings.Split(readText(t, env), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			t.Fatalf("non comment line %q", line)
		}
	}
	if st, _ := os.Stat(env); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if !strings.HasPrefix(readText(t, filepath.Join(dir, "SOUL.md")), "You are icebath") {
		t.Fatal("default soul not seeded")
	}
	for _, sub := range profiles.ProfileDirs {
		if !isDir(filepath.Join(dir, sub)) {
			t.Fatalf("missing %s", sub)
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".coder.staging-*")); len(leftovers) != 0 {
		t.Fatalf("staging left %v", leftovers)
	}
}

func TestCreateProfileCloneConfigCopiesFiles(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	writeFile(t, filepath.Join(def, ".env"), "KEY=val")
	writeFile(t, filepath.Join(def, "SOUL.md"), "Be helpful.")
	writeFile(t, filepath.Join(def, "plugins", "example-plugin", "plugin.yaml"), "name: example-plugin\n")
	dir, err := CreateProfile(context.Background(), "coder", CreateOptions{CloneConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(readText(t, filepath.Join(dir, ".env"))) != "KEY=val" || readText(t, filepath.Join(dir, "SOUL.md")) != "Be helpful." {
		t.Fatal("clone files differ")
	}
	if !strings.Contains(readText(t, filepath.Join(dir, "config.yaml")), "model: test") {
		t.Fatal("config not cloned")
	}
	if readText(t, filepath.Join(dir, "plugins", "example-plugin", "plugin.yaml")) != "name: example-plugin\n" {
		t.Fatal("plugins not cloned")
	}
}

func TestCreateProfileRefusesConflictingOptionsAndExisting(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	from := "default"
	var ve *profiles.ValueError
	for _, o := range []CreateOptions{{NoSkills: true, CloneConfig: true}, {NoSkills: true, CloneFrom: &from}, {SyncImports: true}, {CloneChannels: true}} {
		if _, err := CreateProfile(ctx, "coder", o); !errors.As(err, &ve) {
			t.Fatalf("%+v accepted: %v", o, err)
		}
	}
	if _, err := CreateProfile(ctx, "default", CreateOptions{}); !errors.As(err, &ve) {
		t.Fatalf("default accepted: %v", err)
	}
	if _, err := CreateProfile(ctx, "coder", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProfile(ctx, "coder", CreateOptions{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate: %v", err)
	}
	shell := filepath.Join(tmp, ".icebath", "profiles", "shell")
	writeFile(t, filepath.Join(shell, "skills", "mine.md"), "x")
	if _, err := CreateProfile(ctx, "shell", CreateOptions{}); err == nil || !strings.Contains(err.Error(), "carries no profile identity") {
		t.Fatalf("identity-less dir: %v", err)
	}
	if readText(t, filepath.Join(shell, "skills", "mine.md")) != "x" {
		t.Fatal("user file removed")
	}
}

func TestCreateProfileNoSkillsWritesMarker(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	dir, err := CreateProfile(context.Background(), "bare", CreateOptions{NoSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readText(t, filepath.Join(dir, profiles.NoBundledSkillsMarker)), "opted out of bundled-skill seeding") {
		t.Fatal("marker missing")
	}
}

func TestCloneAllExcludesRuntimeTreesAndStripsRuntimeFiles(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: x\n")
	writeFile(t, filepath.Join(def, "gateway.pid"), "123")
	writeFile(t, filepath.Join(def, "memories", "MEMORY.md"), "m")
	writeFile(t, filepath.Join(def, "sessions", "s.json"), "{}")
	writeFile(t, filepath.Join(def, "profiles", "other", "config.yaml"), "{}")
	dir, err := CreateProfile(context.Background(), "full", CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(dir, "gateway.pid")) || pathExists(filepath.Join(dir, "profiles")) {
		t.Fatal("runtime state cloned")
	}
	if readText(t, filepath.Join(dir, "memories", "MEMORY.md")) != "m" || !isDir(filepath.Join(dir, "sessions")) {
		t.Fatal("state not cloned or history dir missing")
	}
}

func TestBackfillProfileEnvsSeedsOnlyMissing(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	a := namedProfile(t, tmp, "alpha")
	b := namedProfile(t, tmp, "beta")
	writeFile(t, filepath.Join(b, ".env"), "TOKEN=keep")
	got, err := BackfillProfileEnvs(true)
	if err != nil || !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("%v %v", got, err)
	}
	if readText(t, filepath.Join(a, ".env")) != profiles.PlaceholderEnv || readText(t, filepath.Join(b, ".env")) != "TOKEN=keep" {
		t.Fatal("env contents wrong")
	}
}

func TestDeleteProfileRemovesDirAndResetsActive(t *testing.T) {
	tmp := manageEnv(t)
	out := captureOut(t)
	ctx := context.Background()
	dir, err := CreateProfile(ctx, "coder", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.SetActiveProfile("coder"); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteProfile(ctx, "coder", true); err != nil {
		t.Fatal(err)
	}
	if pathExists(dir) || profiles.GetActiveProfile("") != "default" {
		t.Fatal("dir survives or active not reset")
	}
	if !strings.Contains(out.String(), "Profile 'coder' deleted.") {
		t.Fatalf("output %q", out.String())
	}
	_ = tmp
	var ve *profiles.ValueError
	if _, err := DeleteProfile(ctx, "default", true); !errors.As(err, &ve) {
		t.Fatalf("default delete: %v", err)
	}
}

func TestDeleteProfileCancelsWithoutMatchingConfirmation(t *testing.T) {
	manageEnv(t)
	out := captureOut(t)
	ctx := context.Background()
	dir, _ := CreateProfile(ctx, "coder", CreateOptions{})
	old := stdin
	stdin = strings.NewReader("nope\n")
	t.Cleanup(func() { stdin = old })
	if _, err := DeleteProfile(ctx, "coder", false); err != nil {
		t.Fatal(err)
	}
	if !isDir(dir) || !strings.Contains(out.String(), "Cancelled.") {
		t.Fatal("unconfirmed delete went ahead")
	}
}

func TestRenameProfileMovesDirRecordsHistoryAndRetargets(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	old, _ := CreateProfile(ctx, "coder", CreateOptions{})
	profiles.SetActiveProfile("coder")
	newDir, err := RenameProfile(ctx, "coder", "writer")
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(old) || !isDir(newDir) || profiles.GetActiveProfile("") != "writer" {
		t.Fatal("rename incomplete")
	}
	if got := profiles.ReadProfileMeta(newDir).PreviousNames; !reflect.DeepEqual(got, []string{"coder"}) {
		t.Fatalf("history %v", got)
	}
	if _, err := RenameProfile(ctx, "writer", "default"); err == nil {
		t.Fatal("rename to default accepted")
	}
	if _, err := RenameProfile(ctx, "ghost", "x"); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestExportImportRoundTripDropsCredentials(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	dir, _ := CreateProfile(ctx, "coder", CreateOptions{})
	writeFile(t, filepath.Join(dir, "auth.json"), "{}")
	writeFile(t, filepath.Join(dir, "skills", "x", ".ssh", "id_rsa"), "key")
	writeFile(t, filepath.Join(dir, "notes.md"), "plain notes\n")
	writeFile(t, filepath.Join(dir, "installs", "pm.json"), "{}")
	archive, err := ExportProfile("coder", filepath.Join(tmp, "out.tar.gz"), []ExportExtraFile{{Rel: "extra/x.txt", Content: "hi"}})
	if err != nil || archive != filepath.Join(tmp, "out.tar.gz") {
		t.Fatalf("%s %v", archive, err)
	}
	imported, err := ImportProfile(archive, "copy")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"auth.json", ".env", "installs", filepath.Join("skills", "x", ".ssh")} {
		if pathExists(filepath.Join(imported, gone)) {
			t.Fatalf("%s exported", gone)
		}
	}
	if readText(t, filepath.Join(imported, "notes.md")) != "plain notes\n" || readText(t, filepath.Join(imported, "extra", "x.txt")) != "hi" {
		t.Fatal("content lost")
	}
	if _, err := ImportProfile(archive, "copy"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate import: %v", err)
	}
	if _, err := ImportProfile(archive, "default"); err == nil {
		t.Fatal("import as default accepted")
	}
}

func TestBackendArgvMatching(t *testing.T) {
	cases := map[string]bool{
		"/usr/bin/icebath gateway run": true, "python3 /x/icebath serve": true, "python3.12 icebath-notes.py": false,
		"node server.js": false, "/opt/x/icebath-gateway": true, "python /x/tui_gateway/main.py": true,
	}
	for line, want := range cases {
		if got := isIcebathArgv(strings.Fields(line)); got != want {
			t.Fatalf("%q: %v", line, got)
		}
	}
	got := argvProfileSelectors([]string{"icebath", "-p", "a", "--profile", "b", "--profile=c", "-p"})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("%v", got)
	}
}

func TestControlAnswerFailureText(t *testing.T) {
	obj := pyjson.NewObject()
	obj.Set("ok", false)
	obj.Set("error", "boom")
	empty := pyjson.NewObject()
	empty.Set("ok", false)
	for answer, want := range map[any]string{obj: "boom", empty: "{'ok': False}", "odd": "'odd'"} {
		if got := controlAnswerFailure(answer); got != want {
			t.Fatalf("%v: %q", answer, got)
		}
	}
	if controlAnswerFailure(nil) != "no response from gateway control socket" {
		t.Fatal("nil answer text")
	}
}

func TestHonchoHostBlockMigrates(t *testing.T) {
	tmp := manageEnv(t)
	out := captureOut(t)
	dir := namedProfile(t, tmp, "writer")
	writeFile(t, filepath.Join(dir, "honcho.json"), `{"hosts": {"icebath_coder": {"workspace": "w"}, "other": {}}}`)
	if err := migrateHonchoProfileHost("coder", "writer", dir); err != nil {
		t.Fatal(err)
	}
	got := readText(t, filepath.Join(dir, "honcho.json"))
	want := "{\n  \"hosts\": {\n    \"other\": {},\n    \"icebath_writer\": {\n      \"workspace\": \"w\",\n      \"aiPeer\": \"coder\"\n    }\n  }\n}"
	if got != want || !strings.Contains(out.String(), "Honcho host updated: icebath_coder → icebath_writer") {
		t.Fatalf("%s\n%s", got, out.String())
	}
}

func TestProfileExportPathUsesManagedDirectory(t *testing.T) {
	tmp := manageEnv(t)
	p, err := GetProfileExportPath("coder", "20260101-000000")
	if err != nil || p != filepath.Join(tmp, ".icebath", "profile-exports", "coder-20260101-000000.tar.gz") {
		t.Fatalf("%s %v", p, err)
	}
	os.MkdirAll(filepath.Join(tmp, ".git"), 0o755)
	alt := t.TempDir()
	oldTemp := tempDir
	tempDir = func() string { return alt }
	t.Cleanup(func() { tempDir = oldTemp })
	p, err = GetProfileExportPath("coder", "s")
	if err != nil || filepath.Dir(p) != filepath.Join(alt, "icebath-profile-exports-"+strconv.Itoa(os.Getuid())) {
		t.Fatalf("checkout fallback %s %v", p, err)
	}
}
