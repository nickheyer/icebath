package manage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestActiveProfileSetToDefaultRemovesFile(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	if _, err := CreateProfile(context.Background(), "coder", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := profiles.SetActiveProfile("coder"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(tmp, ".icebath", "active_profile")
	if !pathExists(active) {
		t.Fatal("active_profile not written")
	}
	if err := profiles.SetActiveProfile("default"); err != nil {
		t.Fatal(err)
	}
	if pathExists(active) {
		t.Fatal("active_profile kept")
	}
}

func TestGetActiveProfileNameProfilePathReturnsProfileName(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	if _, err := CreateProfile(context.Background(), "coder", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, filepath.Join(tmp, ".icebath", "profiles", "coder"))
	if got := profiles.GetActiveProfileName(context.Background()); got != "coder" {
		t.Fatalf("%q", got)
	}
}

// Every line of a seeded env file is a comment or blank
func commentOnlyEnv(t *testing.T, path string) {
	t.Helper()
	for _, line := range pystr.Splitlines(strings.TrimPrefix(readText(t, path), "\ufeff"), false) {
		if !strings.HasPrefix(line, "#") && pystr.Strip(line) != "" {
			t.Fatalf("%s carries %q", path, line)
		}
	}
}

func TestBackfillProfileEnvsSeedsPlaceholderNeverTheDefaultEnv(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	writeFile(t, filepath.Join(tmp, ".icebath", ".env"), "OPENROUTER_API_KEY=root-key\n")
	p1 := mustCreate(t, "old1", CreateOptions{})
	p2 := mustCreate(t, "old2", CreateOptions{})
	for _, p := range []string{p1, p2} {
		if err := os.Remove(filepath.Join(p, ".env")); err != nil {
			t.Fatal(err)
		}
	}
	backfilled, err := BackfillProfileEnvs(true)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(backfilled)
	if !reflect.DeepEqual(backfilled, []string{"old1", "old2"}) {
		t.Fatalf("%v", backfilled)
	}
	for _, p := range []string{p1, p2} {
		env := filepath.Join(p, ".env")
		if strings.Contains(readText(t, env), "root-key") {
			t.Fatalf("%s copied the default key", env)
		}
		if st, err := os.Stat(env); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v %v", env, st, err)
		}
	}
}

func TestBackfillProfileEnvsPlaceholderWhenDefaultHasNoEnv(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	p := mustCreate(t, "noroot", CreateOptions{})
	if err := os.Remove(filepath.Join(p, ".env")); err != nil {
		t.Fatal(err)
	}
	backfilled, err := BackfillProfileEnvs(true)
	if err != nil || !reflect.DeepEqual(backfilled, []string{"noroot"}) {
		t.Fatalf("%v %v", backfilled, err)
	}
	commentOnlyEnv(t, filepath.Join(p, ".env"))
}

func TestBackfillProfileEnvsNoProfilesRootIsNoop(t *testing.T) {
	manageEnv(t)
	backfilled, err := BackfillProfileEnvs(true)
	if err != nil || len(backfilled) != 0 {
		t.Fatalf("%#v %v", backfilled, err)
	}
}

func TestProfilesToServeStandaloneAnswerIsPerHomeAndMemoInvalidatesOnReplacement(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	alpha := mustCreate(t, "alpha", CreateOptions{})
	beta := mustCreate(t, "beta", CreateOptions{})
	writeFile(t, filepath.Join(alpha, "config.yaml"), "gateway:\n  standalone: true\n")
	check := func(dir string, want bool) {
		t.Helper()
		if v, err := profiles.ProfileIsStandalone(context.Background(), dir); v != want || err != nil {
			t.Fatalf("%s %v %v want %v", dir, v, err, want)
		}
	}
	check(alpha, true)
	check(beta, false)
	check(alpha, true)
	replacement := filepath.Join(alpha, "replacement.yaml")
	writeFile(t, replacement, "gateway:\n  standalone: false\n")
	if err := os.Rename(replacement, filepath.Join(alpha, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	check(alpha, false)
	check(beta, false)
	check(alpha, false)
}

// Runtime trees the local models flow leaves in the root home
var runtimeTrees = []string{"models", "runtimes", "node"}

// Seeds the runtime trees plus a skill and a config in a home
func seedRuntimeTrees(t *testing.T, h string) {
	t.Helper()
	writeFile(t, filepath.Join(h, "models", "big.gguf"), string(make([]byte, 64)))
	writeFile(t, filepath.Join(h, "runtimes", "llamacpp", "bin", "llama-server"), "bin")
	writeFile(t, filepath.Join(h, "node", "bin", "node"), "bin")
	writeFile(t, filepath.Join(h, "skills", "greet", "SKILL.md"), "# greet\n")
	writeFile(t, filepath.Join(h, "config.yaml"), "model: test\n")
}

// Names of an ignore result as a sorted list
func ignoredNames(m map[string]bool) []string {
	out := []string{}
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestCloneAllExcludesRuntimeTreesIgnoreDropsRuntimeTreesOnlyAtTheDefaultRoot(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	seedRuntimeTrees(t, def)
	if err := os.Mkdir(filepath.Join(def, "skills", "greet", "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	ignore := cloneAllCopytreeIgnore(def)
	atRoot := ignore(def, []string{"models", "runtimes", "node", "skills", "config.yaml"})
	nested := ignore(filepath.Join(def, "skills", "greet"), []string{"models", "SKILL.md"})
	if got := ignoredNames(atRoot); !reflect.DeepEqual(got, []string{"models", "node", "runtimes"}) {
		t.Fatalf("root %v", got)
	}
	if got := ignoredNames(nested); len(got) != 0 {
		t.Fatalf("nested %v", got)
	}
	source := mustCreate(t, "source", CreateOptions{})
	for _, tree := range runtimeTrees {
		if err := os.Mkdir(filepath.Join(source, tree), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := ignoredNames(cloneAllCopytreeIgnore(source)(source, append(append([]string{}, runtimeTrees...), "SOUL.md"))); len(got) != 0 {
		t.Fatalf("named source %v", got)
	}
}

func TestCloneAllExcludesRuntimeTreesCloneAllFromDefaultSkipsRuntimeTreesButKeepsTheRest(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	seedRuntimeTrees(t, filepath.Join(tmp, ".icebath"))
	clone := mustCreate(t, "clone", CreateOptions{CloneAll: true})
	for _, name := range runtimeTrees {
		if pathExists(filepath.Join(clone, name)) {
			t.Fatalf("%s cloned", name)
		}
	}
	if st, err := os.Stat(filepath.Join(clone, "skills", "greet", "SKILL.md")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("skill %v", err)
	}
	if st, err := os.Stat(filepath.Join(clone, "config.yaml")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("config %v", err)
	}
}
